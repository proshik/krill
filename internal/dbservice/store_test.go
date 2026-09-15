package dbservice

import (
	"context"
	"errors"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/testutil"
)

// An instance whose superuser password cannot be decrypted must fail the
// lookup. Every consumer of Instance.SuperuserPassword either opens a database
// connection with it or shows it to an operator — a silently wrong value turns
// into "authentication failed" somewhere far away, or into a connection string
// the operator copies and cannot use.
func TestGetInstanceFailsOnUndecryptablePassword(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()

	u, err := q.CreateUser(ctx, db.CreateUserParams{Email: "inst@k.local", PasswordHash: "h"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	o, err := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "Org", Slug: "org-inst", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	secret.Init("old-key")
	encPW := secret.Enc("hunter2")
	secret.Init("new-key")
	defer secret.Init("")

	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "db", AppName: "krill-postgres-rot",
		Image: "postgres:16-alpine", Superuser: "postgres", SuperuserPassword: encPW,
	})
	if err != nil {
		t.Fatalf("create db instance: %v", err)
	}

	got, err := NewDBStore(q).GetInstance(ctx, inst.ID)
	if err == nil {
		t.Fatalf("expected an error for an undecryptable password, got instance %+v", got)
	}
	if !errors.Is(err, secret.ErrUndecryptable) {
		t.Errorf("got err %v, want ErrUndecryptable", err)
	}
}

// The happy path: a password stored under the active key round-trips.
func TestGetInstanceDecryptsPassword(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()

	u, err := q.CreateUser(ctx, db.CreateUserParams{Email: "inst-ok@k.local", PasswordHash: "h"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	o, err := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "Org", Slug: "org-inst-ok", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	secret.Init("a-test-key")
	defer secret.Init("")

	inst, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "db", AppName: "krill-postgres-ok",
		Image: "postgres:16-alpine", Superuser: "postgres", SuperuserPassword: secret.Enc("hunter2"),
	})
	if err != nil {
		t.Fatalf("create db instance: %v", err)
	}

	got, err := NewDBStore(q).GetInstance(ctx, inst.ID)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	if got.SuperuserPassword != "hunter2" {
		t.Errorf("password not decrypted: %q", got.SuperuserPassword)
	}
}

// CountMigratingDBInstances counts only instances whose volume is currently
// being moved to another node — the self-updater refuses to restart Krill
// mid-move, since that would abort it and leave the service scaled to 0.
func TestCountMigratingDBInstances(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()

	u, err := q.CreateUser(ctx, db.CreateUserParams{Email: "migrating@k.local", PasswordHash: "h"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	o, err := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "Org", Slug: "org-migrating", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	migrating, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "db-migrating", AppName: "krill-postgres-migrating",
		Image: "postgres:16-alpine", Superuser: "postgres", SuperuserPassword: "hunter2",
	})
	if err != nil {
		t.Fatalf("create migrating db instance: %v", err)
	}
	if err := q.UpdateDBInstanceStatus(ctx, db.UpdateDBInstanceStatusParams{ID: migrating.ID, Status: "migrating"}); err != nil {
		t.Fatalf("set status migrating: %v", err)
	}

	running, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{
		OrganizationID: o.ID, Engine: "postgres", Name: "db-running", AppName: "krill-postgres-running",
		Image: "postgres:16-alpine", Superuser: "postgres", SuperuserPassword: "hunter2",
	})
	if err != nil {
		t.Fatalf("create running db instance: %v", err)
	}
	if err := q.UpdateDBInstanceStatus(ctx, db.UpdateDBInstanceStatusParams{ID: running.ID, Status: "running"}); err != nil {
		t.Fatalf("set status running: %v", err)
	}

	got, err := q.CountMigratingDBInstances(ctx)
	if err != nil {
		t.Fatalf("CountMigratingDBInstances: %v", err)
	}
	if got != 1 {
		t.Errorf("CountMigratingDBInstances = %d, want 1", got)
	}
}
