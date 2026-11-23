package server

import (
	"net/http"

	"github.com/proshik/krill/internal/web"
)

// AssetHeader carries the asset version of the page an htmx request comes
// from: every page sets it on <body> (hx-headers), and a boosted navigation
// keeps the body's attributes, so it is the version of the stylesheet and
// script the tab actually runs.
const AssetHeader = "X-Krill-Assets"

// assetGate makes a tab left open across an upgrade load the new release's
// assets. htmx swaps a boosted response's body and drops its head, so such a
// tab would render new markup with the old script (and an old tab sends no
// version at all). A boosted page request from it is answered with
// HX-Redirect, which htmx follows as a full page load of the same address; a
// polled fragment is answered with HX-Refresh, reloading the page it polls
// from. Only GETs: a form an old page posts belongs to that page, and the
// page its redirect leads to is a GET this gate then catches.
func assetGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("HX-Request") != "true" || r.Header.Get(AssetHeader) == web.AssetVersion {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("HX-Boosted") == "true" {
			w.Header().Set("HX-Redirect", r.URL.RequestURI())
		} else {
			w.Header().Set("HX-Refresh", "true")
		}
		w.WriteHeader(http.StatusOK)
	})
}
