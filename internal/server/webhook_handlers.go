package server

import (
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/webhook"
)

const maxWebhookBody = 5 << 20 // 5 MiB

// webhookRetryAfter is the Retry-After (seconds) sent with a refused webhook
// deploy: long enough for a sibling deploy to make progress, short enough that a
// CI retry loop still lands the deploy within minutes.
const webhookRetryAfter = "30"

// refuseWebhookDeploy answers a webhook whose deploy could not be queued. The
// per-app in-flight guard, the per-organization cap and a full queue all refuse
// a deploy, and acknowledging one with 202 tells CI it shipped when nothing did.
// 503 with Retry-After is what a CI retry loop (curl --retry, for one) acts on.
func refuseWebhookDeploy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", webhookRetryAfter)
	http.Error(w, "deploy could not be queued, retry later", http.StatusServiceUnavailable)
}

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
	if s.deployer.Enqueue(a.ID, deploy.TriggerWebhook) == 0 {
		logFrom(r).Warn("github webhook deploy refused (already in flight, organization limit reached, or queue full)", "app_id", a.ID, "branch", a.GitBranch)
		refuseWebhookDeploy(w)
		return
	}
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
	// The token is accepted ONLY from the Authorization header. A ?token=
	// query parameter is deliberately never read: query strings are recorded
	// verbatim by reverse-proxy access logs and browser history, which would
	// leak the secret to places nobody treats as secret.
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
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
	tag := r.URL.Query().Get("tag")
	if tag != "" && !webhook.ValidTag(tag) {
		http.Error(w, "bad tag", http.StatusBadRequest)
		return
	}
	// The tag travels with the deploy job and reaches the application row only
	// once the job is accepted, so a refusal below leaves the app's tag alone
	// and a deploy already in flight cannot pick this one up.
	var deployID int64
	if tag != "" {
		deployID = s.deployer.EnqueueImage(a.ID, deploy.TriggerWebhook, a.Image, tag)
	} else {
		deployID = s.deployer.Enqueue(a.ID, deploy.TriggerWebhook)
	}
	if deployID == 0 {
		logFrom(r).Warn("deploy hook refused (already in flight, organization limit reached, or queue full)", "app_id", a.ID)
		refuseWebhookDeploy(w)
		return
	}
	logFrom(r).Info("deploy hook enqueued", "app_id", a.ID)
	w.WriteHeader(http.StatusAccepted)
}
