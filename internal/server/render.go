package server

import (
	"net/http"

	"github.com/a-h/templ"
)

// render renders a templ component as HTML.
func render(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = c.Render(r.Context(), w)
}
