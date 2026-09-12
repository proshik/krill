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
	marked    map[int64]bool
	listErr   error
	instErr   error
	appErr    error
}

func (f *fakeStore) ListOrganizations(context.Context) ([]Org, error) { return f.orgs, f.listErr }
func (f *fakeStore) SetOrganizationNetwork(_ context.Context, orgID int64, net string) error {
	if f.written == nil {
		f.written = map[int64]string{}
	}
	f.written[orgID] = net
	return nil
}
func (f *fakeStore) MarkOrganizationMigrated(_ context.Context, orgID int64) error {
	if f.marked == nil {
		f.marked = map[int64]bool{}
	}
	f.marked[orgID] = true
	return nil
}
func (f *fakeStore) ListAppIDsByOrg(_ context.Context, orgID int64) ([]int64, error) {
	return f.apps[orgID], f.appErr
}
func (f *fakeStore) ListInstanceIDsByOrg(_ context.Context, orgID int64) ([]int64, error) {
	return f.instances[orgID], f.instErr
}

func TestMigratorMovesOnlyUnmigratedOrgs(t *testing.T) {
	st := &fakeStore{
		orgs:      []Org{{ID: 1, NetworkName: ""}, {ID: 2, NetworkName: "krill-org-2", Migrated: true}},
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
	if !st.marked[1] || len(st.marked) != 1 {
		t.Fatalf("a completed pass must be recorded, got %v", st.marked)
	}

	// Idempotent: a second run has nothing left to do.
	st.orgs[0].NetworkName = "krill-org-1"
	st.orgs[0].Migrated = true
	order = nil
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(order) != 0 {
		t.Fatalf("second run must redeploy nothing, got %v", order)
	}
}

// A recorded network name is not proof that anything moved: it is written
// BEFORE the services are redeployed, because the deployer reads it to know
// where to deploy. Only network_migrated_at says the pass finished, so an
// organization whose pass failed halfway must run again in full.
func TestMigratorRetriesAnIncompletePass(t *testing.T) {
	st := &fakeStore{
		orgs:      []Org{{ID: 1}},
		apps:      map[int64][]int64{1: {10, 11}},
		instances: map[int64][]int64{},
	}
	fail := true
	var moved []int64
	m := &Migrator{
		Store:            st,
		EnsureNetwork:    func(context.Context, string) error { return nil },
		RedeployInstance: func(context.Context, int64) error { return nil },
		RedeployApp: func(_ context.Context, id int64) error {
			moved = append(moved, id)
			if fail && id == 11 {
				return errors.New("boom")
			}
			return nil
		},
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if st.marked[1] {
		t.Fatal("an incomplete pass must not be recorded as migrated")
	}

	// Next start: the network name is now recorded, but the pass was not.
	st.orgs[0].NetworkName = "krill-org-1"
	fail = false
	moved = nil
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(moved) != 2 {
		t.Fatalf("a failed pass must be retried in full, moved=%v", moved)
	}
	if !st.marked[1] {
		t.Fatal("a completed retry must be recorded")
	}
}

// One service that refuses to move must not cost the rest of the pass: every
// other service of the organization still has to be attempted.
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
	if st.marked[1] {
		t.Fatal("a pass with failed services must not be recorded as migrated")
	}
}

// If the database instances cannot even be listed we do not know what an app
// depends on. Moving the apps anyway would put them in a network holding none
// of their databases — unresolvable by name, with nothing left to retry it.
func TestMigratorSkipsAppsWhenInstancesCannotBeListed(t *testing.T) {
	st := &fakeStore{
		orgs:      []Org{{ID: 1}},
		apps:      map[int64][]int64{1: {10, 11}},
		instances: map[int64][]int64{},
		instErr:   errors.New("boom"),
	}
	var moved []int64
	m := &Migrator{
		Store:            st,
		EnsureNetwork:    func(context.Context, string) error { return nil },
		RedeployInstance: func(context.Context, int64) error { return nil },
		RedeployApp:      func(_ context.Context, id int64) error { moved = append(moved, id); return nil },
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(moved) != 0 {
		t.Fatalf("no app may move when the databases are unknown, moved=%v", moved)
	}
	if st.marked[1] {
		t.Fatal("the organization must stay un-migrated so the next start retries it")
	}
}

// A network that cannot be created means the organization has nowhere to move
// to: it must stay un-migrated (so the next startup retries) and none of its
// services may be redeployed, or they would land back on the shared network.
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
		t.Fatalf("an organization with no network must not be written: %v", st.written)
	}
	if st.marked[1] {
		t.Fatal("an organization with no network must not be marked migrated")
	}
	if len(moved) != 1 || moved[0] != 20 {
		t.Fatalf("the second organization must still migrate, moved=%v", moved)
	}
}

// FilterApps is what keeps a stopped app stopped across an upgrade.
func TestMigratorHonoursTheAppFilter(t *testing.T) {
	st := &fakeStore{
		orgs:      []Org{{ID: 1}},
		apps:      map[int64][]int64{1: {10, 11, 12}},
		instances: map[int64][]int64{},
	}
	var moved []int64
	m := &Migrator{
		Store:         st,
		EnsureNetwork: func(context.Context, string) error { return nil },
		FilterApps: func(_ context.Context, ids []int64) ([]int64, error) {
			keep := make([]int64, 0, len(ids))
			for _, id := range ids {
				if id != 11 { // 11 stands in for a stopped app
					keep = append(keep, id)
				}
			}
			return keep, nil
		},
		RedeployInstance: func(context.Context, int64) error { return nil },
		RedeployApp:      func(_ context.Context, id int64) error { moved = append(moved, id); return nil },
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(moved) != 2 || moved[0] != 10 || moved[1] != 12 {
		t.Fatalf("the filtered app must not move, moved=%v", moved)
	}
}

// Databases are submitted in bounded batches, and each batch is waited on
// before the next one starts — and before any app is enqueued.
func TestMigratorWaitsForEachDatabaseBatch(t *testing.T) {
	st := &fakeStore{
		orgs:      []Org{{ID: 1}},
		apps:      map[int64][]int64{1: {10}},
		instances: map[int64][]int64{1: {20, 21, 22, 23}},
	}
	var order []string
	var waited [][]int64
	m := &Migrator{
		Store:            st,
		EnsureNetwork:    func(context.Context, string) error { return nil },
		InstanceBatch:    2,
		RedeployInstance: func(_ context.Context, id int64) error { order = append(order, "db"); return nil },
		WaitInstances: func(_ context.Context, ids []int64) error {
			order = append(order, "wait")
			waited = append(waited, append([]int64(nil), ids...))
			return nil
		},
		RedeployApp: func(context.Context, int64) error { order = append(order, "app"); return nil },
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []string{"db", "db", "wait", "db", "db", "wait", "app"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
	if len(waited) != 2 || len(waited[0]) != 2 || waited[0][0] != 20 || waited[1][0] != 22 {
		t.Fatalf("each batch must be waited on with its own ids, got %v", waited)
	}
}

// A wait that times out is a warning, not a failed submission: the instances
// were handed over, so the organization still counts as migrated and the apps
// still follow.
func TestMigratorTreatsAWaitTimeoutAsAWarning(t *testing.T) {
	st := &fakeStore{
		orgs:      []Org{{ID: 1}},
		apps:      map[int64][]int64{1: {10}},
		instances: map[int64][]int64{1: {20}},
	}
	var moved []int64
	m := &Migrator{
		Store:            st,
		EnsureNetwork:    func(context.Context, string) error { return nil },
		RedeployInstance: func(context.Context, int64) error { return nil },
		WaitInstances:    func(context.Context, []int64) error { return ErrWaitTimeout },
		RedeployApp:      func(_ context.Context, id int64) error { moved = append(moved, id); return nil },
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(moved) != 1 {
		t.Fatalf("the apps must still move after a wait timeout, moved=%v", moved)
	}
	if !st.marked[1] {
		t.Fatal("a wait timeout must not hold the organization back forever")
	}
}

func TestMigratorReportsAListingFailure(t *testing.T) {
	m := &Migrator{Store: &fakeStore{listErr: errors.New("boom")}}
	if err := m.Run(context.Background()); err == nil {
		t.Fatal("a failed organization listing must be reported")
	}
}
