package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/web/i18n"
	"github.com/proshik/krill/internal/web/templates"
)

// accountPasswordPage serves the standalone change-password form. A user who
// still owes a password change is held here, so such a user is never sent
// anywhere else — nor is one whose flag cannot be read, because the gate treats
// that as "must change" and redirects here, and bouncing them back out would
// loop. Everyone else is sent to the same form inside the normal layout of their
// first organization, where they can navigate away.
func (s *Server) accountPasswordPage(w http.ResponseWriter, r *http.Request) {
	must, err := s.mustChangePasswordLookup(r.Context(), auth.UserID(r.Context()))
	if err != nil {
		logFrom(r).Error("must_change_password lookup failed", "err", err)
		render(w, r, http.StatusOK, templates.AccountPassword("", false))
		return
	}
	if !must {
		orgs, lerr := s.q.ListOrganizationsForUser(r.Context(), auth.UserID(r.Context()))
		if lerr != nil {
			logFrom(r).Error("account password: failed to list organizations", "err", lerr)
		} else if len(orgs) > 0 {
			http.Redirect(w, r, orgAccountPasswordPath(orgs[0].ID), http.StatusSeeOther)
			return
		}
	}
	render(w, r, http.StatusOK, templates.AccountPassword("", must))
}

func (s *Server) accountPasswordSubmit(w http.ResponseWriter, r *http.Request) {
	forced := s.mustChangePassword(r)
	msg, handled := s.changePassword(w, r)
	if handled {
		return
	}
	if msg != "" {
		render(w, r, http.StatusBadRequest, templates.AccountPassword(msg, forced))
		return
	}
	s.flashOK(w, r, "flash.ok.password_changed")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// orgAccountPasswordPage is the change-password form inside the normal layout,
// linked from the sidebar of every organization page.
func (s *Server) orgAccountPasswordPage(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	render(w, r, http.StatusOK, templates.AccountPasswordPage(o, role, ""))
}

func (s *Server) orgAccountPasswordSubmit(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	msg, handled := s.changePassword(w, r)
	if handled {
		return
	}
	if msg != "" {
		render(w, r, http.StatusBadRequest, templates.AccountPasswordPage(o, role, msg))
		return
	}
	s.flashOK(w, r, "flash.ok.password_changed")
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10), http.StatusSeeOther)
}

func orgAccountPasswordPath(orgID int64) string {
	return "/orgs/" + strconv.FormatInt(orgID, 10) + "/account/password"
}

// changePassword applies a submitted change-password form for the signed-in
// user. It returns a message to show on the form when the input is refused, or
// handled=true when it has already written the response (no session cookie, or
// an internal error). An empty message with handled=false means the password
// was changed.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) (msg string, handled bool) {
	current, next, repeat := r.FormValue("current"), r.FormValue("next"), r.FormValue("repeat")
	if next != repeat {
		return i18n.T(r.Context(), "account.password_mismatch"), false
	}
	c, err := r.Cookie(auth.CookieName)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return "", true
	}
	err = s.auth.ChangePassword(r.Context(), auth.UserID(r.Context()), current, next, c.Value)
	switch {
	case errors.Is(err, auth.ErrWrongPassword):
		return i18n.T(r.Context(), "account.password_wrong"), false
	case errors.Is(err, auth.ErrWeakPassword):
		return i18n.T(r.Context(), "account.password_weak"), false
	case err != nil:
		logFrom(r).Error("change password failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return "", true
	}
	logFrom(r).Info("password changed")
	return "", false
}

// mustChangePassword reports whether the signed-in user still owes a password
// change, for RENDERING only (e.g. whether accountPasswordPage shows the
// forced-change banner). A lookup failure is treated as "no" here — showing
// the wrong banner is harmless. This is NOT the access gate: requirePasswordChange
// below does its own lookup and fails CLOSED on the same kind of error, because
// silently letting a flagged user through defeats the whole feature.
func (s *Server) mustChangePassword(r *http.Request) bool {
	must, err := s.mustChangePasswordLookup(r.Context(), auth.UserID(r.Context()))
	if err != nil {
		logFrom(r).Error("must_change_password lookup failed", "err", err)
		return false
	}
	return must
}

// requirePasswordChange keeps a user who was handed a password by someone else
// on the change-password form until they pick their own. The inviter knows the
// temporary password, so until it is changed the invitee's account is shared.
// It fails CLOSED: a lookup error holds the user on the form exactly like a
// genuine "must change" result, rather than letting a transient DB blip open
// the same hole this middleware exists to close. /account/password itself
// stays exempt, so a fail-closed redirect can never loop.
func (s *Server) requirePasswordChange(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/account/password" {
			next.ServeHTTP(w, r)
			return
		}
		must, err := s.mustChangePasswordLookup(r.Context(), auth.UserID(r.Context()))
		if err != nil {
			logFrom(r).Error("must_change_password lookup failed; holding the user on the change-password form", "err", err)
			http.Redirect(w, r, "/account/password", http.StatusSeeOther)
			return
		}
		if must {
			http.Redirect(w, r, "/account/password", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}
