package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/api"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/server"
)

func TestSplitAppRefRefShapedVerbs(t *testing.T) {
	cases := []struct{ in, wantRef, wantVerb string }{
		{"17/config", "17", "config"},
		{"acme/production/bot/config", "acme/production/bot", "config"},
		{"acme/production/bot/domain-status", "acme/production/bot", "domain-status"},
		// An app literally named "config" or "domain-status" keeps its
		// path-form status route: what precedes the word is not a reference.
		{"acme/production/config", "acme/production/config", ""},
		{"acme/production/domain-status", "acme/production/domain-status", ""},
		// The older verbs keep their behavior.
		{"acme/production/deploy", "acme/production", "deploy"},
	}
	for _, tc := range cases {
		ref, verb := server.SplitAppRef(tc.in)
		if ref != tc.wantRef || verb != tc.wantVerb {
			t.Errorf("SplitAppRef(%q) = (%q,%q), want (%q,%q)", tc.in, ref, verb, tc.wantRef, tc.wantVerb)
		}
	}
}

func apiDo(t *testing.T, h http.Handler, method, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAPIDiagnosticRoutesRejectForeignOrgToken(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	fx := seedTwoOrgs(t, q, orgSvc)
	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/apps/" + fx.appID + "/config"},
		{http.MethodGet, "/api/v1/apps/" + fx.appID + "/domain-status"},
		{http.MethodGet, "/api/v1/apps/" + fx.appID + "/logs?wait_for=x&timeout=1"},
		{http.MethodGet, "/api/v1/deployments/" + fx.deployID + "?wait=5"},
		{http.MethodGet, "/api/v1/backups?app=" + fx.appID},
		{http.MethodPost, "/api/v1/backups/database/1/run"},
		{http.MethodPost, "/api/v1/backups/volume/1/run"},
		{http.MethodPost, "/api/v1/backups/bucket/1/run"},
	} {
		if rec := apiDo(t, h, rt.method, rt.path, fx.foreignToken); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: want 404, got %d: %s", rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}
}

func TestAPIDiagnosticRoutes(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	fx := seedAPISuccessFixture(t, q, orgSvc)
	ctx := context.Background()
	app, err := q.GetApplication(ctx, fx.appID)
	if err != nil {
		t.Fatal(err)
	}
	// An app named like the new verb.
	if _, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: app.EnvironmentID, Name: "config", Image: "nginx", Tag: "alpine",
		Domain: "config-app.example.test", Port: 80, SourceType: "image",
	}); err != nil {
		t.Fatal(err)
	}

	t.Run("whoami names the instance", func(t *testing.T) {
		rec := apiDo(t, h, http.MethodGet, "/api/v1/whoami", fx.token)
		var got api.WhoamiResult
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Instance.URL == "" || got.Instance.Version == "" {
			t.Fatalf("whoami: %s (%v)", rec.Body.String(), err)
		}
	})
	t.Run("config", func(t *testing.T) {
		rec := apiDo(t, h, http.MethodGet, "/api/v1/apps/"+fx.path+"/config", fx.token)
		var got api.AppConfig
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.ID != fx.appID {
			t.Fatalf("config: %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("an app named config keeps its status route", func(t *testing.T) {
		rec := apiDo(t, h, http.MethodGet, "/api/v1/apps/proj/prod/config", fx.token)
		var got api.AppStatus
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.Name != "config" {
			t.Fatalf("status of app config: %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("domain status", func(t *testing.T) {
		rec := apiDo(t, h, http.MethodGet, "/api/v1/apps/"+fx.path+"/domain-status", fx.token)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"domains"`) {
			t.Fatalf("domain status: %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("backups list", func(t *testing.T) {
		rec := apiDo(t, h, http.MethodGet, "/api/v1/backups?environment=proj/prod", fx.token)
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
			t.Fatalf("backups: %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("bad parameters are 400", func(t *testing.T) {
		for _, p := range []string{
			"/api/v1/deployments/1?wait=soon",
			"/api/v1/apps/" + fx.path + "/logs?timeout=x&wait_for=y",
			"/api/v1/apps/" + fx.path + "/logs?until=10m",
			"/api/v1/apps/" + fx.path + "/logs?wait_for=y&contains=z",
			"/api/v1/backups?app=" + fx.path + "&environment=proj/prod",
		} {
			rec := apiDo(t, h, http.MethodGet, p, fx.token)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s: want 400, got %d: %s", p, rec.Code, rec.Body.String())
			}
		}
		if rec := apiDo(t, h, http.MethodPost, "/api/v1/backups/state/1/run", fx.token); rec.Code != http.StatusBadRequest {
			t.Errorf("unknown kind: %d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("backup now needs a write token", func(t *testing.T) {
		u, err := q.GetUserByEmail(ctx, "api-success@k.local")
		if err != nil {
			t.Fatal(err)
		}
		read := issueAPIToken(t, q, u.ID, fx.orgID, api.LevelRead)
		rec := apiDo(t, h, http.MethodPost, "/api/v1/backups/database/1/run", read)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("read token: %d %s", rec.Code, rec.Body.String())
		}
		// A write token gets past the gate to the tenancy check.
		if rec := apiDo(t, h, http.MethodPost, "/api/v1/backups/database/999999/run", fx.token); rec.Code != http.StatusNotFound {
			t.Fatalf("missing backup: %d %s", rec.Code, rec.Body.String())
		}
	})
}
