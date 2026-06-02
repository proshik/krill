package server

import "net/http"

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
