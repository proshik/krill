package server

import (
	"net/http"

	"github.com/proshik/krill/internal/web/i18n"
)

// setLanguage stores the chosen UI language in the krill_lang cookie and
// redirects back. Read by the locale middleware on every request.
func (s *Server) setLanguage(w http.ResponseWriter, r *http.Request) {
	lang := r.FormValue("lang")
	if !i18n.Supported(lang) {
		lang = i18n.DefaultLocale
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "krill_lang",
		Value:    lang,
		Path:     "/",
		MaxAge:   365 * 24 * 3600,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, s.backURL(r), http.StatusSeeOther)
}
