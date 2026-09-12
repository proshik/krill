package orgnet

import (
	"context"
	"fmt"
	"log/slog"
)

// defaultInstanceBatch bounds how many database instances of one organization
// are redeployed at once. Each one is a Swarm service pulling an image and
// restarting a container; letting a whole organization's databases go at once
// would spike the host right when its apps are queued behind them.
const defaultInstanceBatch = 3

// Org is one organization as the migration sees it.
type Org struct {
	ID          int64
	NetworkName string
	// Migrated is true once network_migrated_at is set — every service of this
	// organization was successfully submitted for a move. NetworkName alone does
	// not say that: it is written BEFORE anything moves, because the deployer
	// reads it to know where to deploy.
	Migrated bool
}

// Store is what the migration needs from the database.
type Store interface {
	ListOrganizations(ctx context.Context) ([]Org, error)
	SetOrganizationNetwork(ctx context.Context, orgID int64, network string) error
	MarkOrganizationMigrated(ctx context.Context, orgID int64) error
	ListAppIDsByOrg(ctx context.Context, orgID int64) ([]int64, error)
	ListInstanceIDsByOrg(ctx context.Context, orgID int64) ([]int64, error)
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

	// RedeployInstance submits one database instance for redeployment. The
	// submission is asynchronous, which is why WaitInstances exists.
	RedeployInstance func(ctx context.Context, id int64) error

	// WaitInstances blocks until the given database instances are running again.
	// Nil skips the wait. A returned error is a warning, not a failed migration:
	// the services were submitted either way.
	WaitInstances func(ctx context.Context, ids []int64) error

	RedeployApp func(ctx context.Context, id int64) error

	// InstanceBatch bounds concurrent database redeployments; <= 0 uses
	// defaultInstanceBatch.
	InstanceBatch int
}

// Run creates the network of every organization and moves the services of those
// not yet migrated. It is idempotent: an organization whose previous pass
// completed is skipped, and one whose pass failed halfway is retried in full on
// the next start. A redundant redeploy costs a restart; a tenant silently left
// on the shared overlay costs the isolation this whole change exists for.
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
	for _, o := range orgs {
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
		if !m.moveServices(ctx, o.ID) {
			slog.Warn("organization network migration incomplete, it will be retried on the next start", "org_id", o.ID)
			continue
		}
		if err := m.Store.MarkOrganizationMigrated(ctx, o.ID); err != nil {
			// Harmless: the next pass redoes the (idempotent) redeploys.
			slog.Error("organization network migration: could not record completion", "err", err, "org_id", o.ID)
		}
	}
	return nil
}

// moveServices redeploys one organization's services into its network and
// reports whether every one of them was submitted successfully.
//
// Databases move first, and the apps wait for them. The networks are isolated
// from each other, so an app that lands in the new network while its database
// is still on the old one cannot resolve it by name: it crash-loops, and
// Swarm's rollback-on-failure then puts that app back on the OLD network and
// marks the deploy failed. Submitting in order is not enough to prevent that —
// database redeployments run in their own goroutines rather than queueing
// behind the app worker — so each batch of databases is waited on before the
// apps are enqueued.
func (m *Migrator) moveServices(ctx context.Context, orgID int64) bool {
	instances, err := m.Store.ListInstanceIDsByOrg(ctx, orgID)
	if err != nil {
		// Fail the whole organization. Moving the apps now would strand them in
		// a network holding none of their databases, with nothing to retry it.
		slog.Error("organization network migration: could not list database instances", "err", err, "org_id", orgID)
		return false
	}
	// Resolve the app set BEFORE anything moves. Both of these can fail, and
	// failing after the databases have left the shared network would strand
	// every app of the organization without its database until the next restart.
	// Asked first, the window does not exist.
	apps, err := m.Store.ListAppIDsByOrg(ctx, orgID)
	if err != nil {
		slog.Error("organization network migration: could not list applications", "err", err, "org_id", orgID)
		return false
	}
	if m.FilterApps != nil {
		apps, err = m.FilterApps(ctx, apps)
		if err != nil {
			slog.Error("organization network migration: could not check which applications are running", "err", err, "org_id", orgID)
			return false
		}
	}

	complete := true
	batch := m.InstanceBatch
	if batch <= 0 {
		batch = defaultInstanceBatch
	}
	for start := 0; start < len(instances); start += batch {
		end := min(start+batch, len(instances))
		submitted := make([]int64, 0, end-start)
		for _, id := range instances[start:end] {
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

	for _, id := range apps {
		if err := m.RedeployApp(ctx, id); err != nil {
			slog.Error("organization network migration: could not redeploy an application", "err", err, "org_id", orgID, "app_id", id)
			complete = false
		}
	}
	return complete
}
