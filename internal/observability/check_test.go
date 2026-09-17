package observability

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func server(t *testing.T, h http.HandlerFunc) string {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s.URL
}

func checker() Checker { return Checker{AllowPrivate: true, Instance: "test", Timeout: 3 * time.Second} }

func TestCheckMetricsVerdicts(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		ok      bool
		key     string
		detail  string
	}{
		{"accepted", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Prometheus-Remote-Write-Samples-Written", "1")
			w.WriteHeader(http.StatusNoContent)
		}, true, "obs.check.ok", ""},
		{"2xx without v2 stats", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }, true, "obs.check.ok_unconfirmed", ""},
		{"v1 only", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "io.prometheus.write.v2.Request protobuf message is not accepted by this server", http.StatusUnsupportedMediaType)
		}, true, "obs.check.ok_v1_only", ""},
		{"unauthorized", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }, false, "obs.check.auth", "401"},
		{"forbidden", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) }, false, "obs.check.auth", "403"},
		{"not found", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, false, "obs.check.not_found_metrics", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			url := server(t, c.handler) + "/api/v1/push"
			rep := checker().Check(context.Background(), Settings{Metrics: Target{URL: url}})
			if rep.Logs != nil || rep.Metrics == nil {
				t.Fatalf("report = %+v", rep)
			}
			if got := *rep.Metrics; got.OK != c.ok || got.Key != c.key || got.Detail != c.detail {
				t.Errorf("result = %+v, want ok=%v key=%s detail=%q", got, c.ok, c.key, c.detail)
			}
		})
	}
}

func TestCheckMetricsRequest(t *testing.T) {
	var path, auth, ctype, ua atomic.Value
	url := server(t, func(w http.ResponseWriter, r *http.Request) {
		path.Store(r.URL.Path)
		auth.Store(r.Header.Get("Authorization"))
		ctype.Store(r.Header.Get("Content-Type"))
		ua.Store(r.Header.Get("User-Agent"))
		w.Header().Set("X-Prometheus-Remote-Write-Samples-Written", "1")
		w.WriteHeader(http.StatusNoContent)
	})
	rep := checker().Check(context.Background(), Settings{Metrics: Target{URL: url + "/custom/push", User: "u", Password: "p"}})
	if !rep.Metrics.OK {
		t.Fatalf("result = %+v", rep.Metrics)
	}
	if path.Load() != "/custom/push" {
		t.Errorf("path = %v: the checker must post to the exact URL, not append api/v1/write", path.Load())
	}
	if auth.Load() != "Basic dTpw" {
		t.Errorf("auth = %v", auth.Load())
	}
	if !strings.Contains(ctype.Load().(string), "io.prometheus.write.v2.Request") {
		t.Errorf("content type = %v", ctype.Load())
	}
	if !strings.HasPrefix(ua.Load().(string), "krill/") {
		t.Errorf("user agent = %v", ua.Load())
	}
}

func TestCheckServerErrorIsBoundedAndQuick(t *testing.T) {
	var hits atomic.Int32
	url := server(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, "<html><body><h1>Oops</h1>"+strings.Repeat("x", 5000)+"</body></html>")
	})
	start := time.Now()
	rep := checker().Check(context.Background(), Settings{Metrics: Target{URL: url}, Logs: Target{URL: url}})
	if time.Since(start) > 5*time.Second {
		t.Errorf("check took %s", time.Since(start))
	}
	if hits.Load() > 3 { // metrics: one retry; logs: one attempt
		t.Errorf("requests = %d", hits.Load())
	}
	for _, r := range []*Result{rep.Metrics, rep.Logs} {
		if r.OK || r.Key != "obs.check.http" {
			t.Errorf("result = %+v", r)
		}
		if len(r.Detail) > snippetLimit+len("…") || strings.Contains(r.Detail, "<") || !strings.Contains(r.Detail, "500") {
			t.Errorf("detail = %q", r.Detail)
		}
	}
}

func TestCheckLogs(t *testing.T) {
	var body, ctype atomic.Value
	url := server(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body.Store(string(b))
		ctype.Store(r.Header.Get("Content-Type"))
		w.WriteHeader(http.StatusNoContent)
	})
	rep := checker().Check(context.Background(), Settings{Logs: Target{URL: url + "/loki/api/v1/push"}})
	if rep.Metrics != nil || !rep.Logs.OK || rep.Logs.Key != "obs.check.ok" {
		t.Fatalf("report = %+v %+v", rep, rep.Logs)
	}
	if ctype.Load() != "application/json" || !strings.Contains(body.Load().(string), `"krill_check":"observability"`) {
		t.Errorf("request: %v %v", ctype.Load(), body.Load())
	}

	nf := server(t, http.NotFound)
	if r := checker().Check(context.Background(), Settings{Logs: Target{URL: nf}}).Logs; r.Key != "obs.check.not_found_logs" {
		t.Errorf("404 = %+v", r)
	}
	unsupported := server(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnsupportedMediaType) })
	if r := checker().Check(context.Background(), Settings{Logs: Target{URL: unsupported}}).Logs; r.OK || r.Key != "obs.check.http" {
		t.Errorf("415 on logs = %+v", r)
	}
}

func TestCheckPrivateAndNetwork(t *testing.T) {
	url := server(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	guarded := Checker{AllowPrivate: false, Timeout: 3 * time.Second}
	rep := guarded.Check(context.Background(), Settings{Metrics: Target{URL: url}, Logs: Target{URL: url}})
	for _, r := range []*Result{rep.Metrics, rep.Logs} {
		if r.OK || r.Key != "obs.check.private" || r.Detail != "" {
			t.Errorf("loopback with the guard on = %+v", r)
		}
	}

	// A server that is gone: grab its address, then close it.
	c := httptest.NewServer(http.NotFoundHandler())
	addr := c.URL
	c.Close()
	rep = checker().Check(context.Background(), Settings{Logs: Target{URL: addr}})
	if rep.Logs.OK || rep.Logs.Key != "obs.check.network" || rep.Logs.Detail == "" {
		t.Errorf("closed port = %+v", rep.Logs)
	}
}

func TestCheckRefusesRedirects(t *testing.T) {
	url := server(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "http://example.com/", http.StatusFound) })
	r := checker().Check(context.Background(), Settings{Logs: Target{URL: url}}).Logs
	if r.OK || r.Key != "obs.check.http" || !strings.Contains(r.Detail, "302") {
		t.Errorf("redirect = %+v", r)
	}
}

func TestSnippet(t *testing.T) {
	if got := snippet("  <p>bad\n\n gateway</p> "); got != "bad gateway" {
		t.Errorf("snippet = %q", got)
	}
	long := snippet(strings.Repeat("я", 400))
	if len(long) > snippetLimit+len("…") || !strings.HasSuffix(long, "…") || !strings.HasPrefix(long, "я") {
		t.Errorf("long snippet = %q (%d bytes)", long, len(long))
	}
}
