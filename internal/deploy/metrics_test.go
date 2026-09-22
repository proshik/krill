package deploy

import (
	"context"
	"slices"
	"testing"

	"github.com/proshik/krill/internal/appmetrics"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/testutil"
)

func TestDeployAppMetrics(t *testing.T) {
	secret.Init("")
	defer secret.Init("")
	q := db.New(testutil.NewTestDB(t))
	ctx := context.Background()
	u, err := q.CreateUser(ctx, db.CreateUserParams{Email: "metrics@example.test", PasswordHash: "unused"})
	if err != nil {
		t.Fatal(err)
	}
	o, err := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "Org", Slug: "org", OwnerID: u.ID})
	if err != nil {
		t.Fatal(err)
	}
	p, err := q.CreateProject(ctx, db.CreateProjectParams{Name: "Project", Slug: "project", OrganizationID: o.ID})
	if err != nil {
		t.Fatal(err)
	}
	e, err := q.CreateEnvironment(ctx, db.CreateEnvironmentParams{Name: "Prod", Slug: "prod", ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	a, err := q.CreateApplication(ctx, db.CreateApplicationParams{EnvironmentID: e.ID, Name: "app", Image: "nginx", Tag: "alpine", Domain: "metrics.example.test", Port: 8080, SourceType: "image", EnvText: "METRICS_TOKEN=user"})
	if err != nil {
		t.Fatal(err)
	}
	store := NewDBStore(q)
	got, err := store.GetApplication(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Env[appmetrics.DefaultTokenEnv] != "" || got.ContainerLabels[appmetrics.ContainerLabelTokenHash] != "" || got.MetricsHiddenPaths != nil {
		t.Fatal("disabled metrics changed deployment")
	}
	token := "tok"
	for _, err := range []error{
		q.SetApplicationMetricsToken(ctx, db.SetApplicationMetricsTokenParams{ID: a.ID, MetricsToken: &token}),
		q.SetApplicationMetricsTokenEnv(ctx, db.SetApplicationMetricsTokenEnvParams{ID: a.ID, MetricsTokenEnv: "METRICS_TOKEN"}),
		q.SetApplicationMetricsEnabled(ctx, db.SetApplicationMetricsEnabledParams{ID: a.ID, MetricsEnabled: true}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, port := range []int32{8080, 27015} {
		if _, err := q.CreateMetricsEndpoint(ctx, db.CreateMetricsEndpointParams{ApplicationID: a.ID, Port: port, Path: "/metrics"}); err != nil {
			t.Fatal(err)
		}
	}
	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{OrganizationID: o.ID, Engine: "postgres", Name: "db", AppName: "krill-postgres-metrics", Image: "postgres:16-alpine", Superuser: "postgres", SuperuserPassword: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	ldb, err := q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{InstanceID: inst.ID, EnvironmentID: e.ID, Name: "db", DbName: "appdb", Username: "app", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateDBLink(ctx, db.CreateDBLinkParams{ApplicationID: a.ID, LogicalDatabaseID: &ldb.ID, VarName: "METRICS_TOKEN", Scheme: "postgres", Field: "url"}); err != nil {
		t.Fatal(err)
	}
	got, err = store.GetApplication(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Env["METRICS_TOKEN"] != token || got.ContainerLabels[appmetrics.ContainerLabelTokenHash] != appmetrics.TokenHash("METRICS_TOKEN", token) || !slices.Equal(got.MetricsHiddenPaths, []string{"/metrics"}) {
		t.Fatal("metrics token must override env and database links, fingerprint the deployment, and hide only the app HTTP path")
	}
	secret.Init("key-a")
	encrypted := secret.Enc(token)
	if err := q.SetApplicationMetricsToken(ctx, db.SetApplicationMetricsTokenParams{ID: a.ID, MetricsToken: &encrypted}); err != nil {
		t.Fatal(err)
	}
	secret.Init("key-b")
	if _, err := store.GetApplication(ctx, a.ID); err == nil {
		t.Fatal("an unreadable metrics token must stop deployment")
	}
}
