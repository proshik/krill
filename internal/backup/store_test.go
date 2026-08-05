package backup

import (
	"context"
	"errors"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/testutil"
)

// A destination whose S3 keys were encrypted under a key that is no longer
// configured must fail the lookup. Returning the row with empty (or ciphertext)
// credentials sends the backup job off to S3 with garbage: it fails far from
// the cause, or — worse — silently authenticates as nobody.
func TestGetDestinationFailsOnUndecryptableKeys(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()

	u, err := q.CreateUser(ctx, db.CreateUserParams{Email: "dest@k.local", PasswordHash: "h"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	o, err := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "Org", Slug: "org-dest", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	// Store the keys encrypted under one key, then rotate to another.
	secret.Init("old-key")
	ak, sk := secret.Enc("AKIAEXAMPLE"), secret.Enc("s3cret")
	secret.Init("new-key")
	defer secret.Init("")

	d, err := q.CreateDestination(ctx, db.CreateDestinationParams{
		OrganizationID: o.ID, Name: "s3", Endpoint: "http://minio:9000",
		Bucket: "b", Region: "us-east-1", AccessKey: ak, SecretKey: sk,
	})
	if err != nil {
		t.Fatalf("create destination: %v", err)
	}

	got, err := NewDBStore(q).GetDestination(ctx, d.ID)
	if err == nil {
		t.Fatalf("expected an error for undecryptable keys, got destination %+v", got)
	}
	if !errors.Is(err, secret.ErrUndecryptable) {
		t.Errorf("got err %v, want ErrUndecryptable", err)
	}
}

// The happy path must keep working: keys stored under the active key decrypt.
func TestGetDestinationDecryptsKeys(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := context.Background()

	u, err := q.CreateUser(ctx, db.CreateUserParams{Email: "dest-ok@k.local", PasswordHash: "h"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	o, err := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "Org", Slug: "org-dest-ok", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}

	secret.Init("a-test-key")
	defer secret.Init("")

	d, err := q.CreateDestination(ctx, db.CreateDestinationParams{
		OrganizationID: o.ID, Name: "s3", Endpoint: "http://minio:9000",
		Bucket: "b", Region: "us-east-1",
		AccessKey: secret.Enc("AKIAEXAMPLE"), SecretKey: secret.Enc("s3cret"),
	})
	if err != nil {
		t.Fatalf("create destination: %v", err)
	}

	got, err := NewDBStore(q).GetDestination(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDestination: %v", err)
	}
	if got.AccessKey != "AKIAEXAMPLE" || got.SecretKey != "s3cret" {
		t.Errorf("keys not decrypted: %+v", got)
	}
}
