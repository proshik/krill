package org

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/testutil"
)

func TestMapUniqueErr(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "23505"}
	if !errors.Is(mapUniqueErr(fmt.Errorf("wrap: %w", pgErr)), ErrSlugTaken) {
		t.Error("23505 wrapped error must map to ErrSlugTaken")
	}
	if mapUniqueErr(nil) != nil {
		t.Error("nil must stay nil")
	}
	other := errors.New("some other error")
	if !errors.Is(mapUniqueErr(other), other) {
		t.Error("non-unique error must pass through unchanged")
	}
	fake := errors.New("log line mentioning 23505 but not a real pg error")
	if errors.Is(mapUniqueErr(fake), ErrSlugTaken) {
		t.Error("must NOT treat arbitrary text containing 23505 as unique violation")
	}
}

// CreateOrg checked the slug and then inserted, so two concurrent creates of the
// same name both passed the check and the loser surfaced a raw
// unique-constraint violation instead of ErrSlugTaken — a 500 where the user
// should have been told the name is taken.
func TestCreateOrgConcurrentSameNameYieldsSlugTaken(t *testing.T) {
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	svc := NewService(q)
	ctx := context.Background()

	u, err := q.CreateUser(ctx, db.CreateUserParams{Email: "race@k.local", PasswordHash: "h"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	const n = 32
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, n)
	oks := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all goroutines at once to widen the check→insert race
			_, err := svc.CreateOrg(ctx, u.ID, "Acme Corp")
			errs[i] = err
			oks[i] = err == nil
		}(i)
	}
	close(start)
	wg.Wait()

	created := 0
	for i := 0; i < n; i++ {
		if oks[i] {
			created++
			continue
		}
		if !errors.Is(errs[i], ErrSlugTaken) {
			t.Errorf("loser %d got %v, want ErrSlugTaken", i, errs[i])
		}
	}
	if created != 1 {
		t.Errorf("%d creates succeeded, want exactly 1", created)
	}

	// Whatever happened, no organization may exist without its owner's
	// membership — that org would be invisible and unmanageable in the UI.
	orgs, err := q.ListOrganizationsForUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("list orgs: %v", err)
	}
	total, err := q.CountOrganizations(ctx)
	if err != nil {
		t.Fatalf("count orgs: %v", err)
	}
	if int64(len(orgs)) != total {
		t.Errorf("%d organizations exist but the owner is a member of %d: an org with no membership was left behind", total, len(orgs))
	}
}
