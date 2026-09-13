package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/proshik/krill/internal/api"
	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/web/templates"
)

// flashTokenCookie carries a freshly minted personal-access-token plaintext to
// the very next page render, exactly once — the same one-shot HttpOnly-cookie
// mechanism as flashPwCookie (internal/server/flash.go) for temporary
// passwords, but under its own name so a token and a generated password can
// never land in the same flash slot and collide.
const flashTokenCookie = "krill_flash_token"

func (s *Server) setFlashToken(w http.ResponseWriter, r *http.Request, plain string) {
	http.SetCookie(w, &http.Cookie{
		Name:     flashTokenCookie,
		Value:    plain,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   120,
	})
}

// takeFlashToken reads and immediately clears the flash-token cookie, so a
// reload of the same page never shows the plaintext a second time. The
// plaintext itself is never stored anywhere server-side — only this
// short-lived client cookie carries it, once.
func (s *Server) takeFlashToken(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie(flashTokenCookie)
	if err != nil || c.Value == "" {
		return ""
	}
	http.SetCookie(w, &http.Cookie{Name: flashTokenCookie, Path: "/", MaxAge: -1, HttpOnly: true})
	return c.Value
}

// apiTokenExpiry converts the create-form's "expires" preset into a nullable
// expiry timestamp. ok is false for anything other than the three options the
// form actually offers, so a tampered/unknown value is rejected rather than
// silently guessed at.
func apiTokenExpiry(preset string) (ts pgtype.Timestamptz, ok bool) {
	switch preset {
	case "never":
		return pgtype.Timestamptz{}, true
	case "30d":
		return pgtype.Timestamptz{Time: time.Now().Add(30 * 24 * time.Hour), Valid: true}, true
	case "90d":
		return pgtype.Timestamptz{Time: time.Now().Add(90 * 24 * time.Hour), Valid: true}, true
	default:
		return pgtype.Timestamptz{}, false
	}
}

// listAPITokens renders the signed-in user's personal access tokens scoped to
// the org in the URL. A token is genuinely org-bound — its Identity.OrgID
// (and therefore what it can act on) comes straight from the stored org_id —
// so the list must obey the org the operator is looking at: every other
// Settings page (registries, destinations, git credentials, notification
// channels) is strictly org-scoped, and this one is not an exception. Readable
// by any org member; only minting a new token is admin-gated.
func (s *Server) listAPITokens(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	uid := auth.UserID(r.Context())
	tokens, err := s.q.ListAPITokensByUserAndOrg(r.Context(), db.ListAPITokensByUserAndOrgParams{UserID: uid, OrgID: o.ID})
	if err != nil {
		logFrom(r).Error("listAPITokens: failed to list tokens", "err", err, "user_id", uid, "org_id", o.ID)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// One-shot reveal: read-and-clear, so this plaintext is rendered at most once.
	plain := s.takeFlashToken(w, r)
	render(w, r, http.StatusOK, templates.APITokens(o, role, tokens, plain))
}

// createAPIToken mints a new personal access token for the signed-in user
// (admin-only). The plaintext is handed to the next page render through a
// one-shot flash cookie and is never persisted or logged — only its SHA-256
// hash and 8-character lookup prefix are stored (internal/api.GenerateToken),
// and only the new row's id/prefix are logged, never the token itself.
func (s *Server) createAPIToken(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	uid := auth.UserID(r.Context())
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		s.flashErrT(w, r, "flash.err.api_token_name_required")
		return
	}
	level := api.Level(r.FormValue("level"))
	if level != api.LevelRead && level != api.LevelWrite {
		s.flashErrT(w, r, "flash.err.api_token_invalid_level")
		return
	}
	expiresAt, ok := apiTokenExpiry(r.FormValue("expires"))
	if !ok {
		s.flashErrT(w, r, "flash.err.api_token_invalid_expires")
		return
	}

	plain, prefix, hash, err := api.GenerateToken()
	if err != nil {
		logFrom(r).Error("createAPIToken: failed to generate token", "err", err, "user_id", uid, "org_id", o.ID)
		s.flashErr(w, r, err.Error())
		return
	}

	tok, err := s.q.CreateAPIToken(r.Context(), db.CreateAPITokenParams{
		UserID:    uid,
		OrgID:     o.ID,
		Name:      name,
		TokenHash: hash,
		Prefix:    prefix,
		Level:     string(level),
		ExpiresAt: expiresAt,
	})
	if err != nil {
		logFrom(r).Error("createAPIToken: failed to create token", "err", err, "user_id", uid, "org_id", o.ID)
		s.flashErrErr(w, r, "flash.err.create_api_token", err)
		return
	}

	s.setFlashToken(w, r, plain)
	logFrom(r).Info("api token created", "org_id", o.ID, "user_id", uid, "token_id", tok.ID, "prefix", tok.Prefix, "level", tok.Level)
	s.flashOK(w, r, "flash.api_token_created")
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/api-tokens", http.StatusSeeOther)
}

// deleteAPIToken revokes one of the signed-in user's own tokens. Ownership is
// checked explicitly (404 on mismatch, same convention as every other
// tenant-scoped resource) before the delete, which is itself scoped by
// user_id in the query — belt and suspenders against ever touching another
// user's token. No role gate beyond org membership: revoking your own leaked
// token must not require regaining admin first.
func (s *Server) deleteAPIToken(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r, "tokenID")
	if !ok {
		http.NotFound(w, r)
		return
	}
	uid := auth.UserID(r.Context())
	tok, err := s.q.GetAPIToken(r.Context(), id)
	// The org in the path is part of the identity of the thing being revoked:
	// the list this page renders is scoped to it, so a token belonging to a
	// different org is not on this page and must not be revocable through it —
	// even though it is the caller's own. Otherwise this handler is the one
	// place on the page that ignores the org the URL names.
	if err != nil || tok.UserID != uid || tok.OrgID != o.ID {
		logFrom(r).Info("deleteAPIToken: token not found, not owned by caller, or not in this org", "token_id", id, "user_id", uid, "org_id", o.ID)
		http.NotFound(w, r)
		return
	}
	if err := s.q.DeleteAPIToken(r.Context(), db.DeleteAPITokenParams{ID: id, UserID: uid}); err != nil {
		logFrom(r).Error("deleteAPIToken: failed to delete token", "err", err, "token_id", id, "user_id", uid)
		s.flashErr(w, r, err.Error())
		return
	}
	logFrom(r).Info("api token revoked", "org_id", o.ID, "user_id", uid, "token_id", id, "prefix", tok.Prefix)
	s.flashOK(w, r, "flash.api_token_revoked")
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/api-tokens", http.StatusSeeOther)
}
