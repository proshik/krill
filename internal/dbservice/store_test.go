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
