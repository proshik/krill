package server_test

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"testing"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/backup"
	"github.com/proshik/krill/internal/config"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/testutil"

	"github.com/jackc/pgx/v5/pgxpool"
)

// labelEngine is a noopEngine that records the labels written by
// ServiceUpdateLabels.
type labelEngine struct {
	noopEngine
	mu   sync.Mutex
	last map[string]string
}

func (e *labelEngine) ServiceUpdateLabels(_ context.Context, _ string, labels map[string]string) error {
	e.mu.Lock()
	e.last = labels
	e.mu.Unlock()
	return nil
}

func (e *labelEngine) lastLabels() map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.last
}

func newServerWithLabelEngine(t *testing.T, eng *labelEngine) (http.Handler, *db.Queries, *org.Service) {
	h, q, orgSvc, _ := newServerWithLabelEnginePool(t, eng)
	return h, q, orgSvc
}

// newServerWithLabelEnginePool also hands back the pool, for tests that have to
// reach past sqlc (e.g. to make one specific write fail).
func newServerWithLabelEnginePool(t *testing.T, eng *labelEngine) (http.Handler, *db.Queries, *org.Service, *pgxpool.Pool) {
	t.Helper()
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	orgSvc := org.NewService(q)
	cfg := config.Config{BaseDomain: "127-0-0-1.sslip.io", Network: "krill-net", AllowPrivateEgress: true}
	hub := deploy.NewLogHub()
	dbSvc := dbservice.New(eng, dbservice.NewDBStore(q), hub, "krill-net")
	srv := server.New(cfg, auth.NewService(q), orgSvc, q, nil, eng, hub, dbSvc)
	srv.SetBackups(backup.New(nil, backup.NewDBStore(q), true), func() {})
	return srv.Router(), q, orgSvc, pool
}

// A domain mutation re-applies the app's Traefik labels on the live service.
// Those labels tell Traefik which network to reach the app on, so they must
// name the app's ORGANIZATION network — the one the app actually runs in. The
// configured (shared) network would point the gateway at a network the app left,
// silently breaking routing until the next full deploy rewrote the label.
func TestDomainMutationKeepsTheOrganizationNetworkInTheLabels(t *testing.T) {
	eng := &labelEngine{}
	h, q, orgSvc := newServerWithLabelEngine(t, eng)
	ctx := context.Background()

	ownerID := mkUser(t, q, "owner-net@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	orgNet := "krill-org-" + i64(o.ID)
	if err := q.SetOrganizationNetwork(ctx, db.SetOrganizationNetworkParams{ID: o.ID, NetworkName: orgNet}); err != nil {
		t.Fatalf("set organization network: %v", err)
	}
	p, _ := orgSvc.CreateProject(ctx, o.ID, "Proj", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	a, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "web.example.test", Port: 80, SourceType: "image",
		GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create application: %v", err)
	}
	// The network label is only emitted for a router, i.e. for an exposed
	// domain; an internal-only app carries traefik.enable=false and nothing else.
	if _, err := q.CreateDomain(ctx, db.CreateDomainParams{
		ApplicationID: a.ID, Host: "web.example.test", Tls: false, IsPrimary: true, Exposed: true, Paths: "",
	}); err != nil {
		t.Fatalf("create primary domain: %v", err)
	}
	base := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/apps/" + i64(a.ID)
	cookie := loginAs(t, q, "owner-net@k.local")

	rec := postForm(t, h, base+"/domains", cookie, url.Values{"host": {"extra.example.test"}})
	if rec.Code != http.StatusSeeOther && rec.Code != http.StatusOK {
		t.Fatalf("add domain: status %d", rec.Code)
	}

	labels := eng.lastLabels()
	if labels == nil {
		t.Fatal("the domain mutation did not re-apply any labels")
	}
	if got := labels["traefik.docker.network"]; got != orgNet {
		t.Fatalf("traefik.docker.network = %q, want the organization network %q", got, orgNet)
	}
}

// An organization that has not been migrated yet has an empty network_name; the
// configured network stays the fallback, exactly as the deployer does it.
func TestDomainMutationFallsBackToTheConfiguredNetwork(t *testing.T) {
	eng := &labelEngine{}
	h, q, orgSvc := newServerWithLabelEngine(t, eng)
	ctx := context.Background()

	ownerID := mkUser(t, q, "owner-net2@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org") // network_name left empty
	p, _ := orgSvc.CreateProject(ctx, o.ID, "Proj", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	a, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "web2.example.test", Port: 80, SourceType: "image",
		GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create application: %v", err)
	}
	if _, err := q.CreateDomain(ctx, db.CreateDomainParams{
		ApplicationID: a.ID, Host: "web2.example.test", Tls: false, IsPrimary: true, Exposed: true, Paths: "",
	}); err != nil {
		t.Fatalf("create primary domain: %v", err)
	}
	base := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/apps/" + i64(a.ID)
	cookie := loginAs(t, q, "owner-net2@k.local")

	postForm(t, h, base+"/domains", cookie, url.Values{"host": {"extra2.example.test"}})

	labels := eng.lastLabels()
	if labels == nil {
		t.Fatal("the domain mutation did not re-apply any labels")
	}
	if got := labels["traefik.docker.network"]; got != "krill-net" {
		t.Fatalf("traefik.docker.network = %q, want the configured fallback %q", got, "krill-net")
	}
}

// An organization created through the UI was never on the shared network, so
// the startup migration has nothing to move for it. It must therefore be
// recorded as already migrated at creation time — otherwise the next restart
// redeploys every service it has acquired since (rebuilding Dockerfile apps,
// waiting on databases) purely to move them where they already are.
func TestCreateOrgMarksItAlreadyOnItsOwnNetwork(t *testing.T) {
	eng := &labelEngine{}
	h, q, _ := newServerWithLabelEngine(t, eng)
	ctx := context.Background()

	mkUser(t, q, "founder@k.local")
	cookie := loginAs(t, q, "founder@k.local")

	rec := postForm(t, h, "/orgs", cookie, url.Values{"name": {"Fresh Org"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create org: status %d", rec.Code)
	}

	orgs, err := q.ListOrganizations(ctx)
	if err != nil {
		t.Fatalf("list organizations: %v", err)
	}
	if len(orgs) != 1 {
		t.Fatalf("want one organization, got %d", len(orgs))
	}
	if orgs[0].NetworkName != "krill-org-"+i64(orgs[0].ID) {
		t.Fatalf("network_name = %q", orgs[0].NetworkName)
	}
	if !orgs[0].NetworkMigratedAt.Valid {
		t.Fatal("a newly created organization must be recorded as already migrated")
	}
}

// Marking an organization migrated without a recorded network_name would be
// unrepairable: the startup pass skips anything already flagged, so it would
// never write the missing name and every app of that organization would fall
// back to the shared network forever. A failed name write must therefore leave
// the organization unflagged, so the next boot repairs it.
func TestCreateOrgDoesNotMarkMigratedWhenTheNetworkNameIsNotRecorded(t *testing.T) {
	eng := &labelEngine{}
	h, q, _, pool := newServerWithLabelEnginePool(t, eng)
	ctx := context.Background()

	// Fail exactly the network_name write, and nothing else: an UPDATE that only
	// touches network_migrated_at still goes through, so the test cannot pass by
	// accident just because every write was blocked.
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION krill_test_block_network_name() RETURNS trigger AS $$
		BEGIN
			IF NEW.network_name IS DISTINCT FROM OLD.network_name THEN
				RAISE EXCEPTION 'simulated failure writing network_name';
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER krill_test_block_network_name
		BEFORE UPDATE ON organizations
		FOR EACH ROW EXECUTE FUNCTION krill_test_block_network_name();
	`); err != nil {
		t.Fatalf("install trigger: %v", err)
	}

	mkUser(t, q, "unlucky@k.local")
	cookie := loginAs(t, q, "unlucky@k.local")
	if rec := postForm(t, h, "/orgs", cookie, url.Values{"name": {"Unlucky Org"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("create org: status %d", rec.Code)
	}

	orgs, err := q.ListOrganizations(ctx)
	if err != nil {
		t.Fatalf("list organizations: %v", err)
	}
	if len(orgs) != 1 {
		t.Fatalf("want one organization, got %d", len(orgs))
	}
	if orgs[0].NetworkName != "" {
		t.Fatalf("the trigger should have blocked the write, network_name = %q", orgs[0].NetworkName)
	}
	if orgs[0].NetworkMigratedAt.Valid {
		t.Fatal("an organization with no recorded network must not be marked migrated; the startup pass would never repair it")
	}
}
