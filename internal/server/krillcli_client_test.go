package server_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/proshik/krill/internal/krillcli/client"
)

// TestKrillCLIClientAgainstTheServer drives krill-cli's client against the
// real router and handlers, so the answers the client tells apart — a
// missing app versus an operation the server lacks — are this server's
// actual answers, not the client tests' copies of them. Only the "current
// server" side can be run here; the older server's answers are pinned in
// internal/krillcli/client's tests.
func TestKrillCLIClientAgainstTheServer(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	fx := seedAPISuccessFixture(t, q, orgSvc)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := client.New(srv.URL, fx.token, "krill-cli/test")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	notOld := func(t *testing.T, err error) {
		t.Helper()
		var old *client.OldServerError
		if errors.As(err, &old) {
			t.Fatalf("a current server was taken for an older one: %v", err)
		}
	}

	who, err := c.Whoami(ctx)
	if err != nil || who.InstanceVersion() == "" {
		t.Fatalf("whoami: %+v, %v", who, err)
	}

	cfg, err := c.AppConfig(ctx, fx.path)
	if err != nil || cfg.ID != fx.appID {
		t.Fatalf("config: %+v, %v", cfg, err)
	}
	_, err = c.AppConfig(ctx, "proj/prod/typo")
	notOld(t, err)
	var ae *client.APIError
	if !errors.As(err, &ae) || !ae.IsNotFound() {
		t.Fatalf("config of a missing app: want 404, got %v", err)
	}

	if _, err := c.DomainStatus(ctx, fx.path); err != nil {
		t.Fatalf("domain status: %v", err)
	}

	if _, err := c.Backups(ctx, client.BackupFilter{Environment: "proj/prod"}); err != nil {
		t.Fatalf("backups: %v", err)
	}
	_, err = c.RunBackup(ctx, "database", 999999)
	notOld(t, err)
	if !errors.As(err, &ae) || !ae.IsNotFound() {
		t.Fatalf("run of a missing backup: want 404, got %v", err)
	}

	// The window filters probe whoami first; a current server passes, and
	// the read reaches the log handler of the right app.
	lines, err := c.Logs(ctx, fx.path, client.LogsQuery{Since: "10m", Contains: "x"})
	if err != nil {
		t.Fatalf("log window: %v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("the no-op engine has no lines, got %+v", lines)
	}
	_, err = c.Logs(ctx, fx.path, client.LogsQuery{Since: "soon"})
	if !errors.As(err, &ae) || ae.HTTPStatus != 400 {
		t.Fatalf("a bad since must reach the handler and be refused there: %v", err)
	}

	// WaitLog is not driven here: this server's no-op engine has no log
	// stream to follow. Its answer's shape is pinned in the client's tests.

	_, err = c.DeploymentWait(ctx, 999999, 2*time.Second)
	if !errors.As(err, &ae) || !ae.IsNotFound() {
		t.Fatalf("wait on a missing deployment: want 404, got %v", err)
	}
}
