package server

import (
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/webhook"
)

const maxWebhookBody = 5 << 20 // 5 MiB

// webhookApp loads an app by the {appID} path param and confirms auto-deploy is
// on and the source type matches. It returns ok=false (and writes a generic 404)
// on any miss so the public endpoint never reveals whether an app exists.
func (s *Server) webhookApp(w http.ResponseWriter, r *http.Request, sourceType string) (db.Application, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "appID"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return db.Application{}, false
	}
	a, err := s.q.GetApplication(r.Context(), id)
	if err != nil || !a.AutoDeploy || a.SourceType != sourceType {
		http.NotFound(w, r)
		return db.Application{}, false
	}
	return a, true
}

// githubWebhook handles GitHub push webhooks for Dockerfile/git apps.
func (s *Server) githubWebhook(w http.ResponseWriter, r *http.Request) {
	a, ok := s.webhookApp(w, r, "dockerfile")
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	// An undecryptable secret can never match; log why, then fall through to the
	// same 404 as a bad signature (no capability disclosure either way).
	hookSecret, derr := secret.Dec(a.WebhookSecret)
	if derr != nil {
		logFrom(r).Error("github webhook: stored secret undecryptable", "err", derr, "app_id", a.ID)
	}
	if !webhook.VerifyHMAC(hookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		logFrom(r).Warn("github webhook signature mismatch", "app_id", a.ID)
		// 404 (not 401) so a bad signature is indistinguishable from an
		// unknown/disabled app: the earlier webhookApp checks already 404, and a
		// 401 here would leak which app IDs have auto-deploy enabled.
		http.NotFound(w, r)
		return
	}
	switch r.Header.Get("X-GitHub-Event") {
	case "ping":
		w.WriteHeader(http.StatusOK)
		return
	case "push":
		// handled below
	default:
		w.WriteHeader(http.StatusOK) // ack unknown events so GitHub keeps the hook healthy
		return
	}
	ev, err := webhook.ParsePushEvent(body)
	if err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if a.GitBranch == "" || ev.Deleted || ev.Branch() != a.GitBranch {
		logFrom(r).Info("github webhook skipped", "app_id", a.ID, "ref", ev.Ref, "want_branch", a.GitBranch)
		w.WriteHeader(http.StatusOK)
		return
	}
	s.deployer.Enqueue(a.ID, "webhook")
	logFrom(r).Info("github webhook deploy enqueued", "app_id", a.ID, "branch", a.GitBranch)
	w.WriteHeader(http.StatusAccepted)
}

// enableAutoDeploy turns on auto-deploy, generating a secret if none exists.
func (s *Server) enableAutoDeploy(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	if c.App.WebhookSecret == "" {
		if err := s.q.SetApplicationWebhookSecret(r.Context(), db.SetApplicationWebhookSecretParams{
			ID: c.App.ID, WebhookSecret: secret.Enc(webhook.NewSecret()),
		}); err != nil {
			logFrom(r).Error("enableAutoDeploy: set secret failed", "err", err, "app_id", c.App.ID)
			s.flashErrT(w, r, "flash.err.autodeploy")
			return
		}
	}
	if err := s.q.SetApplicationAutoDeploy(r.Context(), db.SetApplicationAutoDeployParams{ID: c.App.ID, AutoDeploy: true}); err != nil {
		logFrom(r).Error("enableAutoDeploy: enable failed", "err", err, "app_id", c.App.ID)
		s.flashErrT(w, r, "flash.err.autodeploy")
		return
	}
	logFrom(r).Info("auto-deploy enabled", "app_id", c.App.ID)
	s.flashOK(w, r, "flash.ok.autodeploy_enabled")
	http.Redirect(w, r, appURL(c)+"?tab=general", http.StatusSeeOther)
}

// disableAutoDeploy turns off the toggle; the secret is kept (the endpoint 404s
// while disabled, so an inert secret is harmless and avoids re-config on re-enable).
func (s *Server) disableAutoDeploy(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	if err := s.q.SetApplicationAutoDeploy(r.Context(), db.SetApplicationAutoDeployParams{ID: c.App.ID, AutoDeploy: false}); err != nil {
		logFrom(r).Error("disableAutoDeploy failed", "err", err, "app_id", c.App.ID)
		s.flashErrT(w, r, "flash.err.autodeploy")
		return
	}
	logFrom(r).Info("auto-deploy disabled", "app_id", c.App.ID)
	s.flashOK(w, r, "flash.ok.autodeploy_disabled")
	http.Redirect(w, r, appURL(c)+"?tab=general", http.StatusSeeOther)
}

// regenerateWebhookSecret rotates the secret (the user must update GitHub).
func (s *Server) regenerateWebhookSecret(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadAppCtx(w, r)
	if !ok {
		return
	}
	if err := s.q.SetApplicationWebhookSecret(r.Context(), db.SetApplicationWebhookSecretParams{
		ID: c.App.ID, WebhookSecret: secret.Enc(webhook.NewSecret()),
	}); err != nil {
		logFrom(r).Error("regenerateWebhookSecret failed", "err", err, "app_id", c.App.ID)
		s.flashErrT(w, r, "flash.err.autodeploy")
		return
	}
	logFrom(r).Info("webhook secret regenerated", "app_id", c.App.ID)
	s.flashOK(w, r, "flash.ok.autodeploy_regenerated")
	http.Redirect(w, r, appURL(c)+"?tab=general", http.StatusSeeOther)
}

// deployHook handles generic CI deploy hooks for image apps.
func (s *Server) deployHook(w http.ResponseWriter, r *http.Request) {
	a, ok := s.webhookApp(w, r, "image")
	if !ok {
		return
	}
	tok := r.URL.Query().Get("token")
	if tok == "" {
		tok = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}
	want, derr := secret.Dec(a.WebhookSecret)
	if derr != nil {
		logFrom(r).Error("deploy hook: stored secret undecryptable", "err", derr, "app_id", a.ID)
	}
	// An empty stored secret must never authenticate: ConstantTimeEqual("","")
	// is true, so guard it explicitly (defence in depth — enableAutoDeploy always
	// sets a secret, but a future path must not open a bypass).
	if want == "" || !webhook.ConstantTimeEqual(tok, want) {
		logFrom(r).Warn("deploy hook token mismatch", "app_id", a.ID)
		// 404 (not 401) so a bad token is indistinguishable from an
		// unknown/disabled app (see githubWebhook).
		http.NotFound(w, r)
		return
	}
	if tag := r.URL.Query().Get("tag"); tag != "" {
		if !webhook.ValidTag(tag) {
			http.Error(w, "bad tag", http.StatusBadRequest)
			return
		}
		if err := s.q.UpdateApplicationImage(r.Context(), db.UpdateApplicationImageParams{ID: a.ID, Image: a.Image, Tag: tag}); err != nil {
			logFrom(r).Error("deploy hook tag update failed", "err", err, "app_id", a.ID)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	s.deployer.Enqueue(a.ID, "webhook")
	logFrom(r).Info("deploy hook enqueued", "app_id", a.ID)
	w.WriteHeader(http.StatusAccepted)
}
