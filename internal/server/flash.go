package server

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/proshik/krill/internal/web/flash"
)

const flashPwCookie = "krill_flash_pw"

func (s *Server) setFlashPw(w http.ResponseWriter, pw string) {
	http.SetCookie(w, &http.Cookie{
		Name:     flashPwCookie,
		Value:    pw,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   120,
	})
}

func (s *Server) takeFlashPw(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie(flashPwCookie)
	if err != nil || c.Value == "" {
		return ""
	}
	http.SetCookie(w, &http.Cookie{Name: flashPwCookie, Path: "/", MaxAge: -1, HttpOnly: true})
	return c.Value
}

const flashCookie = "krill_flash"

// setFlash stores a one-shot notification in a short-lived cookie.
// kind is "ok" or "err". msg is plain text (URL-encoded in transit).
func (s *Server) setFlash(w http.ResponseWriter, kind, msg string) {
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookie,
		Value:    kind + ":" + url.QueryEscape(msg),
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   30,
	})
}

// takeFlash reads and clears the flash cookie. Returns ("","") if none.
func (s *Server) takeFlash(w http.ResponseWriter, r *http.Request) (kind, msg string) {
	c, err := r.Cookie(flashCookie)
	if err != nil || c.Value == "" {
		return "", ""
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Path: "/", MaxAge: -1, HttpOnly: true})
	k, raw, ok := strings.Cut(c.Value, ":")
	if !ok {
		return "", ""
	}
	m, derr := url.QueryUnescape(raw)
	if derr != nil {
		return "", ""
	}
	return k, m
}

// flashErr sets an error flash and redirects back to the form (PRG), so the
// user stays in context and sees the message instead of a bare error page.
func (s *Server) flashErr(w http.ResponseWriter, r *http.Request, msg string) {
	s.setFlash(w, "err", msg)
	back := r.Referer()
	if back == "" {
		back = "/"
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// flashMiddleware extracts the flash cookie into the request context for
// full-page GET responses, so Layout can render it. It skips HTMX and static
// requests so polling does not consume the flash early.
func (s *Server) flashMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet &&
			r.Header.Get("HX-Request") != "true" &&
			!strings.HasPrefix(r.URL.Path, "/static/") {
			if k, m := s.takeFlash(w, r); k != "" {
				r = r.WithContext(flash.With(r.Context(), &flash.Flash{Kind: k, Msg: m}))
			}
		}
		next.ServeHTTP(w, r)
	})
}
