package selfupdate

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestMain silences slog for the whole package: Run and Check log on error /
// on a newer release, and the tests want deterministic, quiet output.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// newTestChecker builds a Checker pointed at a fresh httptest server running
// handler, with allowPrivate=true so the loopback address the server listens
// on passes the netguard egress guard.
func newTestChecker(handler http.HandlerFunc) (*Checker, *httptest.Server) {
	srv := httptest.NewServer(handler)
	c := NewChecker("owner/repo", 0, true)
	c.baseURL = srv.URL
	c.current = "v0.1.0"
	return c, srv
}

func redirectTo(status int, location string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", location)
		w.WriteHeader(status)
	}
}

func TestCheck_RedirectRelativeLocation(t *testing.T) {
	c, srv := newTestChecker(redirectTo(http.StatusFound, "/owner/repo/releases/tag/v0.2.0"))
	defer srv.Close()

	rel, err := c.Check(context.Background())
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	want := Release{Tag: "v0.2.0", URL: srv.URL + "/owner/repo/releases/tag/v0.2.0"}
	if rel != want {
		t.Errorf("Check() = %+v, want %+v", rel, want)
	}
}

func TestCheck_RedirectAbsoluteLocation(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", srv.URL+"/owner/repo/releases/tag/v0.2.0")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	c := NewChecker("owner/repo", 0, true)
	c.baseURL = srv.URL
	c.current = "v0.1.0"

	rel, err := c.Check(context.Background())
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	want := Release{Tag: "v0.2.0", URL: srv.URL + "/owner/repo/releases/tag/v0.2.0"}
	if rel != want {
		t.Errorf("Check() = %+v, want %+v", rel, want)
	}
}

func TestCheck_RedirectOwnerCaseInsensitive(t *testing.T) {
	c, srv := newTestChecker(redirectTo(http.StatusFound, "/Owner/Repo/releases/tag/v0.2.0"))
	defer srv.Close()

	rel, err := c.Check(context.Background())
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if rel.Tag != "v0.2.0" {
		t.Errorf("Check().Tag = %q, want %q", rel.Tag, "v0.2.0")
	}
}

func TestCheck_RedirectStatuses(t *testing.T) {
	for _, status := range []int{
		http.StatusMovedPermanently,  // 301
		http.StatusFound,             // 302
		http.StatusSeeOther,          // 303
		http.StatusTemporaryRedirect, // 307
		http.StatusPermanentRedirect, // 308
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c, srv := newTestChecker(redirectTo(status, "/owner/repo/releases/tag/v0.2.0"))
			defer srv.Close()

			rel, err := c.Check(context.Background())
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if rel.Tag != "v0.2.0" {
				t.Errorf("Check().Tag = %q, want %q", rel.Tag, "v0.2.0")
			}
		})
	}
}

func TestCheck_RequestHeaders(t *testing.T) {
	var gotPath, gotUA, gotAccept string
	c, srv := newTestChecker(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotUA = r.Header.Get("User-Agent")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Location", "/owner/repo/releases/tag/v0.2.0")
		w.WriteHeader(http.StatusFound)
	})
	defer srv.Close()
	c.current = "v9.9.9-test"

	if _, err := c.Check(context.Background()); err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if want := "/owner/repo/releases/latest"; gotPath != want {
		t.Errorf("request path = %q, want %q", gotPath, want)
	}
	if !strings.HasPrefix(gotUA, "krill/") {
		t.Errorf("User-Agent = %q, want prefix %q", gotUA, "krill/")
	}
	if !strings.Contains(gotUA, "v9.9.9-test") {
		t.Errorf("User-Agent = %q, want it to contain the injected current %q", gotUA, "v9.9.9-test")
	}
	if gotAccept != "text/html" {
		t.Errorf("Accept = %q, want %q", gotAccept, "text/html")
	}
}

func TestCheck_UnexpectedStatus200(t *testing.T) {
	c, srv := newTestChecker(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	defer srv.Close()

	_, err := c.Check(context.Background())
	if err == nil {
		t.Fatal("Check() error = nil, want an error for a 200 with no redirect")
	}
	if want := "unexpected status 200 from releases/latest"; err.Error() != want {
		t.Errorf("Check() error = %q, want %q", err.Error(), want)
	}
}

func TestCheck_PreservesLatestOnError(t *testing.T) {
	var failing atomic.Bool
	c, srv := newTestChecker(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Location", "/owner/repo/releases/tag/v0.2.0")
		w.WriteHeader(http.StatusFound)
	})
	defer srv.Close()

	rel, err := c.Check(context.Background())
	if err != nil {
		t.Fatalf("first Check() error = %v", err)
	}
	if rel.Tag != "v0.2.0" {
		t.Fatalf("first Check().Tag = %q, want %q", rel.Tag, "v0.2.0")
	}

	failing.Store(true)
	_, err = c.Check(context.Background())
	if err == nil {
		t.Fatal("second Check() error = nil, want an error")
	}

	st := c.State()
	if st.Err == nil {
		t.Error("State().Err = nil, want the second check's error")
	}
	if st.Latest.Tag != "v0.2.0" {
		t.Errorf("State().Latest.Tag = %q, want the first check's tag preserved (%q)", st.Latest.Tag, "v0.2.0")
	}
}

func TestCheck_404(t *testing.T) {
	c, srv := newTestChecker(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	defer srv.Close()

	_, err := c.Check(context.Background())
	if err == nil {
		t.Fatal("Check() error = nil, want an error for a 404")
	}
}

func TestCheck_NoRelease(t *testing.T) {
	c, srv := newTestChecker(redirectTo(http.StatusFound, "/owner/repo/releases"))
	defer srv.Close()

	_, err := c.Check(context.Background())
	if !errors.Is(err, ErrNoRelease) {
		t.Errorf("Check() error = %v, want ErrNoRelease", err)
	}
}

func TestCheck_InvalidTag(t *testing.T) {
	tests := []struct {
		name string
		tag  string
	}{
		{"prerelease suffix", "v0.2.0-rc1"},
		{"non-semver tag", "nightly"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, srv := newTestChecker(redirectTo(http.StatusFound, "/owner/repo/releases/tag/"+tt.tag))
			defer srv.Close()

			_, err := c.Check(context.Background())
			if err == nil {
				t.Fatalf("Check() error = nil, want an error for tag %q", tt.tag)
			}
		})
	}
}

func TestCheck_DifferentRepo(t *testing.T) {
	c, srv := newTestChecker(redirectTo(http.StatusFound, "/someone/else/releases/tag/v0.2.0"))
	defer srv.Close()

	_, err := c.Check(context.Background())
	if err == nil {
		t.Fatal("Check() error = nil, want an error for a redirect to a different repository")
	}
}

func TestValidTag(t *testing.T) {
	tests := []struct {
		name string
		tag  string
		want bool
	}{
		{"simple release tag", "v0.1.0", true},
		{"double-digit components", "v10.20.30", true},
		{"missing v prefix", "0.1.0", false},
		{"missing patch component", "v0.1", false},
		{"prerelease suffix", "v0.1.0-rc1", false},
		{"build metadata suffix", "v0.1.0+meta", false},
		{"empty string", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidTag(tt.tag); got != tt.want {
				t.Errorf("ValidTag(%q) = %v, want %v", tt.tag, got, tt.want)
			}
		})
	}
}

func TestNewer(t *testing.T) {
	tests := []struct {
		name            string
		current, latest string
		want            bool
	}{
		{"patch bump is newer", "v0.1.0", "v0.2.0", true},
		{"equal versions are not newer", "v0.2.0", "v0.2.0", false},
		{"current is ahead of latest", "v0.3.0", "v0.2.0", false},
		{"dev current never compares newer", "dev", "v0.2.0", false},
		{"dev+commit current never compares newer", "dev+abc123", "v0.2.0", false},
		{"a prerelease latest still outranks an older release by plain semver", "v0.1.0", "v0.2.0-rc1", true},
		{"empty latest is not newer", "v0.1.0", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Newer(tt.current, tt.latest); got != tt.want {
				t.Errorf("Newer(%q, %q) = %v, want %v", tt.current, tt.latest, got, tt.want)
			}
		})
	}
}

func TestCheckNow_Throttling(t *testing.T) {
	var requests atomic.Int32
	c, srv := newTestChecker(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Location", "/owner/repo/releases/tag/v0.2.0")
		w.WriteHeader(http.StatusFound)
	})
	defer srv.Close()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := base
	c.now = func() time.Time { return clock }

	rel, err := c.CheckNow(context.Background())
	if err != nil {
		t.Fatalf("first CheckNow() error = %v", err)
	}
	if rel.Tag != "v0.2.0" {
		t.Fatalf("first CheckNow().Tag = %q, want %q", rel.Tag, "v0.2.0")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests after first CheckNow() = %d, want 1", got)
	}

	clock = base.Add(30 * time.Second)
	rel2, err := c.CheckNow(context.Background())
	if !errors.Is(err, ErrThrottled) {
		t.Errorf("second CheckNow() error = %v, want ErrThrottled", err)
	}
	if rel2.Tag != "v0.2.0" {
		t.Errorf("second CheckNow() = %+v, want the cached release", rel2)
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("requests after throttled CheckNow() = %d, want still 1", got)
	}

	clock = base.Add(61 * time.Second)
	_, err = c.CheckNow(context.Background())
	if err != nil {
		t.Fatalf("third CheckNow() error = %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("requests after third CheckNow() = %d, want 2", got)
	}
}

func TestAvailable(t *testing.T) {
	t.Run("current older than cached", func(t *testing.T) {
		c := NewChecker("owner/repo", 0, true)
		c.current = "v0.1.0"
		c.state.Latest = Release{Tag: "v0.2.0", URL: "https://github.com/owner/repo/releases/tag/v0.2.0"}

		rel, ok := c.Available()
		if !ok {
			t.Fatal("Available() ok = false, want true")
		}
		if rel.Tag != "v0.2.0" {
			t.Errorf("Available().Tag = %q, want %q", rel.Tag, "v0.2.0")
		}
	})

	t.Run("dev current never has an update available", func(t *testing.T) {
		c := NewChecker("owner/repo", 0, true)
		c.current = "dev+abc"
		c.state.Latest = Release{Tag: "v0.2.0", URL: "https://github.com/owner/repo/releases/tag/v0.2.0"}

		_, ok := c.Available()
		if ok {
			t.Error("Available() ok = true, want false for a dev current version")
		}
	})

	t.Run("nothing cached yet", func(t *testing.T) {
		c := NewChecker("owner/repo", 0, true)
		c.current = "v0.1.0"

		rel, ok := c.Available()
		if ok {
			t.Error("Available() ok = true, want false when nothing has been cached yet")
		}
		if rel != (Release{}) {
			t.Errorf("Available() release = %+v, want zero value", rel)
		}
	})
}

func TestRun_ZeroIntervalReturnsImmediately(t *testing.T) {
	c := NewChecker("owner/repo", 0, true)
	// interval <= 0 means Run must return without blocking; calling it
	// synchronously and letting the test finish is the proof.
	c.Run(context.Background())
}

func TestRun_PeriodicChecks(t *testing.T) {
	var requests atomic.Int32
	c, srv := newTestChecker(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Location", "/owner/repo/releases/tag/v0.2.0")
		w.WriteHeader(http.StatusFound)
	})
	defer srv.Close()
	c.interval = 20 * time.Millisecond
	c.initialDelay = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for requests.Load() < 2 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("timed out waiting for Run() to make at least 2 requests")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run() did not return after its context was canceled")
	}
}
