package server

import (
	"net/http"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/web/templates"
)

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	render(w, r, http.StatusOK, templates.Login(""))
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	email := r.FormValue("email")
	password := r.FormValue("password")
	token, err := s.auth.Authenticate(r.Context(), email, password)
	if err != nil {
		render(w, r, http.StatusUnauthorized, templates.Login("Неверный email или пароль"))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     auth.CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(auth.SessionTTL.Seconds()),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.CookieName); err == nil {
		_ = s.auth.Logout(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: auth.CookieName, Path: "/", MaxAge: -1, HttpOnly: true})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
