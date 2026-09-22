package server_test

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/appmetrics"
	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/config"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/observability"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/panel"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/testutil"
)

type metricsEngine struct {
	noopEngine
	addresses       []docker.TaskAddress
	addrErr         error
	containerLabels map[string]string
	found           bool
	synced          map[string]string
	execErr         error
	state           *docker.ServiceState
}

func (e *metricsEngine) ServiceState(ctx context.Context, name string) (docker.ServiceState, error) {
	if e.state != nil {
		return *e.state, nil
	}
	return e.noopEngine.ServiceState(ctx, name)
}

func (e *metricsEngine) TaskAddresses(context.Context) ([]docker.TaskAddress, error) {
	return e.addresses, e.addrErr
}
func (e *metricsEngine) ServiceContainerLabels(context.Context, string) (map[string]string, bool, error) {
	return e.containerLabels, e.found, nil
}
func (e *metricsEngine) ServiceUpdateLabels(_ context.Context, _ string, labels map[string]string) error {
	e.synced = labels
	return nil
}
func (e *metricsEngine) Exec(context.Context, string, []string, []string, io.Reader, io.Writer) error {
	return e.execErr
}

func TestAlloyAppsProviderAndStatus(t *testing.T) {
	ctx := context.Background()
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	orgSvc := org.NewService(q)
	engine := &metricsEngine{execErr: errors.New("test exec unavailable")}
	hub := deploy.NewLogHub()
	srv := server.New(config.Config{Network: "krill-net", BaseDomain: "127-0-0-1.sslip.io"}, auth.NewService(q), orgSvc, q, nil, engine, hub, dbservice.New(nil, dbservice.NewDBStore(q), hub, "krill-net"))
	tokens := panel.DeriveTokens("test-gateway-secret")
	srv.SetPanelGateway(tokens, "http://10.0.0.1:8080", nil)
	h := srv.Router()
	base, cookie, appID, _, _ := rpFixture(t, h, q, orgSvc, "provider@example.test")
	request := func(token string, gateway bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", observability.AppsProviderPath, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if gateway {
			req.Header.Set(panel.ForwardedHeader, tokens.Forwarded)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if request(tokens.AlloyApps, false).Code != 404 {
		t.Fatal("unwired provider accessible")
	}
	srv.SetAppsProvider(tokens.AlloyApps)
	for _, token := range []string{"", "wrong", tokens.Provider, tokens.Forwarded} {
		if request(token, false).Code != 404 {
			t.Fatal("invalid token accepted")
		}
	}
	if request(tokens.AlloyApps, true).Code != 404 {
		t.Fatal("gateway exposed provider")
	}
	status := func(want string) {
		t.Helper()
		rec := getPage(t, h, base+"/metrics/status", cookie, nil)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("status want %q: %d %s", want, rec.Code, rec.Body.String())
		}
	}
	status("Collection is off")
	rec := postForm(t, h, base+"/metrics/enable", cookie, nil)
	if hasErrFlash(rec) {
		t.Fatal("enable failed")
	}
	status("observability is off")
	// Traefik labels must immediately hide the default metrics path.
	hidden := false
	for _, value := range engine.synced {
		if strings.Contains(value, "!(PathRegexp(`(?i)^/metrics(?:") {
			hidden = true
		}
	}
	if !hidden {
		t.Fatal("enabling did not hide metrics path")
	}
	if err := observability.Save(ctx, q, observability.Input{Metrics: observability.TargetInput{URL: "http://receiver:9090/write"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.SetObservabilityEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	status("cannot run the collector")
	ctl := &fakeObsCtl{}
	srv.SetObservability(ctl, func(context.Context, observability.Settings) observability.Report { return observability.Report{} })
	status("collector is not running")
	ctl.status.Apps = docker.ServiceState{Found: true, Running: 1, Desired: 1}
	status("has not fetched its rules")
	engine.addresses = []docker.TaskAddress{{ServiceName: docker.ServiceName(appID), NodeID: "node-a", NodeHostname: "host-a", Network: "krill-net", IP: "10.0.9.4"}, {ServiceName: docker.ServiceName(appID), NodeID: "wrong", NodeHostname: "wrong-network", Network: "other-org", IP: "10.0.9.5"}}
	rec = request(tokens.AlloyApps, false)
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Content-Type") != "text/plain; charset=utf-8" || !strings.Contains(rec.Body.String(), "declare \"targets\"") || !strings.Contains(rec.Body.String(), "host-a") || strings.Contains(rec.Body.String(), "wrong-network") {
		t.Fatalf("provider: %d %s", rec.Code, rec.Body.String())
	}
	status("not deployed yet")
	engine.found = true
	status("Redeploy the app")
	m, _ := q.GetApplicationMetrics(ctx, appID)
	// newServer uses plaintext secret configuration in this test.
	engine.containerLabels = map[string]string{appmetrics.ContainerLabelTokenHash: appmetrics.TokenHash(m.MetricsTokenEnv, *m.MetricsToken)}
	engine.state = &docker.ServiceState{Found: true}
	status("The app is stopped")
	engine.state = nil
	status("test exec unavailable")
	engine.addrErr = errors.New("task listing failed")
	rec = request(tokens.AlloyApps, false)
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "declare") {
		t.Fatal("failed poll returned a module")
	}
	engine.addrErr = nil
	pool.Close()
	if rec := request(tokens.AlloyApps, false); rec.Code != 500 {
		t.Fatal("database failure must fail provider")
	}
}
