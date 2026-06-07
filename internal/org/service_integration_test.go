package org_test

import (
	"context"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/testutil"
)

func setup(t *testing.T) (*org.Service, *db.Queries, int64) {
	t.Helper()
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	u, err := q.CreateUser(context.Background(), db.CreateUserParams{Email: "u@k.local", PasswordHash: "h"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return org.NewService(q), q, u.ID
}

func TestCreateOrgAndMembership(t *testing.T) {
	svc, _, uid := setup(t)
	ctx := context.Background()

	o, err := svc.CreateOrg(ctx, uid, "Default")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if o.Slug != "default" {
		t.Errorf("slug = %q", o.Slug)
	}
	role, ok := svc.Membership(ctx, uid, o.ID)
	if !ok || role.String() != "owner" {
		t.Errorf("membership = %v %v", role, ok)
	}
	// a stranger is not a member
	if _, ok := svc.Membership(ctx, uid+999, o.ID); ok {
		t.Error("stranger must not be a member")
	}
}

func TestChainIsolation(t *testing.T) {
	svc, q, uid := setup(t)
	ctx := context.Background()
	o, _ := svc.CreateOrg(ctx, uid, "Org A")
	p, _ := svc.CreateProject(ctx, o.ID, "Proj", "")
	e, _ := svc.CreateEnvironment(ctx, p.ID, "production")
	a, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "web", Image: "nginx", Tag: "alpine",
		Domain: "web.x", Port: 80, SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}

	// valid chain
	if _, err := svc.AppInChain(ctx, o.ID, p.ID, e.ID, a.ID); err != nil {
		t.Fatalf("valid chain rejected: %v", err)
	}
	// tampered orgID
	if _, err := svc.AppInChain(ctx, o.ID+999, p.ID, e.ID, a.ID); err == nil {
		t.Error("wrong orgID must be rejected")
	}
	// tampered projID
	if _, err := svc.AppInChain(ctx, o.ID, p.ID+999, e.ID, a.ID); err == nil {
		t.Error("wrong projID must be rejected")
	}
}

func TestProjectSlugCollision(t *testing.T) {
	svc, _, uid := setup(t)
	ctx := context.Background()
	o, _ := svc.CreateOrg(ctx, uid, "Org")
	if _, err := svc.CreateProject(ctx, o.ID, "My App", ""); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := svc.CreateProject(ctx, o.ID, "My App", ""); err != org.ErrSlugTaken {
		t.Errorf("expected ErrSlugTaken, got %v", err)
	}
}
