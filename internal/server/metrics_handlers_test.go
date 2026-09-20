package server_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/secret"
)

func TestAppMetricsActionsAndVisibility(t *testing.T) {
	secret.Init("metrics-test-key")
	defer secret.Init("")
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, appID, _, orgID := rpFixture(t, h, q, orgSvc, "metrics-owner@example.test")
	post := func(path string, form url.Values, wantErr bool) {
		t.Helper()
		rec := postForm(t, h, base+path, cookie, form)
		if rec.Code != 303 || hasErrFlash(rec) != wantErr {
			t.Fatalf("%s: status=%d err=%v body=%s", path, rec.Code, hasErrFlash(rec), rec.Body.String())
		}
	}
	post("/metrics/enable", nil, false)
	m, err := q.GetApplicationMetrics(ctx, appID)
	if err != nil {
		t.Fatal(err)
	}
	if !m.MetricsEnabled || m.MetricsToken == nil || !strings.HasPrefix(*m.MetricsToken, "enc:") {
		t.Fatal("metrics not enabled with encrypted token")
	}
	token, err := secret.Dec(*m.MetricsToken)
	if err != nil || len(token) != 64 {
		t.Fatalf("invalid token: %v", err)
	}
	post("/metrics/enable", nil, false)
	eps, _ := q.ListMetricsEndpointsByApplication(ctx, appID)
	if len(eps) != 1 || eps[0].Port != m.Port {
		t.Fatalf("default endpoints: %+v", eps)
	}
	next, _ := q.GetApplicationMetrics(ctx, appID)
	if *next.MetricsToken != *m.MetricsToken {
		t.Fatal("enable rotated token")
	}
	post("/metrics/endpoints", url.Values{"port": {"27015"}, "path": {"/metrics/"}, "job": {"relay"}}, false)
	post("/metrics/endpoints", url.Values{"port": {"27015"}, "path": {"/metrics"}}, true)
	for _, form := range []url.Values{{"port": {"0"}, "path": {"/metrics"}}, {"port": {"1"}, "path": {"/"}}, {"port": {"1"}, "path": {"/metrics"}, "job": {"bad job"}}} {
		post("/metrics/endpoints", form, true)
	}
	post("/ports", url.Values{"host_port": {"29001"}, "container_port": {"27015"}, "protocol": {"tcp"}}, true)
	post("/ports", url.Values{"host_port": {"29001"}, "container_port": {"27015"}, "protocol": {"udp"}}, false)
	post("/env", url.Values{"env": {"KRILL_METRICS_TOKEN=clash"}}, true)
	post("/metrics/token-env", url.Values{"name": {"1BAD"}}, true)
	post("/metrics/token-env", url.Values{"name": {"METRICS_TOKEN"}}, false)
	member := mkUser(t, q, "metrics-member@example.test")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: orgID, UserID: member, Role: "member"}); err != nil {
		t.Fatal(err)
	}
	mc := loginAs(t, q, "metrics-member@example.test")
	for _, path := range []string{"enable", "disable", "endpoints", "endpoints/1/delete", "token-env", "token/rotate"} {
		if rec := postForm(t, h, base+"/metrics/"+path, mc, nil); rec.Code != http.StatusForbidden {
			t.Fatalf("member POST %s=%d", path, rec.Code)
		}
	}
	for _, who := range []*http.Cookie{cookie, mc} {
		rec := getPage(t, h, base+"?tab=metrics", who, nil)
		if rec.Code != 200 {
			t.Fatalf("metrics page=%d", rec.Code)
		}
		if strings.Contains(rec.Body.String(), token) != (who == cookie) {
			t.Fatal("metrics token visibility mismatch")
		}
	}
	post("/metrics/disable", nil, false)
	off, _ := q.GetApplicationMetrics(ctx, appID)
	if off.MetricsEnabled || off.MetricsToken == nil {
		t.Fatal("disable erased token or stayed enabled")
	}
	post("/metrics/token/rotate", nil, false)
	rotated, _ := q.GetApplicationMetrics(ctx, appID)
	plain, err := secret.Dec(*rotated.MetricsToken)
	if err != nil || plain == token {
		t.Fatal("rotate did not replace token")
	}
	post("/metrics/endpoints/"+i64(eps[0].ID)+"/delete", nil, false)
}

func TestAppMetricsLimitsPublishedPortsAndTenancy(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, appID, _, _ := rpFixture(t, h, q, orgSvc, "metrics-limit@example.test")
	m, _ := q.GetApplicationMetrics(ctx, appID)
	port, err := q.CreateAppPort(ctx, db.CreateAppPortParams{ApplicationID: appID, HostPort: 29111, ContainerPort: m.Port, Protocol: "tcp"})
	if err != nil {
		t.Fatal(err)
	}
	if rec := postForm(t, h, base+"/metrics/enable", cookie, nil); !hasErrFlash(rec) {
		t.Fatal("published app port accepted")
	}
	if err := q.DeleteAppPort(ctx, db.DeleteAppPortParams{ID: port.ID, ApplicationID: appID}); err != nil {
		t.Fatal(err)
	}
	if err := q.UpdateApplicationEnv(ctx, db.UpdateApplicationEnvParams{ID: appID, EnvText: "KRILL_METRICS_TOKEN=x"}); err != nil {
		t.Fatal(err)
	}
	if rec := postForm(t, h, base+"/metrics/enable", cookie, nil); !hasErrFlash(rec) {
		t.Fatal("env collision accepted")
	}
	if err := q.UpdateApplicationEnv(ctx, db.UpdateApplicationEnvParams{ID: appID}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := q.CreateMetricsEndpoint(ctx, db.CreateMetricsEndpointParams{ApplicationID: appID, Port: int32(1000 + i), Path: "/metrics"}); err != nil {
			t.Fatal(err)
		}
	}
	if rec := postForm(t, h, base+"/metrics/endpoints", cookie, url.Values{"port": {"5000"}, "path": {"/metrics"}}); !hasErrFlash(rec) {
		t.Fatal("eleventh endpoint accepted")
	}
	otherBase, otherCookie, otherID, _, _ := rpFixture(t, h, q, orgSvc, "metrics-other@example.test")
	ep, err := q.CreateMetricsEndpoint(ctx, db.CreateMetricsEndpointParams{ApplicationID: otherID, Port: 8080, Path: "/metrics"})
	if err != nil {
		t.Fatal(err)
	}
	if rec := postForm(t, h, base+"/metrics/endpoints/"+i64(ep.ID)+"/delete", cookie, nil); rec.Code != 404 {
		t.Fatalf("foreign endpoint delete=%d", rec.Code)
	}
	if rec := postForm(t, h, otherBase+"/metrics/enable", cookie, nil); rec.Code != 404 {
		t.Fatalf("foreign app enable=%d", rec.Code)
	}
	if rec := getPage(t, h, base+"/metrics/status", otherCookie, nil); rec.Code != 404 {
		t.Fatalf("foreign status=%d", rec.Code)
	}
	if _, err := q.GetMetricsEndpoint(ctx, ep.ID); err != nil {
		t.Fatal("foreign endpoint was removed")
	}
}
