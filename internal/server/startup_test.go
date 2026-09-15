package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// serveGate sends one request through the gate and returns the recorded response.
func serveGate(g *StartupGate, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, r)
	return rec
}

// Until the real router is swapped in, every request gets the starting page:
// a 503 the browser retries on its own, never cached, naming the current phase.
func TestStartupGateServesStartingPage(t *testing.T) {
	cases := []struct {
		name  string
		set   bool
		phase StartupPhase
		want  string
	}{
		{name: "zero value", want: "Applying database migrations…"},
		{name: "migrating", set: true, phase: PhaseMigrating, want: "Applying database migrations…"},
		{name: "starting", set: true, phase: PhaseStarting, want: "Starting services…"},
		{name: "gateway", set: true, phase: PhaseGateway, want: "Preparing the gateway…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := NewStartupGate()
			if tc.set {
				g.SetPhase(tc.phase)
			}
			rec := serveGate(g, httptest.NewRequest(http.MethodGet, "/", nil))

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", rec.Code)
			}
			if got := rec.Header().Get("Retry-After"); got != "3" {
				t.Errorf("Retry-After = %q, want %q", got, "3")
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want %q", got, "no-store")
			}
			if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
				t.Errorf("Content-Type = %q, want text/html; charset=utf-8", got)
			}
			if _, ok := rec.Header()[http.CanonicalHeaderKey("HX-Refresh")]; ok {
				t.Errorf("HX-Refresh set on a request that did not come from htmx")
			}
			body := rec.Body.String()
			for _, s := range []string{
				tc.want,
				"<title>Krill is starting</title>",
				"<h1>Krill is starting</h1>",
				"This page refreshes by itself.",
				`http-equiv="refresh"`,
			} {
				if !strings.Contains(body, s) {
					t.Errorf("body does not contain %q:\n%s", s, body)
				}
			}
		})
	}
}

// The starting page speaks the language the real router would have picked: the
// krill_lang cookie first, then Accept-Language, then the default.
func TestStartupGateLocale(t *testing.T) {
	cases := []struct {
		name   string
		cookie string
		accept string
		want   string
	}{
		{name: "default", want: "Krill is starting"},
		{name: "cookie", cookie: "ru", want: "Krill запускается"},
		{name: "accept-language", accept: "ru-RU,ru;q=0.9", want: "Krill запускается"},
		{name: "cookie wins over accept-language", cookie: "en", accept: "ru", want: "Krill is starting"},
		{name: "unsupported cookie falls back to accept-language", cookie: "xx", accept: "ru", want: "Krill запускается"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: "krill_lang", Value: tc.cookie})
			}
			if tc.accept != "" {
				r.Header.Set("Accept-Language", tc.accept)
			}
			rec := serveGate(NewStartupGate(), r)
			if body := rec.Body.String(); !strings.Contains(body, tc.want) {
				t.Errorf("body does not contain %q:\n%s", tc.want, body)
			}
		})
	}
}

// An htmx request (a polling fragment, a boosted click) would otherwise swap the
// starting page into part of the old page; HX-Refresh turns it into a full load.
func TestStartupGateHXRefresh(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/orgs/1/updates/status", nil)
	r.Header.Set("HX-Request", "true")
	rec := serveGate(NewStartupGate(), r)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("HX-Refresh"); got != "true" {
		t.Errorf("HX-Refresh = %q, want %q", got, "true")
	}
}

// Every method and path is held back, not just page loads: webhooks and the
// gateway's provider poll get a 503 rather than reaching a half-wired server.
func TestStartupGateHoldsEveryMethod(t *testing.T) {
	g := NewStartupGate()
	g.SetPhase(PhaseGateway)
	rec := serveGate(g, httptest.NewRequest(http.MethodPost, "/webhooks/github/1", strings.NewReader("{}")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "3" {
		t.Errorf("Retry-After = %q, want %q", got, "3")
	}
}

// Once swapped, the gate is transparent: the real handler's response comes back
// untouched, with none of the starting page's headers.
func TestStartupGateDelegatesAfterSwap(t *testing.T) {
	g := NewStartupGate()
	g.Swap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "real "+r.Method)
	}))

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		r := httptest.NewRequest(method, "/webhooks/github/1", nil)
		r.Header.Set("HX-Request", "true")
		rec := serveGate(g, r)
		if rec.Code != http.StatusTeapot {
			t.Errorf("%s: status = %d, want %d", method, rec.Code, http.StatusTeapot)
		}
		if got, want := rec.Body.String(), "real "+method; got != want {
			t.Errorf("%s: body = %q, want %q", method, got, want)
		}
		if got := rec.Header().Get("Content-Type"); got != "text/plain" {
			t.Errorf("%s: Content-Type = %q, want text/plain", method, got)
		}
		for _, h := range []string{"Retry-After", "Cache-Control", "HX-Refresh"} {
			if _, ok := rec.Header()[http.CanonicalHeaderKey(h)]; ok {
				t.Errorf("%s: gate added %s after the swap", method, h)
			}
		}
	}
}

// The swap happens while requests are already being served on the listener, so
// it must be safe against concurrent ServeHTTP calls (run with -race).
func TestStartupGateSwapIsConcurrencySafe(t *testing.T) {
	g := NewStartupGate()
	stub := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	const n = 50
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		if i == n/2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				g.SetPhase(PhaseStarting)
				g.Swap(stub)
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- serveGate(g, httptest.NewRequest(http.MethodGet, "/", nil)).Code
		}()
	}
	wg.Wait()
	close(codes)

	for code := range codes {
		if code != http.StatusServiceUnavailable && code != http.StatusOK {
			t.Errorf("status = %d, want 503 or 200", code)
		}
	}
	if code := serveGate(g, httptest.NewRequest(http.MethodGet, "/", nil)).Code; code != http.StatusOK {
		t.Errorf("after the swap: status = %d, want 200", code)
	}
}
