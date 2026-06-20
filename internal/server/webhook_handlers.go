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
	if !webhook.VerifyHMAC(secret.Dec(a.WebhookSecret), body, r.Header.Get("X-Hub-Signature-256")) {
		logFrom(r).Warn("github webhook signature mismatch", "app_id", a.ID)
		http.Error(w, "bad signature", http.StatusUnauthorized)
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
	if ev.Deleted || ev.Branch() != a.GitBranch {
		logFrom(r).Info("github webhook skipped", "app_id", a.ID, "ref", ev.Ref, "want_branch", a.GitBranch)
		w.WriteHeader(http.StatusOK)
		return
	}
	s.deployer.Enqueue(a.ID, "webhook")
	logFrom(r).Info("github webhook deploy enqueued", "app_id", a.ID, "branch", a.GitBranch)
	w.WriteHeader(http.StatusAccepted)
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
	if !webhook.ConstantTimeEqual(tok, secret.Dec(a.WebhookSecret)) {
		logFrom(r).Warn("deploy hook token mismatch", "app_id", a.ID)
		http.Error(w, "bad token", http.StatusUnauthorized)
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
