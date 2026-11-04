package envclone

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/proshik/krill/internal/appmetrics"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/hostshare"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/secret"
)

func (s *Service) publish(ctx context.Context, tx pgx.Tx, r Request, snap Snapshot, intent ResourceIntent, passwords map[int64]string) (int64, error) {
	q := db.New(tx)
	var found int64
	// Row locks retain source identity/dependencies until commit; source values
	// are never recopied here. Project/environment UI deletion also shares a
	// short-lived process operation lock with Clone around its external actions.
	if err := tx.QueryRow(ctx, `SELECT e.id FROM environments e JOIN projects p ON p.id=e.project_id
 WHERE e.id=$1 AND p.id=$2 AND p.organization_id=$3 FOR SHARE OF e,p`, r.SourceID, r.ProjectID, r.OrgID).Scan(&found); err != nil {
		return 0, metadataError(err)
	}
	var role string
	if err := tx.QueryRow(ctx, "SELECT role FROM members WHERE organization_id=$1 AND user_id=$2 FOR SHARE", r.OrgID, r.UserID).Scan(&role); err != nil {
		return 0, metadataError(err)
	}
	if role != "admin" && role != "owner" {
		return 0, ErrNotFound
	}

	for _, d := range snap.Databases {
		if err := tx.QueryRow(ctx, `SELECT ld.id FROM logical_databases ld JOIN db_instances i ON i.id=ld.instance_id
   WHERE ld.id=$1 AND ld.environment_id=$2 AND i.id=$3 AND i.organization_id=$4 AND i.app_name=$5
   FOR SHARE OF ld,i`, d.SourceID, r.SourceID, d.InstanceID, r.OrgID, d.InstanceAppName).Scan(&found); err != nil {
			return 0, metadataError(err)
		}
	}
	for _, in := range snap.Instances {
		if err := tx.QueryRow(ctx, "SELECT id FROM db_instances WHERE id=$1 AND organization_id=$2 AND app_name=$3 FOR SHARE", in.SourceID, r.OrgID, in.AppName).Scan(&found); err != nil {
			return 0, metadataError(err)
		}
	}
	for _, a := range snap.Apps {
		if err := tx.QueryRow(ctx, "SELECT id FROM applications WHERE id=$1 AND environment_id=$2 FOR SHARE", a.SourceID, r.SourceID).Scan(&found); err != nil {
			return 0, metadataError(err)
		}
		if a.Config.RegistryID != nil {
			if err := tx.QueryRow(ctx, "SELECT id FROM registries WHERE id=$1 AND organization_id=$2 FOR SHARE", *a.Config.RegistryID, r.OrgID).Scan(&found); err != nil {
				return 0, metadataError(err)
			}
		}
		if a.Config.GitCredentialID != nil {
			if err := tx.QueryRow(ctx, "SELECT id FROM git_credentials WHERE id=$1 AND organization_id=$2 FOR SHARE", *a.Config.GitCredentialID, r.OrgID).Scan(&found); err != nil {
				return 0, metadataError(err)
			}
		}
	}
	env, err := q.CreateEnvironment(ctx, db.CreateEnvironmentParams{ProjectID: r.ProjectID, Name: r.Name, Slug: org.Slugify(r.Name)})
	if err != nil {
		return 0, err
	}
	dbIDs, instanceIDs := map[int64]int64{}, map[int64]int64{}
	for i, src := range snap.Databases {
		d := intent.Databases[i].Database
		row, err := q.CreateLogicalDatabase(ctx, db.CreateLogicalDatabaseParams{EnvironmentID: env.ID, InstanceID: src.InstanceID, Name: src.Name, DbName: d.DBName, Username: d.Username, Password: secret.Enc(passwords[src.SourceID])})
		if err != nil {
			return 0, err
		}
		dbIDs[src.SourceID] = row.ID
	}
	for _, src := range snap.Instances {
		pw, err := appmetrics.NewToken()
		if err != nil {
			return 0, err
		}
		// New configs only. No engine call, borrowed credentials or external ports.
		name := src.Name + " (" + r.Name + "-" + strconv.FormatInt(env.ID, 10) + ")"
		row, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{OrganizationID: r.OrgID, Engine: src.Engine, Name: name, AppName: "krill-" + src.Engine + "-" + r.Key + "-" + strconv.FormatInt(src.SourceID, 36), Image: src.Image, Superuser: src.Superuser, SuperuserPassword: secret.Enc(pw), NodeHostname: src.NodeHostname})
		if err != nil {
			return 0, err
		}
		instanceIDs[src.SourceID] = row.ID
	}
	for _, src := range snap.Apps {
		a := src.Config
		var id int64
		if err := tx.QueryRow(ctx, "SELECT nextval(pg_get_serial_sequence('applications','id'))").Scan(&id); err != nil {
			return 0, err
		}
		host, err := PrimaryHost(a.Name, id, s.baseDomain)
		if err != nil {
			return 0, err
		}
		if err := hostshare.ReservePrimaryHost(ctx, tx, host); err != nil {
			return 0, err
		}
		_, err = q.CreateCloneApplication(ctx, db.CreateCloneApplicationParams{ID: id, EnvironmentID: env.ID, Name: a.Name, Image: a.Image, Tag: a.Tag, Domain: host, Port: a.Port, EnvText: a.EnvText, SourceType: a.SourceType, GitUrl: a.GitUrl, GitBranch: a.GitBranch, DockerfilePath: a.DockerfilePath})
		if err != nil {
			return 0, err
		}
		if _, err := q.CreateDomain(ctx, db.CreateDomainParams{ApplicationID: id, Host: host, IsPrimary: true}); err != nil {
			return 0, err
		}
		if err := q.UpdateApplicationAdvanced(ctx, db.UpdateApplicationAdvancedParams{ID: id, Command: a.Command, MemoryLimit: a.MemoryLimit, CpuLimit: a.CpuLimit, Replicas: a.Replicas, RestartCondition: a.RestartCondition, RestartMaxAttempts: a.RestartMaxAttempts, HealthcheckCmd: a.HealthcheckCmd, HealthcheckInterval: a.HealthcheckInterval, HealthcheckTimeout: a.HealthcheckTimeout, HealthcheckRetries: a.HealthcheckRetries, HealthcheckStartPeriod: a.HealthcheckStartPeriod, PredeployCommand: a.PredeployCommand}); err != nil {
			return 0, err
		}
		if err := q.UpdateApplicationEnvClassified(ctx, db.UpdateApplicationEnvClassifiedParams{ID: id, EnvText: a.EnvText, EnvPublicKeys: a.EnvPublicKeys}); err != nil {
			return 0, err
		}
		if err := q.UpdateApplicationBuild(ctx, db.UpdateApplicationBuildParams{ID: id, BuildArgs: a.BuildArgs}); err != nil {
			return 0, err
		}
		if err := q.SetApplicationRegistry(ctx, db.SetApplicationRegistryParams{ID: id, RegistryID: a.RegistryID}); err != nil {
			return 0, err
		}
		if err := q.SetApplicationGitCredential(ctx, db.SetApplicationGitCredentialParams{ID: id, GitCredentialID: a.GitCredentialID}); err != nil {
			return 0, err
		}
		if err := q.SetApplicationPlacement(ctx, db.SetApplicationPlacementParams{ID: id, PlacementMode: a.PlacementMode, PlacementNodes: a.PlacementNodes}); err != nil {
			return 0, err
		}
		if err := q.SetApplicationMetricsEnabled(ctx, db.SetApplicationMetricsEnabledParams{ID: id, MetricsEnabled: a.MetricsEnabled}); err != nil {
			return 0, err
		}
		if err := q.SetApplicationMetricsTokenEnv(ctx, db.SetApplicationMetricsTokenEnvParams{ID: id, MetricsTokenEnv: a.MetricsTokenEnv}); err != nil {
			return 0, err
		}
		if err := q.SetApplicationMetricsToken(ctx, db.SetApplicationMetricsTokenParams{ID: id, MetricsToken: a.MetricsToken}); err != nil {
			return 0, err
		}
		for _, ep := range src.Metrics {
			if _, err := q.CreateMetricsEndpoint(ctx, db.CreateMetricsEndpointParams{ApplicationID: id, Port: ep.Port, Path: ep.Path, Job: ep.Job}); err != nil {
				return 0, err
			}
		}
		for _, v := range src.Volumes {
			if _, err := q.CreateVolume(ctx, db.CreateVolumeParams{ApplicationID: id, Name: v.Name, MountPath: v.MountPath, Owner: v.Owner}); err != nil {
				return 0, err
			}
		}
		for _, link := range src.Links {
			param := db.CreateDBLinkParams{ApplicationID: id, VarName: link.VarName, Scheme: link.Scheme, Field: link.Field}
			if link.LogicalDatabaseID != nil {
				mapped, ok := dbIDs[*link.LogicalDatabaseID]
				if !ok {
					return 0, ErrNotFound
				}
				param.LogicalDatabaseID = &mapped
			} else if link.InstanceID != nil {
				mapped, ok := instanceIDs[*link.InstanceID]
				if !ok {
					return 0, ErrNotFound
				}
				param.InstanceID = &mapped
			} else {
				return 0, ErrNotFound
			}
			if _, err := q.CreateDBLink(ctx, param); err != nil {
				return 0, err
			}
		}
	}
	return env.ID, nil
}
