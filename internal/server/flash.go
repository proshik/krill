package server

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/proshik/krill/internal/web/flash"
	"github.com/proshik/krill/internal/web/i18n"
	"github.com/proshik/krill/internal/web/nav"
)

const flashPwCookie = "krill_flash_pw"

func (s *Server) setFlashPw(w http.ResponseWriter, r *http.Request, pw string) {
	http.SetCookie(w, &http.Cookie{
		Name:     flashPwCookie,
		Value:    pw,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cookieSecure(r),
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
func (s *Server) setFlash(w http.ResponseWriter, r *http.Request, kind, msg string) {
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookie,
		Value:    kind + ":" + url.QueryEscape(msg),
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cookieSecure(r),
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

// backURL returns a safe same-origin redirect target from the Referer, or "/".
func (s *Server) backURL(r *http.Request) string {
	ref := r.Referer()
	if ref == "" {
		return "/"
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "/"
	}
	if u.Host != "" && u.Host != r.Host {
		return "/" // cross-origin referer — don't honor it
	}
	if u.Path == "" {
		return "/"
	}
	back := u.Path
	if u.RawQuery != "" {
		back += "?" + u.RawQuery
	}
	return back
}

// flashErr sets an error flash and redirects back to the form (PRG), so the
// user stays in context and sees the message instead of a bare error page.
func (s *Server) flashErr(w http.ResponseWriter, r *http.Request, msg string) {
	s.setFlash(w, r, "err", msg)
	http.Redirect(w, r, s.backURL(r), http.StatusSeeOther)
}

// flashErrT sets a translated error flash and redirects back (PRG).
func (s *Server) flashErrT(w http.ResponseWriter, r *http.Request, key string) {
	s.flashErr(w, r, i18n.T(r.Context(), key))
}

// flashErrErr sets a translated error-prefix flash with the (English, technical)
// error detail appended, and redirects back.
func (s *Server) flashErrErr(w http.ResponseWriter, r *http.Request, key string, err error) {
	s.flashErr(w, r, i18n.T(r.Context(), key)+": "+err.Error())
}

// flashOK stores a translated success flash; the caller issues its own redirect.
func (s *Server) flashOK(w http.ResponseWriter, r *http.Request, key string) {
	s.setFlash(w, r, "ok", i18n.T(r.Context(), key))
}

// flashMiddleware records the request path (for sidebar highlighting) and
// extracts the flash cookie into the request context for full-page renders, so
// Layout can render the toast. It consumes the flash on normal full-page GETs
// and on hx-boost navigations (which also render the full Layout), but skips
// non-boosted HTMX requests (status polling, partial swaps) so they do not
// consume the flash early. Static requests are skipped entirely.
func (s *Server) flashMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(nav.WithPath(r.Context(), r.URL.Path))
		boosted := r.Header.Get("HX-Boosted") == "true"
		if r.Method == http.MethodGet &&
			(r.Header.Get("HX-Request") != "true" || boosted) &&
			!strings.HasPrefix(r.URL.Path, "/static/") {
			if k, m := s.takeFlash(w, r); k != "" {
				r = r.WithContext(flash.With(r.Context(), &flash.Flash{Kind: k, Msg: m}))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// desiredState reads the on/off state a toggle form asks for ("1" or "0" in
// field). Toggles post the state they want rather than "flip": a duplicate of
// a flip — a double click, a retried request — undoes the first. A form
// without it (a page from before this) is refused so the operator reloads.
func (s *Server) desiredState(w http.ResponseWriter, r *http.Request, field string) (bool, bool) {
	switch r.FormValue(field) {
	case "1":
		return true, true
	case "0":
		return false, true
	}
	s.flashErrT(w, r, "flash.err.stale_form")
	return false, false
}
