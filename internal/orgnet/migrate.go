package orgnet

import (
	"context"
	"fmt"
	"log/slog"
)

// Org is one organization as the migration sees it: its id, and the overlay
// network already recorded for it. An empty NetworkName means the organization
// predates per-organization networks — its services are still on the single
// shared overlay.
type Org struct {
	ID          int64
	NetworkName string
}

// Store is what the migration needs from the database.
type Store interface {
	ListOrganizations(ctx context.Context) ([]Org, error)
	SetOrganizationNetwork(ctx context.Context, orgID int64, network string) error
	ListAppIDsByOrg(ctx context.Context, orgID int64) ([]int64, error)
	ListInstanceIDsByOrg(ctx context.Context, orgID int64) ([]int64, error)
}

// Migrator moves an existing installation off the single shared overlay network
// and onto one network per organization. A fresh binary on an old install finds
// every service — every tenant's — sharing one network, where any container can
// resolve and reach any other by name; nothing but a redeploy moves a Swarm
// service between networks.
//
// The redeploy functions are injected rather than taken as services so this
// package stays a leaf: the deployer and the DB-instance service both already
// read the organization network from the store, so redeploying is all it takes
// to land a service in the right place.
type Migrator struct {
	Store            Store
	EnsureNetwork    func(ctx context.Context, name string) error
	RedeployInstance func(ctx context.Context, id int64) error
	RedeployApp      func(ctx context.Context, id int64) error
}

// Run creates the network of every organization and redeploys the services of
// those not yet migrated. It is idempotent: an organization with a recorded
// network is left alone, so a second run (the next startup) does nothing.
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
		if o.NetworkName != "" {
			continue // already migrated
		}
		if err := m.Store.SetOrganizationNetwork(ctx, o.ID, name); err != nil {
			// Without the stored name a redeploy would put the service straight
			// back on the shared network, so skip the organization entirely and
			// let the next startup retry it.
			slog.Error("organization network migration: could not record the network", "err", err, "org_id", o.ID, "network", name)
			continue
		}
		slog.Info("migrating an organization onto its own network", "org_id", o.ID, "network", name)
		m.moveServices(ctx, o.ID)
	}
	return nil
}

// moveServices redeploys one organization's services into its network.
//
// Databases move before apps. The networks are isolated from each other, so an
// app that lands in the new network while its database is still on the old one
// cannot resolve it by name and will crash-loop until the database follows.
// Submitting the databases first — the app deploys queue up behind a single
// worker — means an app only ever arrives in a network its databases are
// already headed for. The reverse order would break every app in the
// organization for the length of the migration.
func (m *Migrator) moveServices(ctx context.Context, orgID int64) {
	instances, err := m.Store.ListInstanceIDsByOrg(ctx, orgID)
	if err != nil {
		slog.Error("organization network migration: could not list database instances", "err", err, "org_id", orgID)
	}
	for _, id := range instances {
		if err := m.RedeployInstance(ctx, id); err != nil {
			// One service that will not move must not cost the rest of the pass:
			// the organization is already marked migrated, so nothing retries it.
			slog.Error("organization network migration: could not redeploy a database instance", "err", err, "org_id", orgID, "instance_id", id)
		}
	}
	apps, err := m.Store.ListAppIDsByOrg(ctx, orgID)
	if err != nil {
		slog.Error("organization network migration: could not list applications", "err", err, "org_id", orgID)
		return
	}
	for _, id := range apps {
		if err := m.RedeployApp(ctx, id); err != nil {
			slog.Error("organization network migration: could not redeploy an application", "err", err, "org_id", orgID, "app_id", id)
		}
	}
}
