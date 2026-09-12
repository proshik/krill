package server

import (
	"errors"
	"net/http"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/web/i18n"
	"github.com/proshik/krill/internal/web/templates"
)

func (s *Server) accountPasswordPage(w http.ResponseWriter, r *http.Request) {
	render(w, r, http.StatusOK, templates.AccountPassword("", s.mustChangePassword(r)))
}

func (s *Server) accountPasswordSubmit(w http.ResponseWriter, r *http.Request) {
	current, next, repeat := r.FormValue("current"), r.FormValue("next"), r.FormValue("repeat")
	forced := s.mustChangePassword(r)
	if next != repeat {
		render(w, r, http.StatusBadRequest, templates.AccountPassword(i18n.T(r.Context(), "account.password_mismatch"), forced))
		return
	}
	c, err := r.Cookie(auth.CookieName)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	err = s.auth.ChangePassword(r.Context(), auth.UserID(r.Context()), current, next, c.Value)
	switch {
	case errors.Is(err, auth.ErrWrongPassword):
		render(w, r, http.StatusBadRequest, templates.AccountPassword(i18n.T(r.Context(), "account.password_wrong"), forced))
		return
	case errors.Is(err, auth.ErrWeakPassword):
		render(w, r, http.StatusBadRequest, templates.AccountPassword(i18n.T(r.Context(), "account.password_weak"), forced))
		return
	case err != nil:
		logFrom(r).Error("change password failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	logFrom(r).Info("password changed")
	s.flashOK(w, r, "flash.ok.password_changed")
	http.Redirect(w, r, "/", http.StatusSeeOther)
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
