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
// change. A read failure is treated as "no" for rendering only — the middleware
// is what actually gates access.
func (s *Server) mustChangePassword(r *http.Request) bool {
	must, err := s.q.GetUserMustChangePassword(r.Context(), auth.UserID(r.Context()))
	if err != nil {
		logFrom(r).Error("must_change_password lookup failed", "err", err)
		return false
	}
	return must
}

// requirePasswordChange keeps a user who was handed a password by someone else
// on the change-password form until they pick their own. The inviter knows the
// temporary password, so until it is changed the invitee's account is shared.
func (s *Server) requirePasswordChange(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/account/password" || r.URL.Path == "/logout" {
			next.ServeHTTP(w, r)
			return
		}
		if s.mustChangePassword(r) {
			http.Redirect(w, r, "/account/password", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}
