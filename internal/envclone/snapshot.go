package envclone

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/proshik/krill/internal/builder"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice/drivers"
	"github.com/proshik/krill/internal/docker"
)

var ErrNotFound = errors.New("clone source or dependency not found in tenant")
var ErrCredentialHost = errors.New("clone credential host does not match source host")

// Snapshot contains sanitized values captured in one repeatable-read view.
// Publication must use these values, not reread mutable source configuration.
// It is deliberately not the journal payload: new metrics identities and public
// values belong only in the target configuration, never in recovery metadata.
type Snapshot struct {
	OrgID, ProjectID, SourceID int64
	Apps                       []AppSnapshot
	Databases                  []DatabaseSnapshot
	Instances                  []InstanceSnapshot
	ProtectedDatabases         map[int64][]string
}

type AppSnapshot struct {
	SourceID int64
	Config   db.Application
	Links    []db.ListDBLinksByApplicationRow
	Volumes  []db.AppVolume
	Metrics  []db.AppMetricsEndpoint
}

type DatabaseSnapshot struct {
	SourceID        int64
	InstanceID      int64
	InstanceAppName string
	Name            string
}

type InstanceSnapshot struct {
	SourceID                                              int64
	Engine, Name, AppName, Image, Superuser, NodeHostname string
}

// ReadSnapshot does no Docker work and stores no source credential values.
// The HTTP caller must additionally enforce live admin membership.
func ReadSnapshot(ctx context.Context, pool *pgxpool.Pool, orgID, projectID, sourceID int64) (Snapshot, error) {
	out := Snapshot{OrgID: orgID, ProjectID: projectID, SourceID: sourceID, ProtectedDatabases: map[int64][]string{}}
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Snapshot{}, err
	}
	defer rollback(tx)
	q := db.New(tx)
	project, err := q.GetProject(ctx, projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	if err != nil {
		return Snapshot{}, err
	}
	if project.OrganizationID != orgID {
		return Snapshot{}, ErrNotFound
	}
	source, err := q.GetEnvironment(ctx, sourceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	if err != nil {
		return Snapshot{}, err
	}
	if source.ProjectID != projectID {
		return Snapshot{}, ErrNotFound
	}
	logical, err := q.ListLogicalDatabasesByEnvironment(ctx, sourceID)
	if err != nil {
		return Snapshot{}, err
	}
	known := make(map[int64]bool, len(logical))
	for _, ld := range logical {
		inst, err := q.GetDBInstance(ctx, ld.InstanceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return Snapshot{}, ErrNotFound
		}
		if err != nil {
			return Snapshot{}, err
		}
		if inst.OrganizationID != orgID || inst.Engine != "postgres" {
			return Snapshot{}, ErrNotFound
		}
		if _, ok := out.ProtectedDatabases[inst.ID]; !ok {
			all, err := q.ListLogicalDatabasesByInstance(ctx, inst.ID)
			if err != nil {
				return Snapshot{}, err
			}
			for _, existing := range all {
				out.ProtectedDatabases[inst.ID] = append(out.ProtectedDatabases[inst.ID], existing.DbName)
			}
		}
		known[ld.ID] = true
		out.Databases = append(out.Databases, DatabaseSnapshot{SourceID: ld.ID, InstanceID: inst.ID, InstanceAppName: inst.AppName, Name: ld.Name})
	}
	apps, err := q.ListApplicationsByEnvironment(ctx, sourceID)
	if err != nil {
		return Snapshot{}, err
	}
	instances := map[int64]bool{}
	for _, app := range apps {
		links, err := q.ListDBLinksByApplication(ctx, app.ID)
		if err != nil {
			return Snapshot{}, err
		}
		for _, link := range links {
			if link.InstanceID != nil {
				id := *link.InstanceID
				if !instances[id] {
					inst, err := q.GetDBInstance(ctx, id)
					if errors.Is(err, pgx.ErrNoRows) {
						return Snapshot{}, ErrNotFound
					}
					if err != nil {
						return Snapshot{}, err
					}
					driver, valid := drivers.Registry.Get(inst.Engine)
					if inst.OrganizationID != orgID || !valid || driver.HasLogicalResource() {
						return Snapshot{}, ErrNotFound
					}
					out.Instances = append(out.Instances, InstanceSnapshot{SourceID: id, Engine: inst.Engine, Name: inst.Name, AppName: inst.AppName, Image: inst.Image, Superuser: inst.Superuser, NodeHostname: inst.NodeHostname})
					instances[id] = true
				}
				continue
			}
			if link.LogicalDatabaseID == nil || !known[*link.LogicalDatabaseID] {
				return Snapshot{}, ErrNotFound
			}
		}
		copied, err := CopyApplication(app, links)
		if err != nil {
			return Snapshot{}, err
		}
		if app.RegistryID != nil {
			r, err := q.GetRegistry(ctx, *app.RegistryID)
			if errors.Is(err, pgx.ErrNoRows) {
				return Snapshot{}, ErrNotFound
			}
			if err != nil {
				return Snapshot{}, err
			}
			if r.OrganizationID != orgID {
				return Snapshot{}, ErrNotFound
			}
			want := docker.CanonicalRegistryHost(builder.NormalizeHost(docker.RegistryHost(r.RegistryUrl)))
			if want == "" || want != docker.CanonicalRegistryHost(docker.ImageHost(app.Image)) {
				return Snapshot{}, ErrCredentialHost
			}
		}
		if app.GitCredentialID != nil {
			cred, err := q.GetGitCredential(ctx, *app.GitCredentialID)
			if errors.Is(err, pgx.ErrNoRows) {
				return Snapshot{}, ErrNotFound
			}
			if err != nil {
				return Snapshot{}, err
			}
			if cred.OrganizationID != orgID {
				return Snapshot{}, ErrNotFound
			}
			host, err := builder.HostOf(copied.GitUrl)
			want := builder.NormalizeHost(cred.Host)
			if err != nil || want == "" || host != want {
				return Snapshot{}, ErrCredentialHost
			}
		}
		volumes, err := q.ListVolumesByApplication(ctx, app.ID)
		if err != nil {
			return Snapshot{}, err
		}
		metrics, err := q.ListMetricsEndpointsByApplication(ctx, app.ID)
		if err != nil {
			return Snapshot{}, err
		}
		out.Apps = append(out.Apps, AppSnapshot{SourceID: app.ID, Config: copied, Links: links, Volumes: volumes, Metrics: metrics})
	}
	return out, tx.Commit(ctx)
}
