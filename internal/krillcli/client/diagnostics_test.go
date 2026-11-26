package client_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/proshik/krill/internal/krillcli/client"
)

// The two answers an older Krill gives for an operation it does not have,
// taken from its router and its app-reference parser (internal/server
// SplitAppRef and internal/api resolveApp at the release before the agent
// API diagnostics).
const (
	routerNotFound = "404 page not found\n"
)

func oldRefRefusal(ref string) string {
	return `{"code":"invalid","message":"app \"` + ref + `\" is neither a numeric id nor a project/environment/app path","request_id":"r1"}`
}

func TestWaitLogSendsTheWaitAndDecodesTheResult(t *testing.T) {
	var q url.Values
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.Query()
		_, _ = w.Write([]byte(`{"found":true,"stopped_by":"match","match":{"t":"2026-11-26T10:00:00Z","lvl":"info","msg":"listening on :8080"},
			"lines":[{"t":"2026-11-26T10:00:00Z","lvl":"info","msg":"listening on :8080"}],"following_since":"2026-11-26T09:59:58Z"}`))
	})
	res, err := c.WaitLog(context.Background(), "acme/production/bot", "Listening", 5, 90*time.Second)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if q.Get("wait_for") != "Listening" || q.Get("timeout") != "90" || q.Get("tail") != "5" {
		t.Fatalf("query = %v", q)
	}
	if !res.Found || res.Match == nil || res.Match.Message != "listening on :8080" || res.FollowingSince == "" {
		t.Fatalf("result = %+v", res)
	}
}

// TestWaitLogOnAnOldServer: an older Krill ignores wait_for and answers with
// the plain tail at once. Decoding that as "not found" would be a lie.
func TestWaitLogOnAnOldServer(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"t":"2026-11-26T10:00:00Z","lvl":"info","msg":"hello"}]`))
	})
	_, err := c.WaitLog(context.Background(), "17", "listening", 0, 0)
	var old *client.OldServerError
	if !errors.As(err, &old) {
		t.Fatalf("want OldServerError, got %v", err)
	}
}

// TestLogWindowOnAnOldServerIsRefusedBeforeReading: since/until/contains are
// silently ignored by an older Krill, which would answer with the plain tail
// — a plausible, wrong result. The client asks first.
func TestLogWindowOnAnOldServerIsRefusedBeforeReading(t *testing.T) {
	logsRead := false
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/whoami" {
			_, _ = w.Write([]byte(`{"org_name":"acme","level":"read","role":"member"}`))
			return
		}
		logsRead = true
		_, _ = w.Write([]byte(`[]`))
	})
	_, err := c.Logs(context.Background(), "17", client.LogsQuery{Contains: "boom"})
	var old *client.OldServerError
	if !errors.As(err, &old) {
		t.Fatalf("want OldServerError, got %v", err)
	}
	if logsRead {
		t.Fatal("the log was read from a server that would have ignored the filter")
	}
}

func TestLogWindowOnACurrentServer(t *testing.T) {
	var q url.Values
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/whoami" {
			_, _ = w.Write([]byte(`{"org_name":"acme","instance":{"url":"https://krill.example.com","version":"v0.14.0"}}`))
			return
		}
		q = r.URL.Query()
		_, _ = w.Write([]byte(`[]`))
	})
	if _, err := c.Logs(context.Background(), "17", client.LogsQuery{Since: "10m", Until: "5m", Contains: "boom", Level: "warn"}); err != nil {
		t.Fatalf("logs: %v", err)
	}
	if q.Get("since") != "10m" || q.Get("until") != "5m" || q.Get("contains") != "boom" || q.Get("level") != "warn" {
		t.Fatalf("query = %v", q)
	}
}

// TestPlainLogsDoNotProbe: an ordinary tail works on every server and costs
// one request, as before.
func TestPlainLogsDoNotProbe(t *testing.T) {
	var paths []string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = w.Write([]byte(`[]`))
	})
	if _, err := c.Logs(context.Background(), "17", client.LogsQuery{Tail: 50}); err != nil {
		t.Fatalf("logs: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("requests = %v", paths)
	}
}

func TestDeploymentWaitAsksTheServerToWait(t *testing.T) {
	var q string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"id":5,"status":"running","settling":true}`))
	})
	d, err := c.DeploymentWait(context.Background(), 5, 1500*time.Millisecond)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if q != "wait=2" {
		t.Fatalf("query = %q, want the wait rounded up to whole seconds", q)
	}
	if !d.Settling {
		t.Fatalf("settling lost: %+v", d)
	}
}

// TestConfigTellsAnOldServerFromAMissingApp: the two answers look alike (both
// are about the app reference) and must not be confused — one means "update
// Krill", the other "check the name".
func TestConfigTellsAnOldServerFromAMissingApp(t *testing.T) {
	t.Run("old server", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(oldRefRefusal("acme/production/bot/config")))
		})
		_, err := c.AppConfig(context.Background(), "acme/production/bot")
		var old *client.OldServerError
		if !errors.As(err, &old) {
			t.Fatalf("want OldServerError, got %v", err)
		}
	})
	t.Run("old server, domain check by id", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(oldRefRefusal("17/domain-status")))
		})
		_, err := c.DomainStatus(context.Background(), "17")
		var old *client.OldServerError
		if !errors.As(err, &old) {
			t.Fatalf("want OldServerError, got %v", err)
		}
	})
	t.Run("missing app", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"not_found","message":"no application \"acme/production/typo\" in this organization"}`))
		})
		_, err := c.AppConfig(context.Background(), "acme/production/typo")
		var old *client.OldServerError
		var ae *client.APIError
		if errors.As(err, &old) || !errors.As(err, &ae) || !ae.IsNotFound() {
			t.Fatalf("want the server's not-found, got %v", err)
		}
	})
	t.Run("an ill-formed reference is refused unsent", func(t *testing.T) {
		sent := false
		c, _ := newTestClient(t, func(http.ResponseWriter, *http.Request) { sent = true })
		_, err := c.AppConfig(context.Background(), "production/bot")
		var ue *client.UsageError
		if !errors.As(err, &ue) || sent {
			t.Fatalf("want a UsageError and no request, got %v (sent %v)", err, sent)
		}
	})
}

// TestBackupsRouteMissing: the router's bare 404 means "no such route" on an
// older Krill, but it is also what every route answers with the agent API off
// — whoami tells the two apart.
func TestBackupsRouteMissing(t *testing.T) {
	t.Run("older server", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/whoami" {
				_, _ = w.Write([]byte(`{"org_name":"acme"}`))
				return
			}
			http.Error(w, strings.TrimSpace(routerNotFound), http.StatusNotFound)
		})
		_, err := c.Backups(context.Background(), client.BackupFilter{})
		var old *client.OldServerError
		if !errors.As(err, &old) {
			t.Fatalf("want OldServerError, got %v", err)
		}
		_, err = c.RunBackup(context.Background(), "database", 3)
		if !errors.As(err, &old) {
			t.Fatalf("run: want OldServerError, got %v", err)
		}
	})
	t.Run("agent API off", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, strings.TrimSpace(routerNotFound), http.StatusNotFound)
		})
		_, err := c.Backups(context.Background(), client.BackupFilter{})
		var old *client.OldServerError
		var ae *client.APIError
		if errors.As(err, &old) || !errors.As(err, &ae) || ae.Code != "http_404" {
			t.Fatalf("want the router's 404 kept, got %v", err)
		}
	})
	t.Run("a missing backup is not an old server", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"not_found","message":"no database backup 999"}`))
		})
		_, err := c.RunBackup(context.Background(), "database", 999)
		var old *client.OldServerError
		if errors.As(err, &old) {
			t.Fatalf("a handler's not-found was read as an old server: %v", err)
		}
	})
}

func TestBackupsQueryAndRun(t *testing.T) {
	var gotPath, gotQuery, gotMethod string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotMethod = r.URL.Path, r.URL.RawQuery, r.Method
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"kind":"volume","id":4,"status":"started","started_at":"2026-11-26T10:00:00.123456Z"}`))
			return
		}
		_, _ = w.Write([]byte(`[{"kind":"volume","id":4,"target":"acme/production/bot:data","running":true}]`))
	})
	list, err := c.Backups(context.Background(), client.BackupFilter{Environment: "acme/production"})
	if err != nil || len(list) != 1 || !list[0].Running {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if gotPath != "/api/v1/backups" || gotQuery != "environment=acme%2Fproduction" {
		t.Fatalf("list request = %s ?%s", gotPath, gotQuery)
	}
	st, err := c.RunBackup(context.Background(), "volume", 4)
	if err != nil || st.StartedAt != "2026-11-26T10:00:00.123456Z" {
		t.Fatalf("run = %+v, %v", st, err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/backups/volume/4/run" {
		t.Fatalf("run request = %s %s", gotMethod, gotPath)
	}
}

func TestWellFormedRef(t *testing.T) {
	for ref, want := range map[string]bool{
		"17": true, "acme/production/bot": true,
		"bot": false, "production/bot": false, "a/b/c/d": false, "a//c": false, "": false,
	} {
		if got := client.WellFormedRef(ref); got != want {
			t.Errorf("WellFormedRef(%q) = %v, want %v", ref, got, want)
		}
	}
}
