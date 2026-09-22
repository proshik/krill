package database_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/testutil"
)

func TestAppMetricsStorage(t *testing.T) {
	ctx := context.Background()
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	var appID int64
	err := pool.QueryRow(ctx, `WITH u AS (
 INSERT INTO users (email,password_hash) VALUES ('metrics@example.test','unused') RETURNING id
 ), o AS (
 INSERT INTO organizations (name,slug,owner_id) SELECT 'Org','org',id FROM u RETURNING id
 ), p AS (
 INSERT INTO projects (organization_id,name,slug) SELECT id,'Project','project' FROM o RETURNING id
 ), e AS (
 INSERT INTO environments (project_id,name,slug) SELECT id,'Production','production' FROM p RETURNING id
 ) INSERT INTO applications (environment_id,name,image,domain,port,source_type)
 SELECT id,'App','nginx','metrics.example.test',8080,'image' FROM e RETURNING id`).Scan(&appID)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := q.GetApplicationMetrics(ctx, appID)
	if err != nil {
		t.Fatal(err)
	}
	if settings.MetricsEnabled || settings.MetricsToken != nil || settings.MetricsTokenEnv != "KRILL_METRICS_TOKEN" || settings.Port != 8080 {
		t.Fatalf("unexpected defaults: %+v", settings)
	}
	var endpointID int64
	var path, job string
	err = pool.QueryRow(ctx, `INSERT INTO app_metrics_endpoints (application_id,port) VALUES ($1,8080) RETURNING id,path,job`, appID).Scan(&endpointID, &path, &job)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/metrics" || job != "" {
		t.Fatalf("unexpected endpoint defaults: %q %q", path, job)
	}
	for _, tc := range []struct {
		port       int32
		path, code string
	}{
		{8080, "/metrics", "23505"}, {8080, "metrics", "23514"}, {0, "/invalid", "23514"}, {65536, "/invalid", "23514"},
	} {
		_, err := q.CreateMetricsEndpoint(ctx, db.CreateMetricsEndpointParams{ApplicationID: appID, Port: tc.port, Path: tc.path})
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != tc.code {
			t.Errorf("endpoint (%d,%q): %v, want %s", tc.port, tc.path, err, tc.code)
		}
	}
	checkTargets := func(want int) {
		t.Helper()
		rows, err := q.ListMetricsScrapeTargets(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != want {
			t.Fatalf("targets: got %d, want %d", len(rows), want)
		}
		if want > 0 && (rows[0].AppID != appID || rows[0].OrgName != "Org" || rows[0].ProjectName != "Project" || rows[0].EnvName != "Production" || rows[0].EndpointID != endpointID || rows[0].OrgID == 0) {
			t.Fatalf("incorrect target identity: %+v", rows[0])
		}
	}
	checkTargets(0)
	if err := q.SetApplicationMetricsEnabled(ctx, db.SetApplicationMetricsEnabledParams{ID: appID, MetricsEnabled: true}); err != nil {
		t.Fatal(err)
	}
	checkTargets(0)
	token := "encrypted-token"
	if err := q.SetApplicationMetricsToken(ctx, db.SetApplicationMetricsTokenParams{ID: appID, MetricsToken: &token}); err != nil {
		t.Fatal(err)
	}
	checkTargets(1)
	if err := q.SetApplicationMetricsEnabled(ctx, db.SetApplicationMetricsEnabledParams{ID: appID}); err != nil {
		t.Fatal(err)
	}
	checkTargets(0)
	if err := q.DeleteApplication(ctx, appID); err != nil {
		t.Fatal(err)
	}
	count, err := q.CountMetricsEndpointsByApplication(ctx, appID)
	if err != nil || count != 0 {
		t.Fatalf("cascade count=%d err=%v", count, err)
	}
}
