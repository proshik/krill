package orgnet

import (
	"context"
	"errors"
	"testing"
	"time"
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
	// deployStatus is the status reported for a deployment id; an id not in the
	// map reads as "done". A value of "" makes the row disappear (app deleted).
	deployStatus map[int64]string
	// deployReadErrs fails that many DeploymentStatuses calls before answering.
	deployReadErrs int
	deployReads    int
	// onDeployRead runs before every DeploymentStatuses answer, so a test can
	// move a deployment along between polls.
	onDeployRead func(f *fakeStore)
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
func (f *fakeStore) DeploymentStatuses(_ context.Context, ids []int64) (map[int64]string, error) {
	f.deployReads++
	if f.onDeployRead != nil {
		f.onDeployRead(f)
	}
	if f.deployReads <= f.deployReadErrs {
		return nil, errors.New("transient")
	}
	out := map[int64]string{}
	for _, id := range ids {
		st, ok := f.deployStatus[id]
		switch {
		case !ok:
			out[id] = "done"
		case st != "":
			out[id] = st
		}
	}
	return out, nil
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
		RedeployApp:      func(_ context.Context, id int64) (int64, error) { order = append(order, "app"); return id, nil },
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
		RedeployApp: func(_ context.Context, id int64) (int64, error) {
			moved = append(moved, id)
			if fail && id == 11 {
				return 0, errors.New("boom")
			}
			return id, nil
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
		RedeployApp: func(_ context.Context, id int64) (int64, error) {
			apps = append(apps, id)
			if id == 10 {
				return 0, errors.New("boom")
			}
			return id, nil
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
		RedeployApp:      func(_ context.Context, id int64) (int64, error) { moved = append(moved, id); return id, nil },
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
		RedeployApp:      func(_ context.Context, id int64) (int64, error) { moved = append(moved, id); return id, nil },
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
		RedeployApp:      func(_ context.Context, id int64) (int64, error) { moved = append(moved, id); return id, nil },
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
		RedeployApp: func(_ context.Context, id int64) (int64, error) { order = append(order, "app"); return id, nil },
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

// The database redeploy is fire-and-forget — RedeployInstance cannot report a
// refused submission — so the wait is the only evidence the databases actually
// arrived. A wait that fails therefore leaves the organization un-migrated and
// retried next boot. The apps still follow: the databases are on their way to
// the same network, and holding the apps back forever is worse.
func TestMigratorDoesNotMarkTheOrgWhenTheDatabaseWaitFails(t *testing.T) {
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
		RedeployApp:      func(_ context.Context, id int64) (int64, error) { moved = append(moved, id); return id, nil },
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(moved) != 1 {
		t.Fatalf("the apps must still move after a wait failure, moved=%v", moved)
	}
	if st.marked[1] {
		t.Fatal("a failed database wait must leave the organization to be retried")
	}
}

// Listing and filtering the applications can both fail. Doing either after the
// databases have already left the shared network would strand every app of the
// organization without its database until the next restart, so the app set is
// resolved first and a failure costs nothing.
func TestMigratorResolvesAppsBeforeMovingDatabases(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store func() *fakeStore
		filt  func(context.Context, []int64) ([]int64, error)
	}{
		{
			name: "listing fails",
			store: func() *fakeStore {
				return &fakeStore{
					orgs:      []Org{{ID: 1}},
					apps:      map[int64][]int64{1: {10}},
					instances: map[int64][]int64{1: {20, 21}},
					appErr:    errors.New("boom"),
				}
			},
		},
		{
			name: "filtering fails",
			store: func() *fakeStore {
				return &fakeStore{
					orgs:      []Org{{ID: 1}},
					apps:      map[int64][]int64{1: {10}},
					instances: map[int64][]int64{1: {20, 21}},
				}
			},
			filt: func(context.Context, []int64) ([]int64, error) { return nil, errors.New("boom") },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := tc.store()
			var dbs, apps []int64
			m := &Migrator{
				Store:            st,
				EnsureNetwork:    func(context.Context, string) error { return nil },
				FilterApps:       tc.filt,
				RedeployInstance: func(_ context.Context, id int64) error { dbs = append(dbs, id); return nil },
				RedeployApp:      func(_ context.Context, id int64) (int64, error) { apps = append(apps, id); return id, nil },
			}
			if err := m.Run(context.Background()); err != nil {
				t.Fatalf("run: %v", err)
			}
			if len(dbs) != 0 {
				t.Fatalf("no database may move before the app set is known, moved=%v", dbs)
			}
			if len(apps) != 0 {
				t.Fatalf("no app may move either, moved=%v", apps)
			}
			if st.marked[1] {
				t.Fatal("the organization must stay un-migrated so the next start retries it")
			}
		})
	}
}

func TestMigratorReportsAListingFailure(t *testing.T) {
	m := &Migrator{Store: &fakeStore{listErr: errors.New("boom")}}
	if err := m.Run(context.Background()); err == nil {
		t.Fatal("a failed organization listing must be reported")
	}
}

// Submitting a deploy proves nothing: the deploy runs later on the worker, and
// one that fails never reaches ServiceDeploy (or is rolled back by Swarm), so
// that app keeps running on the OLD shared network. Flagging the organization
// migrated anyway would bless exactly that state and no later start would ever
// retry it. The next start, where the deploy succeeds, records the pass.
func TestMigratorDoesNotMarkTheOrgWhenAnAppDeploymentFails(t *testing.T) {
	st := &fakeStore{
		orgs:         []Org{{ID: 1}},
		apps:         map[int64][]int64{1: {10, 11}},
		instances:    map[int64][]int64{},
		deployStatus: map[int64]string{101: "error"},
	}
	m := &Migrator{
		Store:            st,
		EnsureNetwork:    func(context.Context, string) error { return nil },
		RedeployInstance: func(context.Context, int64) error { return nil },
		// The deployment id differs from the app id, so the test proves the
		// migrator polls what RedeployApp returned rather than the app ids.
		RedeployApp: func(_ context.Context, id int64) (int64, error) { return id + 90, nil },
		DeployPoll:  time.Millisecond,
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if st.marked[1] {
		t.Fatal("an organization whose app deployment failed must not be recorded as migrated")
	}

	st.orgs[0].NetworkName = "krill-org-1"
	st.deployStatus = nil
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !st.marked[1] {
		t.Fatal("a retry whose deployments succeed must be recorded")
	}
}

// The organization is recorded only once every submitted deployment is
// terminal, not while any of them is still running.
func TestMigratorWaitsForAppDeploymentsToFinish(t *testing.T) {
	st := &fakeStore{
		orgs:         []Org{{ID: 1}},
		apps:         map[int64][]int64{1: {10}},
		instances:    map[int64][]int64{},
		deployStatus: map[int64]string{10: "running"},
	}
	st.onDeployRead = func(f *fakeStore) {
		if f.marked[1] {
			t.Error("the organization was recorded while its deployment was still running")
		}
		if f.deployReads == 3 {
			f.deployStatus[10] = "done"
		}
	}
	m := &Migrator{
		Store:            st,
		EnsureNetwork:    func(context.Context, string) error { return nil },
		RedeployInstance: func(context.Context, int64) error { return nil },
		RedeployApp:      func(_ context.Context, id int64) (int64, error) { return id, nil },
		DeployPoll:       time.Millisecond,
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if st.deployReads < 3 {
		t.Fatalf("the migrator must poll until the deployment is terminal, got %d reads", st.deployReads)
	}
	if !st.marked[1] {
		t.Fatal("a pass whose deployments all finished must be recorded")
	}
}

// The pass deadline bounds the wait, and running out of time leaves the
// organization UNMARKED: a deploy that has not finished has not provably moved
// the app. The organizations after it are not attempted at all, and do not
// each fail on the expired context.
func TestMigratorLeavesTheOrgUnmarkedWhenTheDeadlineExpires(t *testing.T) {
	st := &fakeStore{
		orgs:         []Org{{ID: 1}, {ID: 2}},
		apps:         map[int64][]int64{1: {10}, 2: {20}},
		instances:    map[int64][]int64{},
		deployStatus: map[int64]string{10: "running"},
	}
	var ensured []string
	m := &Migrator{
		Store:            st,
		EnsureNetwork:    func(_ context.Context, n string) error { ensured = append(ensured, n); return nil },
		RedeployInstance: func(context.Context, int64) error { return nil },
		RedeployApp:      func(_ context.Context, id int64) (int64, error) { return id, nil },
		DeployPoll:       time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := m.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	if st.marked[1] {
		t.Fatal("a timed-out wait must leave the organization unmarked")
	}
	if len(ensured) != 1 {
		t.Fatalf("no organization may be attempted after the deadline, ensured=%v", ensured)
	}
}

// One failed read of the deployments table is not a failed deployment.
func TestMigratorToleratesATransientDeploymentReadError(t *testing.T) {
	st := &fakeStore{
		orgs:           []Org{{ID: 1}},
		apps:           map[int64][]int64{1: {10}},
		instances:      map[int64][]int64{},
		deployReadErrs: 2,
	}
	m := &Migrator{
		Store:            st,
		EnsureNetwork:    func(context.Context, string) error { return nil },
		RedeployInstance: func(context.Context, int64) error { return nil },
		RedeployApp:      func(_ context.Context, id int64) (int64, error) { return id, nil },
		DeployPoll:       time.Millisecond,
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !st.marked[1] {
		t.Fatal("a transient read error must not cost the organization its pass")
	}
}

// A deployment row that vanished belongs to an app deleted mid-pass: there is
// nothing left to move, so it neither fails the pass nor holds it open.
func TestMigratorTreatsADeletedDeploymentAsNothingToMove(t *testing.T) {
	st := &fakeStore{
		orgs:         []Org{{ID: 1}},
		apps:         map[int64][]int64{1: {10}},
		instances:    map[int64][]int64{},
		deployStatus: map[int64]string{10: ""},
	}
	m := &Migrator{
		Store:            st,
		EnsureNetwork:    func(context.Context, string) error { return nil },
		RedeployInstance: func(context.Context, int64) error { return nil },
		RedeployApp:      func(_ context.Context, id int64) (int64, error) { return id, nil },
		DeployPoll:       time.Millisecond,
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !st.marked[1] {
		t.Fatal("a deleted app must not keep the organization unmigrated")
	}
}

// Parked (stopped) instances are moved without being redeployed or waited on;
// only the running ones are submitted and waited for.
func TestMigratorParksStoppedInstances(t *testing.T) {
	st := &fakeStore{
		orgs:      []Org{{ID: 1}},
		apps:      map[int64][]int64{1: {10}},
		instances: map[int64][]int64{1: {20, 21}},
	}
	var redeployed []int64
	var parked []ParkedInstance
	var waited []int64
	m := &Migrator{
		Store:         st,
		EnsureNetwork: func(context.Context, string) error { return nil },
		PlanInstances: func(context.Context, []int64) (InstancePlan, error) {
			return InstancePlan{Running: []int64{20}, Parked: []ParkedInstance{{ID: 21, Replicas: 0}}}, nil
		},
		ParkInstance: func(_ context.Context, id int64, replicas uint64) error {
			parked = append(parked, ParkedInstance{ID: id, Replicas: replicas})
			return nil
		},
		RedeployInstance: func(_ context.Context, id int64) error { redeployed = append(redeployed, id); return nil },
		WaitInstances:    func(_ context.Context, ids []int64) error { waited = append(waited, ids...); return nil },
		RedeployApp:      func(_ context.Context, id int64) (int64, error) { return id, nil },
		DeployPoll:       time.Millisecond,
	}
	if err := m.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(redeployed) != 1 || redeployed[0] != 20 {
		t.Fatalf("only the running instance may be redeployed, got %v", redeployed)
	}
	if len(parked) != 1 || parked[0] != (ParkedInstance{ID: 21, Replicas: 0}) {
		t.Fatalf("the stopped instance must be parked at zero replicas, got %v", parked)
	}
	if len(waited) != 1 || waited[0] != 20 {
		t.Fatalf("only the running instance may be waited on, got %v", waited)
	}
	if !st.marked[1] {
		t.Fatal("a pass that moved everything must be recorded")
	}
}

// A stopped instance that could not be moved leaves the organization to be
// retried, and so does a plan that could not be made — before anything moves.
func TestMigratorDoesNotMarkTheOrgWhenInstancesCannotBeMoved(t *testing.T) {
	t.Run("parking fails", func(t *testing.T) {
		st := &fakeStore{orgs: []Org{{ID: 1}}, apps: map[int64][]int64{1: {10}}, instances: map[int64][]int64{1: {21}}}
		m := &Migrator{
			Store:         st,
			EnsureNetwork: func(context.Context, string) error { return nil },
			PlanInstances: func(context.Context, []int64) (InstancePlan, error) {
				return InstancePlan{Parked: []ParkedInstance{{ID: 21}}}, nil
			},
			ParkInstance:     func(context.Context, int64, uint64) error { return errors.New("boom") },
			RedeployInstance: func(context.Context, int64) error { return nil },
			RedeployApp:      func(_ context.Context, id int64) (int64, error) { return id, nil },
			DeployPoll:       time.Millisecond,
		}
		if err := m.Run(context.Background()); err != nil {
			t.Fatalf("run: %v", err)
		}
		if st.marked[1] {
			t.Fatal("a stopped instance left on the shared network must keep the organization unmigrated")
		}
	})
	t.Run("planning fails", func(t *testing.T) {
		st := &fakeStore{orgs: []Org{{ID: 1}}, apps: map[int64][]int64{1: {10}}, instances: map[int64][]int64{1: {20}}}
		var moved int
		m := &Migrator{
			Store:         st,
			EnsureNetwork: func(context.Context, string) error { return nil },
			PlanInstances: func(context.Context, []int64) (InstancePlan, error) {
				return InstancePlan{}, errors.New("boom")
			},
			RedeployInstance: func(context.Context, int64) error { moved++; return nil },
			RedeployApp:      func(_ context.Context, id int64) (int64, error) { moved++; return id, nil },
		}
		if err := m.Run(context.Background()); err != nil {
			t.Fatalf("run: %v", err)
		}
		if moved != 0 || st.marked[1] {
			t.Fatalf("nothing may move and the organization must stay unmigrated, moved=%d marked=%v", moved, st.marked[1])
		}
	})
}
