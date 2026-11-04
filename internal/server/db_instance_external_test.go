package server_test

import (
	"context"
	"github.com/proshik/krill/internal/oplock"
	"net/http"
	"net/url"
	"testing"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
)

func TestSetExternalPortToggle(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()
	uid := mkUser(t, q, "extport@k.local")
	o, _ := orgSvc.CreateOrg(ctx, uid, "Org")
	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "pg", AppName: "krill-postgres-pg-1",
		Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "x", ExternalPort: nil, NodeHostname: "",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	cookie := loginAs(t, q, "extport@k.local")
	base := "/orgs/" + i64(o.ID) + "/db-servers/" + i64(inst.ID)
	t.Cleanup(func() { awaitInstanceLifecycle(t, inst.AppName) })

	// set a valid port
	if rec := postForm(t, h, base+"/external-port", cookie, url.Values{"external_port": {"5433"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("set: got %d, want 303", rec.Code)
	}
	got, _ := q.GetDBInstance(ctx, inst.ID)
	if got.ExternalPort == nil || *got.ExternalPort != 5433 {
		t.Fatalf("external_port not persisted: %v", got.ExternalPort)
	}

	// clear it
	awaitInstanceLifecycle(t, inst.AppName)
	postForm(t, h, base+"/external-port", cookie, url.Values{"external_port": {""}})
	got, _ = q.GetDBInstance(ctx, inst.ID)
	if got.ExternalPort != nil {
		t.Fatalf("external_port should be cleared, got %v", *got.ExternalPort)
	}

	// out-of-range rejected
	awaitInstanceLifecycle(t, inst.AppName)
	postForm(t, h, base+"/external-port", cookie, url.Values{"external_port": {"70000"}})
	if got, _ := q.GetDBInstance(ctx, inst.ID); got.ExternalPort != nil {
		t.Fatalf("bad port must not persist, got %v", *got.ExternalPort)
	}
}

// TestSetConsolePortToggle exercises the same edit handler's console-port
// path (parsing/range/persist), and that submitting only external_port
// leaves an already-set console_external_port untouched (the "absent field
// must not silently wipe it" guard in setDBInstanceExternalPort).
func TestSetConsolePortToggle(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()
	uid := mkUser(t, q, "consoleport@k.local")
	o, _ := orgSvc.CreateOrg(ctx, uid, "Org")
	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "pg", AppName: "krill-postgres-pg-2",
		Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "x",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	cookie := loginAs(t, q, "consoleport@k.local")
	base := "/orgs/" + i64(o.ID) + "/db-servers/" + i64(inst.ID)
	t.Cleanup(func() { awaitInstanceLifecycle(t, inst.AppName) })

	// set both external and console ports together
	if rec := postForm(t, h, base+"/external-port", cookie, url.Values{
		"external_port": {"9100"}, "console_external_port": {"9101"},
	}); rec.Code != http.StatusSeeOther {
		t.Fatalf("set: got %d, want 303", rec.Code)
	}
	got, _ := q.GetDBInstance(ctx, inst.ID)
	if got.ConsoleExternalPort == nil || *got.ConsoleExternalPort != 9101 {
		t.Fatalf("console_external_port not persisted: %v", got.ConsoleExternalPort)
	}

	// editing external_port alone (no console_external_port field in the
	// request) must NOT clear the previously-set console port
	awaitInstanceLifecycle(t, inst.AppName)
	postForm(t, h, base+"/external-port", cookie, url.Values{"external_port": {"9100"}})
	got, _ = q.GetDBInstance(ctx, inst.ID)
	if got.ConsoleExternalPort == nil || *got.ConsoleExternalPort != 9101 {
		t.Fatalf("console_external_port must survive an external_port-only edit, got %v", got.ConsoleExternalPort)
	}

	// a console port equal to the instance's own external port is rejected
	// (both would host-publish the same port for two different targets)
	awaitInstanceLifecycle(t, inst.AppName)
	postForm(t, h, base+"/external-port", cookie, url.Values{
		"external_port": {"9100"}, "console_external_port": {"9100"},
	})
	got, _ = q.GetDBInstance(ctx, inst.ID)
	if got.ConsoleExternalPort == nil || *got.ConsoleExternalPort != 9101 {
		t.Fatalf("self-collision must be rejected, console_external_port changed to %v", got.ConsoleExternalPort)
	}

	// clear the console port explicitly
	awaitInstanceLifecycle(t, inst.AppName)
	postForm(t, h, base+"/external-port", cookie, url.Values{
		"external_port": {"9100"}, "console_external_port": {""},
	})
	got, _ = q.GetDBInstance(ctx, inst.ID)
	if got.ConsoleExternalPort != nil {
		t.Fatalf("console_external_port should be cleared, got %v", *got.ConsoleExternalPort)
	}
}

// TestCreateDBInstanceConsolePortConflict exercises the create-time
// console_external_port conflict check: a console port colliding with
// another instance's external_port (both are host-published, same
// namespace) must be rejected.
func TestCreateDBInstanceConsolePortConflict(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()
	uid := mkUser(t, q, "consoleconflict@k.local")
	o, _ := orgSvc.CreateOrg(ctx, uid, "Org")
	ep := int32(9500)
	if _, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "existing", AppName: "krill-postgres-existing",
		Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "x", ExternalPort: &ep,
	}); err != nil {
		t.Fatalf("create existing instance: %v", err)
	}
	cookie := loginAs(t, q, "consoleconflict@k.local")

	if rec := postForm(t, h, "/orgs/"+i64(o.ID)+"/db-servers", cookie, url.Values{
		"engine": {"postgres"}, "name": {"new"}, "console_external_port": {"9500"},
	}); rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("console port conflicting with another instance's external_port want 303+err, got %d", rec.Code)
	}
	insts, _ := q.ListDBInstancesByOrg(ctx, o.ID)
	if len(insts) != 1 {
		t.Fatalf("conflicting instance must not be created, got %d instances", len(insts))
	}
}

// TestCreateDBInstanceConsolePortConsoleConflict is the console-vs-console
// counterpart of TestCreateDBInstanceConsolePortConflict: a candidate
// console_external_port colliding with another instance's
// console_external_port (as opposed to its external_port) must also be
// rejected — both columns are host-published and share one port namespace
// (CountDBInstancesByExternalPort checks external_port OR
// console_external_port).
func TestCreateDBInstanceConsolePortConsoleConflict(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()
	uid := mkUser(t, q, "consoleconsoleconflict@k.local")
	o, _ := orgSvc.CreateOrg(ctx, uid, "Org")
	cp := int32(9600)
	if _, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "existing", AppName: "krill-postgres-existing2",
		Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "x", ConsoleExternalPort: &cp,
	}); err != nil {
		t.Fatalf("create existing instance: %v", err)
	}
	cookie := loginAs(t, q, "consoleconsoleconflict@k.local")

	if rec := postForm(t, h, "/orgs/"+i64(o.ID)+"/db-servers", cookie, url.Values{
		"engine": {"postgres"}, "name": {"new"}, "console_external_port": {"9600"},
	}); rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("console port conflicting with another instance's console_external_port want 303+err, got %d", rec.Code)
	}
	insts, _ := q.ListDBInstancesByOrg(ctx, o.ID)
	if len(insts) != 1 {
		t.Fatalf("conflicting instance must not be created, got %d instances", len(insts))
	}
}

// Each edit starts an asynchronous redeploy. A subsequent independent edit
// must wait; overlapping edits now correctly receive busy without changing state.
func awaitInstanceLifecycle(t *testing.T, appName string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for oplock.Held(oplock.DBInstance(appName)) {
		if time.Now().After(deadline) {
			t.Fatal("instance lifecycle did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
