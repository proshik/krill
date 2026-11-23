package server_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/web"
)

// A tab left open across an upgrade runs the previous release's script; its
// next boosted navigation must load the destination as a full page, and its
// polls must reload the page they come from. A current tab is served as is.
func TestAssetGateReloadsTabsFromAnotherRelease(t *testing.T) {
	h, q, _ := newServer(t)
	mkUser(t, q, "gate@k.local")
	cookie := loginAs(t, q, "gate@k.local")
	get := func(headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/orgs?x=1", nil)
		req.AddCookie(cookie)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for name, hdr := range map[string]map[string]string{
		"old release":   {"HX-Request": "true", "HX-Boosted": "true", server.AssetHeader: "0000000000"},
		"before stamps": {"HX-Request": "true", "HX-Boosted": "true"},
	} {
		rec := get(hdr)
		if rec.Header().Get("HX-Redirect") != "/orgs?x=1" || rec.Body.Len() != 0 {
			t.Errorf("%s: a boosted navigation must become a full load, got HX-Redirect=%q body=%d", name, rec.Header().Get("HX-Redirect"), rec.Body.Len())
		}
	}
	if rec := get(map[string]string{"HX-Request": "true", server.AssetHeader: "0000000000"}); rec.Header().Get("HX-Refresh") != "true" {
		t.Error("a poll from an old page must reload that page")
	}
	rec := get(map[string]string{"HX-Request": "true", "HX-Boosted": "true", server.AssetHeader: web.AssetVersion})
	if rec.Header().Get("HX-Redirect") != "" || rec.Header().Get("HX-Refresh") != "" || !strings.Contains(rec.Body.String(), "<main") {
		t.Error("a current tab's request must be served normally")
	}
	if rec := get(nil); rec.Header().Get("HX-Redirect") != "" || !strings.Contains(rec.Body.String(), `"X-Krill-Assets":"`) && !strings.Contains(rec.Body.String(), `X-Krill-Assets&#34;:&#34;`+web.AssetVersion) {
		t.Error("a full page must carry its asset version for htmx to send")
	}
}
