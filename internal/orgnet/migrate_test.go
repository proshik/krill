package orgnet

import (
	"context"
	"errors"
	"testing"
)

type fakeStore struct {
	orgs      []Org
	apps      map[int64][]int64
	instances map[int64][]int64
	written   map[int64]string
	listErr   error
}

func (f *fakeStore) ListOrganizations(context.Context) ([]Org, error) { return f.orgs, f.listErr }
func (f *fakeStore) SetOrganizationNetwork(_ context.Context, orgID int64, net string) error {
	if f.written == nil {
		f.written = map[int64]string{}
	}
	f.written[orgID] = net
	return nil
}
func (f *fakeStore) ListAppIDsByOrg(_ context.Context, orgID int64) ([]int64, error) {
	return f.apps[orgID], nil
}
func (f *fakeStore) ListInstanceIDsByOrg(_ context.Context, orgID int64) ([]int64, error) {
	return f.instances[orgID], nil
}

func TestMigratorMovesOnlyUnmigratedOrgs(t *testing.T) {
	st := &fakeStore{
		orgs:      []Org{{ID: 1, NetworkName: ""}, {ID: 2, NetworkName: "krill-org-2"}},
		apps:      map[int64][]int64{1: {10}},
		instances: map[int64][]int64{1: {20}},
	}
	var ensured []string
	var order []string
	m := &Migrator{
		Store:            st,
		EnsureNetwork:    func(_ context.Context, n string) error { ensured = append(ensured, n); return nil },
		RedeployInstance: func(_ context.Context, id int64) error { order = append(order, "db"); return nil },
		RedeployApp:      func(_ context.Context, id int64) error { order = append(order, "app"); return nil },
	}

	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(ensured) != 2 {
		t.Fatalf("both networks must be ensured, got %v", ensured)
	}
	if st.written[1] != "krill-org-1" || len(st.written) != 1 {
		t.Fatalf("only the unmigrated org is written: %v", st.written)
	}
	if len(order) != 2 || order[0] != "db" || order[1] != "app" {
		t.Fatalf("databases must move before apps, got %v", order)
	}

	// Idempotent: a second run has nothing left to do.
	st.orgs[0].NetworkName = "krill-org-1"
	order = nil
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(order) != 0 {
		t.Fatalf("second run must redeploy nothing, got %v", order)
	}
}

// One service that refuses to move must not cost the rest of the pass: the
// organization is already marked migrated, and the next startup would not
// retry it, so everything still reachable has to be attempted now.
func TestMigratorContinuesPastAFailedService(t *testing.T) {
	st := &fakeStore{
		orgs:      []Org{{ID: 1}},
		apps:      map[int64][]int64{1: {10, 11}},
		instances: map[int64][]int64{1: {20, 21}},
	}
	var apps, dbs []int64
	m := &Migrator{
		Store:         st,
		EnsureNetwork: func(context.Context, string) error { return nil },
		RedeployInstance: func(_ context.Context, id int64) error {
			dbs = append(dbs, id)
			if id == 20 {
				return errors.New("boom")
			}
			return nil
		},
		RedeployApp: func(_ context.Context, id int64) error {
			apps = append(apps, id)
			if id == 10 {
				return errors.New("boom")
			}
			return nil
		},
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(dbs) != 2 || len(apps) != 2 {
		t.Fatalf("every service must be attempted, dbs=%v apps=%v", dbs, apps)
	}
}

// A network that cannot be created means the organization has nowhere to move
// to: it must stay marked un-migrated (so the next startup retries) and none of
// its services may be redeployed, or they would land back on the shared network.
func TestMigratorSkipsOrgWhoseNetworkFails(t *testing.T) {
	st := &fakeStore{
		orgs:      []Org{{ID: 1}, {ID: 2}},
		apps:      map[int64][]int64{1: {10}, 2: {20}},
		instances: map[int64][]int64{},
	}
	var moved []int64
	m := &Migrator{
		Store: st,
		EnsureNetwork: func(_ context.Context, n string) error {
			if n == "krill-org-1" {
				return errors.New("boom")
			}
			return nil
		},
		RedeployInstance: func(context.Context, int64) error { return nil },
		RedeployApp:      func(_ context.Context, id int64) error { moved = append(moved, id); return nil },
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, ok := st.written[1]; ok {
		t.Fatalf("an organization with no network must not be marked migrated: %v", st.written)
	}
	if len(moved) != 1 || moved[0] != 20 {
		t.Fatalf("the second organization must still migrate, moved=%v", moved)
	}
}

func TestMigratorReportsAListingFailure(t *testing.T) {
	m := &Migrator{Store: &fakeStore{listErr: errors.New("boom")}}
	if err := m.Run(context.Background()); err == nil {
		t.Fatal("a failed organization listing must be reported")
	}
}
