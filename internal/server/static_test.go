package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/proshik/krill/internal/web"
)

func serveStatic(t *testing.T, target string, hdr http.Header) *httptest.ResponseRecorder {
	t.Helper()
	h, err := embeddedStatic()
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range hdr {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	http.StripPrefix("/static/", h).ServeHTTP(rec, req)
	return rec
}

// Each kind of static file gets its cache policy and a content ETag, so a
// reload revalidates (304) instead of downloading the fonts again.
func TestStaticCachePolicy(t *testing.T) {
	for _, c := range []struct {
		name, target, cache string
	}{
		{"versioned stylesheet", "/static/app.css?v=" + web.AssetVersion, "public, max-age=31536000, immutable"},
		{"versioned script", "/static/app.js?v=" + web.AssetVersion, "public, max-age=31536000, immutable"},
		{"stale version", "/static/app.css?v=0000000000", "no-cache"},
		{"unversioned stylesheet", "/static/app.css", "no-cache"},
		{"version on an uncovered file", "/static/htmx.min.js?v=" + web.AssetVersion, "no-cache"},
		{"font", "/static/fonts/onest-latin.woff2", "public, max-age=2592000"},
		{"script", "/static/htmx.min.js", "no-cache"},
	} {
		rec := serveStatic(t, c.target, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d, want 200", c.name, rec.Code)
			continue
		}
		if got := rec.Header().Get("Cache-Control"); got != c.cache {
			t.Errorf("%s: Cache-Control %q, want %q", c.name, got, c.cache)
		}
		if et := rec.Header().Get("ETag"); len(et) < 3 || et[0] != '"' || et[len(et)-1] != '"' {
			t.Errorf("%s: ETag %q, want a quoted strong tag", c.name, et)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s: empty body", c.name)
		}
	}
}

func TestStaticRevalidation(t *testing.T) {
	for _, target := range []string{"/static/htmx.min.js", "/static/fonts/onest-latin.woff2"} {
		etag := serveStatic(t, target, nil).Header().Get("ETag")
		rec := serveStatic(t, target, http.Header{"If-None-Match": {etag}})
		if rec.Code != http.StatusNotModified {
			t.Errorf("%s: If-None-Match %s gave %d, want 304", target, etag, rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("%s: 304 carries a body", target)
		}
		if rec := serveStatic(t, target, http.Header{"If-None-Match": {`"other"`}}); rec.Code != http.StatusOK {
			t.Errorf("%s: a different ETag gave %d, want 200", target, rec.Code)
		}
	}
}

func TestStaticMissingFile(t *testing.T) {
	rec := serveStatic(t, "/static/nope.js", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
	if rec.Header().Get("ETag") != "" || rec.Header().Get("Cache-Control") != "" {
		t.Errorf("404 carries cache headers: %v", rec.Header())
	}
}
