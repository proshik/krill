package server

import (
	"errors"
	"net/http"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/web/i18n"
	"github.com/proshik/krill/internal/web/templates"
)

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	render(w, r, http.StatusOK, templates.Login("", s.loginCookieWarning(r)))
}

// loginCookieWarning reports whether signing in from r cannot work: the session
// cookie would be Secure, yet r arrived over plain HTTP — straight at the UI
// port with KRILL_COOKIE_SECURE=true set before HTTPS was in place.
func (s *Server) loginCookieWarning(r *http.Request) bool {
	return s.cookieSecure(r) && r.TLS == nil && !s.secureViaGateway(r) &&
		!(s.cfg.TrustProxy && r.Header.Get("X-Forwarded-Proto") == "https")
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	email := r.FormValue("email")
	password := r.FormValue("password")
	token, err := s.auth.Authenticate(r.Context(), email, password)
	if err != nil {
		// Telling a user their password is wrong when the database is down
		// sends them off resetting credentials that were never the problem.
		if !errors.Is(err, auth.ErrInvalidCredentials) {
			logFrom(r).Error("login: authentication backend failed", "err", err, "email", email)
			render(w, r, http.StatusInternalServerError, templates.Login(i18n.T(r.Context(), "login.backend_error"), s.loginCookieWarning(r)))
			return
		}
		logFrom(r).Warn("login failed", "email", email, "remote", r.RemoteAddr)
		render(w, r, http.StatusUnauthorized, templates.Login("Invalid email or password", s.loginCookieWarning(r)))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     auth.CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(auth.SessionTTL.Seconds()),
	})
	logFrom(r).Info("login succeeded", "email", email)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.CookieName); err == nil {
		_ = s.auth.Logout(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: auth.CookieName, Path: "/", MaxAge: -1, HttpOnly: true})
	logFrom(r).Info("logout")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
