package link

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/proshik/krill/internal/testutil"
)

// The relay's list stops at MaxDestinations, so the whole list always fits
// one control line; re-adding one already there is not refused.
func TestDestinationListBound(t *testing.T) {
	st := NewStore(testutil.NewTestDB(t))
	ctx := context.Background()
	if err := st.Init(ctx); err != nil {
		t.Fatal(err)
	}
	var longest []Destination
	for i := 0; i < MaxDestinations; i++ {
		// The longest names DNS allows, as the worst case for the line.
		host := fmt.Sprintf("%02d%s.example.com", i, string(make([]byte, 0)))
		for len(host) < 250 {
			host = "a" + host
		}
		host = host[:63] + "." + host[64:127] + "." + host[128:191] + "." + host[192:]
		d, err := ParseDestination(host, "65535")
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		longest = append(longest, d)
		if _, err := st.addDestination(ctx, d); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	if _, err := st.addDestination(ctx, Destination{"one-more.example.com", 443}); !errors.Is(err, ErrTooManyDestinations) {
		t.Fatalf("past the bound: %v", err)
	}
	if added, err := st.addDestination(ctx, longest[0]); err != nil || added {
		t.Fatalf("re-adding one already there: %v %v", added, err)
	}
	ds, err := st.Destinations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []message{{Type: "lists", Destinations: ds}, {Type: "hello", Proto: ProtoVersion, KrillVersion: "v99.99.99-rc.1+build.1234567890", Egress: true, Destinations: ds}} {
		if n, _ := jsonLen(m); n >= maxMessage {
			t.Fatalf("%s with a full list is %d bytes, over the %d-byte control line", m.Type, n, maxMessage)
		}
	}
}

// Two replacements of one app's selection, starting empty, never merge into a
// set neither of them saved.
func TestSetAppDestinationsSerialized(t *testing.T) {
	pool := testutil.NewTestDB(t)
	st := NewStore(pool)
	ctx := context.Background()
	var appID int64
	err := pool.QueryRow(ctx, `WITH u AS (INSERT INTO users (email, password_hash) VALUES ('race@example.com', 'x') RETURNING id),
		o AS (INSERT INTO organizations (name, slug, owner_id) SELECT 'race', 'race', id FROM u RETURNING id),
		p AS (INSERT INTO projects (organization_id, name, slug) SELECT id, 'p', 'p' FROM o RETURNING id),
		e AS (INSERT INTO environments (project_id, name, slug) SELECT id, 'e', 'e' FROM p RETURNING id)
		INSERT INTO applications (environment_id, name, image, tag, domain, port, source_type, git_url, git_branch, dockerfile_path)
		SELECT id, 'app', 'alpine', '3', 'race.example.com', 80, 'image', '', '', 'Dockerfile' FROM e RETURNING id`).Scan(&appID)
	if err != nil {
		t.Fatal(err)
	}
	a, b := []Destination{tg}, []Destination{oai}
	for round := 0; round < 20; round++ {
		if err := st.setAppDestinations(ctx, appID, nil, nil); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for _, set := range [][]Destination{a, b} {
			wg.Add(1)
			go func(set []Destination) {
				defer wg.Done()
				if err := st.setAppDestinations(ctx, appID, set, nil); err != nil {
					t.Error(err)
				}
			}(set)
		}
		wg.Wait()
		got, err := st.AppDestinations(ctx, appID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, a) && !reflect.DeepEqual(got, b) {
			t.Fatalf("round %d: saved %v, want one of the two sets", round, got)
		}
	}
}
