package server_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
)

func TestAddAppPort(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, appID, _, orgID := rpFixture(t, h, q, orgSvc, "ports@k.local")

	// out-of-range host port -> err flash
	if rec := postForm(t, h, base+"/ports", cookie, url.Values{
		"host_port": {"70000"}, "container_port": {"22"}, "protocol": {"tcp"},
	}); rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("bad host port want 303+err, got %d", rec.Code)
	}
	// bad protocol -> err flash
	if rec := postForm(t, h, base+"/ports", cookie, url.Values{
		"host_port": {"2222"}, "container_port": {"22"}, "protocol": {"sctp"},
	}); rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("bad protocol want 303+err, got %d", rec.Code)
	}
	// happy path -> persisted
	if rec := postForm(t, h, base+"/ports", cookie, url.Values{
		"host_port": {"2222"}, "container_port": {"22"}, "protocol": {"tcp"},
	}); rec.Code != http.StatusSeeOther {
		t.Fatalf("add port want 303, got %d body %s", rec.Code, rec.Body.String())
	}
	ports, _ := q.ListAppPorts(ctx, appID)
	if len(ports) != 1 || ports[0].HostPort != 2222 || ports[0].ContainerPort != 22 || ports[0].Protocol != "tcp" {
		t.Fatalf("ports = %+v", ports)
	}
	// duplicate host port (same proto) -> conflict err flash
	if rec := postForm(t, h, base+"/ports", cookie, url.Values{
		"host_port": {"2222"}, "container_port": {"23"}, "protocol": {"tcp"},
	}); rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("dup port want 303+err, got %d", rec.Code)
	}
	// same number but UDP is allowed (distinct protocol)
	if rec := postForm(t, h, base+"/ports", cookie, url.Values{
		"host_port": {"2222"}, "container_port": {"23"}, "protocol": {"udp"},
	}); rec.Code != http.StatusSeeOther || hasErrFlash(rec) {
		t.Fatalf("udp same number want 303 ok, got %d", rec.Code)
	}

	// member is blocked
	memberID := mkUser(t, q, "ports-member@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: orgID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatalf("create member: %v", err)
	}
	mc := loginAs(t, q, "ports-member@k.local")
	if rec := postForm(t, h, base+"/ports", mc, url.Values{
		"host_port": {"3000"}, "container_port": {"3000"}, "protocol": {"tcp"},
	}); rec.Code != http.StatusForbidden {
		t.Errorf("member add port want 403, got %d", rec.Code)
	}
}

func TestAddAppPortConflictsWithDBExternalPort(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, _, _, _ := rpFixture(t, h, q, orgSvc, "ports-db@k.local")
	// CountPostgresByExternalPort is global (no env filter), so the conflicting DB
	// can live in its own org/env — only its external_port matters.
	dbOwner := mkUser(t, q, "ports-db-owner@k.local")
	o2, _ := orgSvc.CreateOrg(ctx, dbOwner, "OrgPortsDB")
	p2, _ := orgSvc.CreateProject(ctx, o2.ID, "P", "")
	e2, _ := orgSvc.CreateEnvironment(ctx, p2.ID, "prod")
	ep := int32(5599)
	if _, err := q.CreatePostgres(ctx, db.CreatePostgresParams{
		EnvironmentID: e2.ID, Name: "db", AppName: "krill-pg-ports",
		DatabaseName: "app", DatabaseUser: "postgres", DatabasePassword: "pw",
		Image: "postgres:17", ExternalPort: &ep,
	}); err != nil {
		t.Fatalf("create postgres: %v", err)
	}
	// TCP host port colliding with the DB external port -> err flash
	if rec := postForm(t, h, base+"/ports", cookie, url.Values{
		"host_port": {"5599"}, "container_port": {"22"}, "protocol": {"tcp"},
	}); rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("db-port conflict want 303+err, got %d", rec.Code)
	}
}

func TestDeleteAppPort(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, appID, _, _ := rpFixture(t, h, q, orgSvc, "ports-del@k.local")
	p, err := q.CreateAppPort(ctx, db.CreateAppPortParams{
		ApplicationID: appID, HostPort: 2222, ContainerPort: 22, Protocol: "tcp",
	})
	if err != nil {
		t.Fatalf("create port: %v", err)
	}
	if rec := postForm(t, h, base+"/ports/"+i64(p.ID)+"/delete", cookie, url.Values{}); rec.Code != http.StatusSeeOther {
		t.Fatalf("delete want 303, got %d", rec.Code)
	}
	if ports, _ := q.ListAppPorts(ctx, appID); len(ports) != 0 {
		t.Fatalf("ports after delete = %d, want 0", len(ports))
	}
}
