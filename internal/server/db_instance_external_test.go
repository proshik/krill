package server_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

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

	// set a valid port
	if rec := postForm(t, h, base+"/external-port", cookie, url.Values{"external_port": {"5433"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("set: got %d, want 303", rec.Code)
	}
	got, _ := q.GetDBInstance(ctx, inst.ID)
	if got.ExternalPort == nil || *got.ExternalPort != 5433 {
		t.Fatalf("external_port not persisted: %v", got.ExternalPort)
	}

	// clear it
	postForm(t, h, base+"/external-port", cookie, url.Values{"external_port": {""}})
	got, _ = q.GetDBInstance(ctx, inst.ID)
	if got.ExternalPort != nil {
		t.Fatalf("external_port should be cleared, got %v", *got.ExternalPort)
	}

	// out-of-range rejected
	postForm(t, h, base+"/external-port", cookie, url.Values{"external_port": {"70000"}})
	if got, _ := q.GetDBInstance(ctx, inst.ID); got.ExternalPort != nil {
		t.Fatalf("bad port must not persist, got %v", *got.ExternalPort)
	}
}
