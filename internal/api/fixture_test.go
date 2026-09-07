package api_test

import (
	"strconv"
	"testing"

	"github.com/proshik/krill/internal/api"
	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/testutil"
)

type apiFixture struct {
	q           *db.Queries
	svc         *api.Service
	ident       api.Identity // write-level, owner of the org that holds the app
	readIdent   api.Identity // read-level, same org
	otherIdent  api.Identity // write-level, a different org
	appID       int64
	appIDString string
	// dep is set by newWriteFixture; stopDeployer makes Enqueue refuse
	// without it being a conflict.
	dep *deploy.Deployer
}

// stopDeployer shuts the deployer down so Enqueue returns 0 for a reason that
// is NOT an in-flight conflict — the only such reason a test can reach.
func (f *apiFixture) stopDeployer() {
	if f.dep != nil {
		f.dep.Stop()
	}
}

// newAPIFixture builds two orgs: "acme" with project acme-proj / env production
// / app bot, and "rival" with nothing in it.
func newAPIFixture(t *testing.T) *apiFixture {
	t.Helper()
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	authSvc := auth.NewService(q)
	if err := authSvc.SeedAdmin(t.Context(), "admin@k.local", "pw"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	orgSvc := org.NewService(q)

	u, err := q.GetUserByEmail(t.Context(), "admin@k.local")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	// CreateOrg(ctx, ownerID, name) — owner first.
	acme, err := orgSvc.CreateOrg(t.Context(), u.ID, "acme")
	if err != nil {
		t.Fatalf("org acme: %v", err)
	}
	rival, err := orgSvc.CreateOrg(t.Context(), u.ID, "rival")
	if err != nil {
		t.Fatalf("org rival: %v", err)
	}
	proj, err := orgSvc.CreateProject(t.Context(), acme.ID, "acme-proj", "")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	env, err := orgSvc.CreateEnvironment(t.Context(), proj.ID, "production")
	if err != nil {
		t.Fatalf("environment: %v", err)
	}
	// SourceType is CHECK-constrained to image|dockerfile — omitting it fails
	// the insert with an opaque constraint error.
	app, err := q.CreateApplication(t.Context(), db.CreateApplicationParams{
		EnvironmentID: env.ID,
		Name:          "bot",
		Image:         "nginx",
		Tag:           "alpine",
		Domain:        "bot.example.test",
		Port:          80,
		EnvText:       "# db\nPORT=8080\nSECRET_TOKEN=hunter2",
		SourceType:    "image",
	})
	if err != nil {
		t.Fatalf("application: %v", err)
	}

	// Engine and deployer are nil: these tests exercise resolution, tenancy and
	// the level gate, none of which touch docker.
	svc := api.NewService(q, nil, nil)
	return &apiFixture{
		q:           q,
		svc:         svc,
		ident:       api.Identity{UserID: u.ID, OrgID: acme.ID, TokenID: 1, Level: api.LevelWrite, Role: auth.RoleOwner},
		readIdent:   api.Identity{UserID: u.ID, OrgID: acme.ID, TokenID: 2, Level: api.LevelRead, Role: auth.RoleOwner},
		otherIdent:  api.Identity{UserID: u.ID, OrgID: rival.ID, TokenID: 3, Level: api.LevelWrite, Role: auth.RoleOwner},
		appID:       app.ID,
		appIDString: strconv.FormatInt(app.ID, 10),
	}
}

// seedDeployment creates a deployment row for the fixture's app (bot, in
// acme) and returns its id. Call it multiple times to seed a history.
func (f *apiFixture) seedDeployment(t *testing.T) int64 {
	t.Helper()
	dep, err := f.q.CreateDeployment(t.Context(), db.CreateDeploymentParams{
		ApplicationID: f.appID,
		Trigger:       "manual",
	})
	if err != nil {
		t.Fatalf("seed deployment: %v", err)
	}
	return dep.ID
}
