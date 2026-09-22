package orgnet

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// defaultInstanceBatch bounds how many database instances of one organization
// are redeployed at once. Each one is a Swarm service pulling an image and
// restarting a container; letting a whole organization's databases go at once
// would spike the host right when its apps are queued behind them.
const defaultInstanceBatch = 3

// defaultDeployPoll is how often the migration re-reads the deployments it
// submitted. One worker runs every deploy and a single one takes seconds to
// minutes, so polling faster only costs queries.
const defaultDeployPoll = 2 * time.Second

// finalSweepTimeout bounds the one last read of the submitted deployments
// after the pass deadline has passed. It runs on a context detached from the
// expired one, so an organization whose deploys did finish still gets marked.
const finalSweepTimeout = 10 * time.Second

// Terminal deployment statuses (the deployments.status CHECK).
const (
	deploymentDone  = "done"
	deploymentError = "error"
)

// Org is one organization as the migration sees it.
type Org struct {
	ID          int64
	NetworkName string
	// Migrated is true once network_migrated_at is set — every service of this
	// organization was moved: its database instances came back up and every app
	// deployment finished without an error. NetworkName alone does not say that:
	// it is written BEFORE anything moves, because the deployer reads it to know
	// where to deploy.
	Migrated bool
}

// Store is what the migration needs from the database.
type Store interface {
	ListOrganizations(ctx context.Context) ([]Org, error)
	SetOrganizationNetwork(ctx context.Context, orgID int64, network string) error
	MarkOrganizationMigrated(ctx context.Context, orgID int64) error
	ListAppIDsByOrg(ctx context.Context, orgID int64) ([]int64, error)
	ListInstanceIDsByOrg(ctx context.Context, orgID int64) ([]int64, error)
	// DeploymentStatuses returns the status of each listed deployment. A
	// deployment missing from the map no longer exists.
	DeploymentStatuses(ctx context.Context, ids []int64) (map[int64]string, error)
}

// Migrator moves an existing installation off the single shared overlay network
// and onto one network per organization. A fresh binary on an old install finds
// every service — every tenant's — sharing one network, where any container can
// resolve and reach any other by name; nothing but a redeploy moves a Swarm
// service between networks.
//
// The hooks are injected rather than taken as services so this package stays a
// leaf: the deployer and the DB-instance service both already read the
// organization network from the store, so redeploying is all it takes to land a
// service in the right place.
type Migrator struct {
	Store         Store
	EnsureNetwork func(ctx context.Context, name string) error

	// FilterApps narrows an organization's applications to the ones worth
	// moving. Nil means "move them all". See RunningAppsFilter.
	FilterApps func(ctx context.Context, ids []int64) ([]int64, error)

	// PlanInstances decides how each database instance of an organization moves:
	// redeployed and waited on, or parked (moved without being started). Nil
	// redeploys them all. See RunningInstancesFilter.
	PlanInstances func(ctx context.Context, ids []int64) (InstancePlan, error)

	// ParkInstance synchronously rewrites a stopped instance's service onto its
	// organization network at the given replica count, without starting it.
	// Required whenever PlanInstances can park anything.
	ParkInstance func(ctx context.Context, id int64, replicas uint64) error

	// RedeployInstance submits one database instance for redeployment. The
	// submission is asynchronous, which is why WaitInstances exists.
	RedeployInstance func(ctx context.Context, id int64) error

	// WaitInstances blocks until the given database instances are running again.
	// Nil skips the wait. A returned error is a warning, not a failed migration:
	// the services were submitted either way.
	WaitInstances func(ctx context.Context, ids []int64) error

	// RedeployApp submits one application for redeployment and returns the id
	// of the deployment row it created. Submission is all it proves — the
	// deploy runs later on the worker — so the migration polls that row until
	// it is terminal before counting the app as moved.
	RedeployApp func(ctx context.Context, id int64) (int64, error)

	// InstanceBatch bounds concurrent database redeployments; <= 0 uses
	// defaultInstanceBatch.
	InstanceBatch int

	// DeployPoll is how often submitted app deployments are re-read; <= 0 uses
	// defaultDeployPoll.
	DeployPoll time.Duration
}

// orgPass is one organization's progress through a migration pass.
type orgPass struct {
	orgID int64
	// ok is false once any step failed; the organization is then not marked
	// and the whole pass is retried for it on the next start.
	ok bool
	// pending counts its submitted app deployments not yet terminal.
	pending int
}

// Run creates the network of every organization and moves the services of those
// not yet migrated. It is idempotent: an organization whose previous pass
// completed is skipped, and one whose pass failed halfway is retried in full on
// the next start. A redundant redeploy costs a restart; a tenant silently left
// on the shared overlay costs the isolation this whole change exists for.
//
// It runs in two phases. First every organization's databases are moved and
// its app redeploys submitted; then the deployments of all organizations are
// waited on together, and each organization is marked migrated as soon as its
// own deployments have finished. Waiting organization by organization instead
// let one slow organization — a long build, a deploy that never converges —
// spend the pass deadline on its own wait while every organization after it
// had not even been submitted.
//
// Only a failure to enumerate the organizations is returned. Everything else is
// logged and stepped over: this runs during startup, and a control plane that
// refuses to boot because one service would not redeploy leaves the operator
// with neither a UI nor the logs explaining why.
func (m *Migrator) Run(ctx context.Context) error {
	orgs, err := m.Store.ListOrganizations(ctx)
	if err != nil {
		return fmt.Errorf("list organizations: %w", err)
	}
	var passes []*orgPass
	deployments := map[int64]*orgPass{}
	for i, o := range orgs {
		// Once the pass deadline is gone every call below fails with it, and
		// each remaining organization would log an unrelated-looking error of its
		// own ("could not create the network: context deadline exceeded"). Say
		// what actually happened, once, and still collect what was submitted.
		if err := ctx.Err(); err != nil {
			slog.Warn("organization network migration ran out of time; the remaining organizations will be retried on the next start",
				"err", err, "remaining", len(orgs)-i)
			break
		}
		name := Name(o.ID)
		// Ensure the network even for an already-migrated organization: it is
		// cheap, idempotent, and it repairs an install whose network was removed
		// by hand.
		if err := m.EnsureNetwork(ctx, name); err != nil {
			slog.Error("organization network migration: could not create the network", "err", err, "org_id", o.ID, "network", name)
			continue
		}
		if o.Migrated {
			continue
		}
		if o.NetworkName != name {
			if err := m.Store.SetOrganizationNetwork(ctx, o.ID, name); err != nil {
				// Without the stored name a redeploy would put the service straight
				// back on the shared network, so skip the organization entirely and
				// let the next startup retry it.
				slog.Error("organization network migration: could not record the network", "err", err, "org_id", o.ID, "network", name)
				continue
			}
		}
		slog.Info("migrating an organization onto its own network", "org_id", o.ID, "network", name)
		ok, ids := m.moveServices(ctx, o.ID)
		p := &orgPass{orgID: o.ID, ok: ok, pending: len(ids)}
		for _, id := range ids {
			deployments[id] = p
		}
		passes = append(passes, p)
	}
	for _, p := range passes {
		if p.pending == 0 {
			m.finish(ctx, p)
		}
	}
	m.waitDeployments(ctx, deployments)
	return nil
}

// finish records an organization whose pass is over: marked migrated when
// every step succeeded, otherwise left for the next start. The write runs on a
// context detached from the pass deadline: an organization that did move must
// not lose its record to a deadline that expired because of another one.
func (m *Migrator) finish(ctx context.Context, p *orgPass) {
	if !p.ok {
		slog.Warn("organization network migration incomplete, it will be retried on the next start", "org_id", p.orgID)
		return
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalSweepTimeout)
	defer cancel()
	if err := m.Store.MarkOrganizationMigrated(wctx, p.orgID); err != nil {
		// Harmless: the next pass redoes the (idempotent) redeploys.
		slog.Error("organization network migration: could not record completion", "err", err, "org_id", p.orgID)
	}
}

// moveServices redeploys one organization's services into its network. It
// reports whether every step up to submitting the app deploys succeeded, and
// returns the ids of the app deployments it submitted: those still have to
// finish without an error before the organization counts as moved.
// Submitting is not enough. A deploy that fails never reaches ServiceDeploy (or
// is rolled back by Swarm), so that app keeps running on the OLD shared network;
// counting it as moved would flag the organization migrated and no later start
// would ever retry it.
//
// Databases move first, and the apps wait for them. The networks are isolated
// from each other, so an app that lands in the new network while its database
// is still on the old one cannot resolve it by name: it crash-loops, and
// Swarm's rollback-on-failure then puts that app back on the OLD network and
// marks the deploy failed. Submitting in order is not enough to prevent that —
// database redeployments run in their own goroutines rather than queueing
// behind the app worker — so each batch of databases is waited on before the
// apps are enqueued.
func (m *Migrator) moveServices(ctx context.Context, orgID int64) (bool, []int64) {
	instances, err := m.Store.ListInstanceIDsByOrg(ctx, orgID)
	if err != nil {
		// Fail the whole organization. Moving the apps now would strand them in
		// a network holding none of their databases, with nothing to retry it.
		slog.Error("organization network migration: could not list database instances", "err", err, "org_id", orgID)
		return false, nil
	}
	// Resolve the app set BEFORE anything moves. Both of these can fail, and
	// failing after the databases have left the shared network would strand
	// every app of the organization without its database until the next restart.
	// Asked first, the window does not exist.
	apps, err := m.Store.ListAppIDsByOrg(ctx, orgID)
	if err != nil {
		slog.Error("organization network migration: could not list applications", "err", err, "org_id", orgID)
		return false, nil
	}
	if m.FilterApps != nil {
		apps, err = m.FilterApps(ctx, apps)
		if err != nil {
			slog.Error("organization network migration: could not check which applications are running", "err", err, "org_id", orgID)
			return false, nil
		}
	}
	plan := InstancePlan{Running: instances}
	if m.PlanInstances != nil {
		plan, err = m.PlanInstances(ctx, instances)
		if err != nil {
			slog.Error("organization network migration: could not check which database instances are running", "err", err, "org_id", orgID)
			return false, nil
		}
	}

	complete := true
	// Stopped instances first: parking is synchronous and starts nothing, so it
	// costs the apps behind it no wait.
	for _, p := range plan.Parked {
		if m.ParkInstance == nil {
			slog.Error("organization network migration: no way to move a stopped database instance", "org_id", orgID, "instance_id", p.ID)
			complete = false
			continue
		}
		if err := m.ParkInstance(ctx, p.ID, p.Replicas); err != nil {
			slog.Error("organization network migration: could not move a stopped database instance", "err", err, "org_id", orgID, "instance_id", p.ID)
			complete = false
		}
	}

	batch := m.InstanceBatch
	if batch <= 0 {
		batch = defaultInstanceBatch
	}
	running := plan.Running
	for start := 0; start < len(running); start += batch {
		end := min(start+batch, len(running))
		submitted := make([]int64, 0, end-start)
		for _, id := range running[start:end] {
			if err := m.RedeployInstance(ctx, id); err != nil {
				// One service that will not move must not cost the rest of the
				// pass; the organization simply stays un-migrated and is retried.
				slog.Error("organization network migration: could not redeploy a database instance", "err", err, "org_id", orgID, "instance_id", id)
				complete = false
				continue
			}
			submitted = append(submitted, id)
		}
		if m.WaitInstances == nil || len(submitted) == 0 {
			continue
		}
		if err := m.WaitInstances(ctx, submitted); err != nil {
			// The apps still follow — holding them back forever is worse, and
			// the databases are on their way to the same network. But the
			// organization does NOT count as migrated: RedeployInstance cannot
			// report a refused submission (the DB-instance deploy is
			// fire-and-forget), so this wait is the only evidence the databases
			// actually arrived. Recording completion here would bless exactly
			// the silent partial state the flag exists to prevent; one
			// idempotent retry on the next boot is the whole cost.
			slog.Warn("organization network migration: databases did not come back in time, it will be retried on the next start", "err", err, "org_id", orgID)
			complete = false
		}
	}

	deployments := make([]int64, 0, len(apps))
	for _, id := range apps {
		if ctx.Err() != nil {
			// Out of time: every further submission fails the same way. Run logs
			// the reason once.
			return false, deployments
		}
		depID, err := m.RedeployApp(ctx, id)
		if err != nil {
			slog.Error("organization network migration: could not redeploy an application", "err", err, "org_id", orgID, "app_id", id)
			complete = false
			continue
		}
		deployments = append(deployments, depID)
	}
	return complete, deployments
}

// waitDeployments polls the submitted deployments of every organization until
// each is terminal, finishing an organization as soon as its last one is: an
// error fails it, done counts toward it. It is bounded by ctx — the deadline of
// the whole pass — and an organization with a deployment still unfinished then
// is left unmarked: an app whose deploy has not finished has not provably
// moved, and an unmarked organization costs one idempotent retry on the next
// start. One last read on a detached context follows the deadline, so the
// organizations that did finish in time are still recorded.
//
// A failed read is not a failed deployment; it is logged and polled again, so
// one transient database error does not cost anyone their pass.
func (m *Migrator) waitDeployments(ctx context.Context, pending map[int64]*orgPass) {
	if len(pending) == 0 {
		return
	}
	poll := m.DeployPoll
	if poll <= 0 {
		poll = defaultDeployPoll
	}
	for {
		if ctx.Err() != nil {
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalSweepTimeout)
			m.sweepDeployments(fctx, pending)
			cancel()
			unfinished := map[*orgPass]int{}
			for _, p := range pending {
				unfinished[p]++
			}
			for p, n := range unfinished {
				slog.Warn("organization network migration: application deployments did not finish in time, the organization will be retried on the next start",
					"err", ctx.Err(), "org_id", p.orgID, "unfinished", n)
			}
			return
		}
		m.sweepDeployments(ctx, pending)
		if len(pending) == 0 {
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(poll):
		}
	}
}

// sweepDeployments reads every pending deployment once, drops the terminal
// ones and finishes each organization whose last deployment that was.
func (m *Migrator) sweepDeployments(ctx context.Context, pending map[int64]*orgPass) {
	ids := make([]int64, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	statuses, err := m.Store.DeploymentStatuses(ctx, ids)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("organization network migration: could not read application deployments, retrying", "err", err)
		}
		return
	}
	for _, id := range ids {
		p := pending[id]
		status, found := statuses[id]
		switch {
		case !found:
			// The row is gone, which only happens when its app was deleted
			// mid-pass: nothing is left to move.
		case status == deploymentError:
			slog.Error("organization network migration: an application deployment failed, the organization will be retried on the next start",
				"org_id", p.orgID, "deployment_id", id)
			p.ok = false
		case status == deploymentDone:
		default:
			continue
		}
		delete(pending, id)
		p.pending--
		if p.pending == 0 {
			m.finish(ctx, p)
		}
	}
}
