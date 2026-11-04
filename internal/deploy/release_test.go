package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/testutil"
)

type releaseEngine struct {
	*predeployEngine
	owners      []int
	validateErr error
	afterDeploy func()
	updateState string
}

func (e *releaseEngine) ValidateRelease(context.Context, docker.ServiceSpec, []string) error {
	return e.validateErr
}
func (e *releaseEngine) ServiceDeploy(ctx context.Context, s docker.ServiceSpec) error {
	err := e.digestMockEngine.ServiceDeploy(ctx, s)
	if e.afterDeploy != nil {
		e.afterDeploy()
	}
	return err
}
func (e *releaseEngine) ServiceProgress(context.Context, string, []string) (docker.ServiceProgress, error) {
	return docker.ServiceProgress{Found: true, Desired: 1, Running: 1, UpdateState: e.updateState, TaskIDs: []string{"new"}}, nil
}

func releaseFixture(t *testing.T) (*db.Queries, *DBStore, *Deployer, *releaseEngine, int64) {
	t.Helper()
	secret.Init("release-test-key")
	t.Cleanup(func() { secret.Init("") })
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	ctx := t.Context()
	u, err := q.CreateUser(ctx, db.CreateUserParams{Email: "release@k.local", PasswordHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	o, err := q.CreateOrganization(ctx, db.CreateOrganizationParams{Name: "Org", Slug: "org", OwnerID: u.ID})
	if err != nil {
		t.Fatal(err)
	}
	p, err := q.CreateProject(ctx, db.CreateProjectParams{OrganizationID: o.ID, Name: "P", Slug: "p"})
	if err != nil {
		t.Fatal(err)
	}
	env, err := q.CreateEnvironment(ctx, db.CreateEnvironmentParams{ProjectID: p.ID, Name: "prod", Slug: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	a, err := q.CreateApplication(ctx, db.CreateApplicationParams{EnvironmentID: env.ID, Name: "web", Image: "nginx", Tag: "latest", SourceType: "image", Domain: "release.test", Port: 80, DockerfilePath: "Dockerfile"})
	if err != nil {
		t.Fatal(err)
	}
	if err = q.UpdateApplicationEnvClassified(ctx, db.UpdateApplicationEnvClassifiedParams{ID: a.ID, EnvText: "# v1\nPUBLIC=one\nTOKEN=old-secret", EnvPublicKeys: []string{"PUBLIC"}}); err != nil {
		t.Fatal(err)
	}
	st := NewDBStore(q, pool)
	eng := &releaseEngine{predeployEngine: &predeployEngine{digestMockEngine: &digestMockEngine{digest: "nginx@sha256:" + strings.Repeat("1", 64)}}}
	d := newDeployer(eng, &mockBuilder{}, st)
	d.SetResourceDefaults(64<<20, 100000000)
	return q, st, d, eng, a.ID
}
func runReleaseJob(t *testing.T, d *Deployer, id int64) {
	t.Helper()
	if id == 0 {
		t.Fatal("enqueue failed")
	}
	j := <-d.queue
	d.run(t.Context(), j)
}
func requireDeployment(t *testing.T, q *db.Queries, id int64, status string) {
	t.Helper()
	r, err := q.GetDeployment(t.Context(), id)
	if err != nil || r.Status != status {
		t.Fatalf("deployment: %+v err=%v", r, err)
	}
}

func TestReleaseRollbackRestoresEditorsAndImmutableImage(t *testing.T) {
	q, st, d, e, appID := releaseFixture(t)
	if _, err := st.pool.Exec(t.Context(), "UPDATE applications SET predeploy_command='migrate-v1' WHERE id=$1", appID); err != nil {
		t.Fatal(err)
	}
	v1 := d.Enqueue(appID, TriggerManual)
	runReleaseJob(t, d, v1)
	requireDeployment(t, q, v1, "done")
	snap, err := st.LoadRelease(t.Context(), appID, v1)
	if err != nil {
		t.Fatal(err)
	}
	if err = q.UpdateApplicationEnvClassified(t.Context(), db.UpdateApplicationEnvClassifiedParams{ID: appID, EnvText: "TOKEN=new-value\nPUBLIC=two", EnvPublicKeys: []string{"TOKEN", "PUBLIC"}}); err != nil {
		t.Fatal(err)
	}
	cmd := "different-command"
	if err = q.UpdateApplicationAdvanced(t.Context(), db.UpdateApplicationAdvancedParams{ID: appID, Command: &cmd, Replicas: 2, RestartCondition: "on-failure", RestartMaxAttempts: 3}); err != nil {
		t.Fatal(err)
	}
	e.digest = strings.ReplaceAll(e.digest, "1", "2")
	v2 := d.Enqueue(appID, TriggerManual)
	runReleaseJob(t, d, v2)
	rollback, err := d.EnqueueRollback(t.Context(), appID, v1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.EnqueueRollback(t.Context(), appID, v1); err == nil {
		t.Fatal("duplicate admitted")
	}
	runReleaseJob(t, d, rollback)
	requireDeployment(t, q, rollback, "done")
	restored, err := q.GetApplication(t.Context(), appID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(runtimeSettings(restored), snap.Runtime) {
		t.Fatalf("runtime settings not restored: %+v", runtimeSettings(restored))
	}
	if got := e.deployed[len(e.deployed)-1]; got.Image != snap.Image || got.Env["TOKEN"] != "old-secret" || got.Replicas != 1 || got.MemoryLimitBytes != 64<<20 {
		t.Fatalf("wrong release: %+v", got)
	}
	if len(e.calls) != 1 || e.calls[0].Command != "migrate-v1" || restored.PredeployCommand != "migrate-v1" {
		t.Fatal("rollback ran predeploy")
	}
	// A normal next deploy reads the restored editor values, including secrecy.
	next, err := st.GetApplication(t.Context(), appID)
	if err != nil || next.Env["TOKEN"] != "old-secret" || !reflect.DeepEqual(next.Runtime.EnvPublicKeys, []string{"PUBLIC"}) {
		t.Fatalf("next deploy settings: %+v %v", next.Runtime, err)
	}
	history, err := q.ListDeploymentSummariesByApplication(t.Context(), appID)
	if err != nil || !history[0].RollbackEligible || history[0].RollbackSourceID == nil || *history[0].RollbackSourceID != v1 {
		t.Fatalf("history missing source/eligibility: %+v %v", history, err)
	}
}

func TestReleaseRollbackRefusesQueuedAndInFlightEdits(t *testing.T) {
	for _, stage := range []string{"queued", "rolling"} {
		t.Run(stage, func(t *testing.T) {
			q, _, d, e, id := releaseFixture(t)
			v1 := d.Enqueue(id, TriggerManual)
			runReleaseJob(t, d, v1)
			rollback, err := d.EnqueueRollback(t.Context(), id, v1)
			if err != nil {
				t.Fatal(err)
			}
			edit := func() {
				if err := q.UpdateApplicationEnv(t.Context(), db.UpdateApplicationEnvParams{ID: id, EnvText: "TOKEN=concurrent-edit"}); err != nil {
					t.Fatal(err)
				}
			}
			before := len(e.deployed)
			if stage == "queued" {
				edit()
			} else {
				e.afterDeploy = edit
			}
			runReleaseJob(t, d, rollback)
			requireDeployment(t, q, rollback, "error")
			app, _ := q.GetApplication(t.Context(), id)
			if app.EnvText != "TOKEN=concurrent-edit" {
				t.Fatal("concurrent edit overwritten")
			}
			if stage == "queued" && len(e.deployed) != before {
				t.Fatal("queued edit was not checked before service mutation")
			}
		})
	}
}

func TestReleaseEligibilityAndEncryption(t *testing.T) {
	q, st, d, e, id := releaseFixture(t)
	legacy, _ := q.CreateDeployment(t.Context(), db.CreateDeploymentParams{ApplicationID: id, Trigger: TriggerManual})
	q.FinishDeployment(t.Context(), db.FinishDeploymentParams{ID: legacy.ID, Status: "done"})
	if _, err := d.EnqueueRollback(t.Context(), id, legacy.ID); err == nil {
		t.Fatal("legacy accepted")
	}
	v1 := d.Enqueue(id, TriggerManual)
	runReleaseJob(t, d, v1)
	if _, err := st.LoadRelease(t.Context(), id+1, v1); err == nil {
		t.Fatal("foreign application accepted")
	}
	for _, key := range []string{"", "wrong"} {
		secret.Init(key)
		if _, err := d.EnqueueRollback(t.Context(), id, v1); err == nil {
			t.Fatal("missing/wrong key accepted")
		}
	}
	secret.Init("release-test-key")
	r, _ := st.LoadRelease(t.Context(), id, v1)
	r.Version = 999
	payload, _ := encodeRelease(id, v1, r)
	// Corruption/version rejection is checked without exposing decrypted payloads.
	if _, err := decodeRelease(id, v1, payload); err == nil {
		t.Fatal("future version accepted")
	}
	if _, err := decodeRelease(id, v1, "plaintext"); err == nil {
		t.Fatal("plaintext accepted")
	}
	oldTimeout, oldPoll := convergeTimeout, convergePollInterval
	convergeTimeout = 5 * time.Millisecond
	convergePollInterval = time.Millisecond
	t.Cleanup(func() { convergeTimeout = oldTimeout; convergePollInterval = oldPoll })
	e.updateState = "updating"
	pending := d.Enqueue(id, TriggerManual)
	runReleaseJob(t, d, pending)
	if _, err := st.LoadRelease(t.Context(), id, pending); err == nil {
		t.Fatal("unsettled update eligible")
	}
}

func TestReleaseRollbackDependencyAndAvailabilityGuards(t *testing.T) {
	q, _, d, e, id := releaseFixture(t)
	v1 := d.Enqueue(id, TriggerManual)
	runReleaseJob(t, d, v1)
	// Placement changes reject before queue admission, not after launching a service.
	if err := q.SetApplicationPlacement(t.Context(), db.SetApplicationPlacementParams{ID: id, PlacementMode: "pin", PlacementNodes: "node-other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.EnqueueRollback(t.Context(), id, v1); !errors.Is(err, ErrReleaseDrift) {
		t.Fatalf("placement drift: %v", err)
	}
	q.SetApplicationPlacement(t.Context(), db.SetApplicationPlacementParams{ID: id, PlacementMode: "any"})
	e.validateErr = errors.New("image missing")
	rollback, err := d.EnqueueRollback(t.Context(), id, v1)
	if err != nil {
		t.Fatal(err)
	}
	before := len(e.deployed)
	runReleaseJob(t, d, rollback)
	requireDeployment(t, q, rollback, "error")
	if len(e.deployed) != before {
		t.Fatal("missing image changed service")
	}
}

func TestReleaseDependencyCredentialsAndInfrastructureDrift(t *testing.T) {
	for _, change := range []string{"db-password", "db-link", "metrics", "mount", "port"} {
		t.Run(change, func(t *testing.T) {
			q, st, d, _, id := releaseFixture(t)
			chain, err := q.GetApplicationChain(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			inst, err := q.CreateDBInstance(t.Context(), db.CreateDBInstanceParams{OrganizationID: chain.OrgID, Engine: "redis", Name: "redis", AppName: "krill-redis-release", Image: "redis:7", Superuser: "default", SuperuserPassword: "original-password"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = q.CreateDBLink(t.Context(), db.CreateDBLinkParams{ApplicationID: id, InstanceID: &inst.ID, VarName: "REDIS_URL", Scheme: "redis", Field: "url"})
			if err != nil {
				t.Fatal(err)
			}
			v1 := d.Enqueue(id, TriggerManual)
			runReleaseJob(t, d, v1)
			var query string
			switch change {
			case "db-password":
				query = "UPDATE db_instances SET superuser_password='rotated'"
			case "db-link":
				query = "DELETE FROM app_db_links"
			case "metrics":
				query = "UPDATE applications SET metrics_enabled=true, metrics_token='new-token'"
			case "mount":
				query = fmt.Sprintf("INSERT INTO app_volumes(application_id,name,mount_path) VALUES (%d,'data','/data')", id)
			case "port":
				query = fmt.Sprintf("INSERT INTO app_ports(application_id,host_port,container_port,protocol) VALUES (%d,23567,8080,'tcp')", id)
			}
			if _, err := st.pool.Exec(t.Context(), query); err != nil {
				t.Fatal(err)
			}
			if _, err := d.EnqueueRollback(t.Context(), id, v1); !errors.Is(err, ErrReleaseDrift) {
				t.Fatalf("%s not rejected: %v", change, err)
			}
		})
	}
}

func TestReleaseRuntimePortAndEffectiveUnlimitedRestore(t *testing.T) {
	q, st, d, e, id := releaseFixture(t)
	d.SetResourceDefaults(0, 0)
	first := d.Enqueue(id, TriggerManual)
	runReleaseJob(t, d, first)
	if _, err := st.pool.Exec(t.Context(), "UPDATE applications SET port=9090 WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	d.SetResourceDefaults(128<<20, 1000000000)
	r, err := d.EnqueueRollback(t.Context(), id, first)
	if err != nil {
		t.Fatal(err)
	}
	runReleaseJob(t, d, r)
	requireDeployment(t, q, r, "done")
	got := e.deployed[len(e.deployed)-1]
	if got.MemoryLimitBytes != 0 || got.NanoCPUs != 0 {
		t.Fatal("new defaults changed the old release's effective unlimited limits")
	}
	app, _ := q.GetApplication(t.Context(), id)
	if app.MemoryLimit != nil || app.CpuLimit != nil {
		t.Fatal("raw inherited limits were not restored")
	}
	if app.Port != 80 {
		t.Fatal("runtime port was not restored")
	}
	next, err := st.GetApplication(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	nextSpec := d.buildSpec(next, "nginx@sha256:"+strings.Repeat("1", 64))
	if nextSpec.MemoryLimitBytes != 128<<20 || nextSpec.NanoCPUs != 1000000000 {
		t.Fatal("next deploy did not apply current defaults to restored raw fields")
	}

}

func TestReleaseWithoutKeyAndOrganizationCap(t *testing.T) {
	q, st, d, _, id := releaseFixture(t)
	first := d.Enqueue(id, TriggerManual)
	runReleaseJob(t, d, first)
	secret.Init("")
	unrecorded := d.Enqueue(id, TriggerManual)
	runReleaseJob(t, d, unrecorded)
	requireDeployment(t, q, unrecorded, "done")
	secret.Init("release-test-key")
	if _, err := st.LoadRelease(t.Context(), id, unrecorded); err == nil {
		t.Fatal("unencrypted release snapshot stored")
	}
	a, _ := q.GetApplication(t.Context(), id)
	other, err := q.CreateApplication(t.Context(), db.CreateApplicationParams{EnvironmentID: a.EnvironmentID, Name: "other", Domain: "other-cap.test", Image: "nginx", Tag: "latest", SourceType: "image", Port: 80})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateDeployment(t.Context(), db.CreateDeploymentParams{ApplicationID: other.ID, Trigger: TriggerManual}); err != nil {
		t.Fatal(err)
	}
	d.SetMaxBuildsPerOrg(1)
	if _, err := d.EnqueueRollback(t.Context(), id, first); err == nil {
		t.Fatal("rollback bypassed organization cap")
	}
}

func TestReleaseRawZeroStillInheritsDefaults(t *testing.T) {
	_, st, d, _, id := releaseFixture(t)
	if _, err := st.pool.Exec(t.Context(), "UPDATE applications SET memory_limit='0', cpu_limit='0' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	d.SetResourceDefaults(128<<20, 1000000000)
	a, err := st.GetApplication(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	got := d.buildSpec(a, "nginx")
	if got.MemoryLimitBytes != 128<<20 || got.NanoCPUs != 1000000000 {
		t.Fatal("existing zero limits no longer inherit defaults")
	}
}

func TestConfirmReleasePreservesConcurrentEditor(t *testing.T) {
	q, st, d, _, id := releaseFixture(t)
	source := d.Enqueue(id, TriggerManual)
	runReleaseJob(t, d, source)
	snap, err := st.LoadRelease(t.Context(), id, source)
	if err != nil {
		t.Fatal(err)
	}
	current, err := st.GetApplication(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	destination, err := q.CreateDeployment(t.Context(), db.CreateDeploymentParams{ApplicationID: id, Trigger: TriggerManual})
	if err != nil {
		t.Fatal(err)
	}
	if err = st.SaveRelease(t.Context(), id, destination.ID, snap, source); err != nil {
		t.Fatal(err)
	}
	if err = q.UpdateApplicationEnv(t.Context(), db.UpdateApplicationEnvParams{ID: id, EnvText: "CONCURRENT=preserved"}); err != nil {
		t.Fatal(err)
	}
	if err = st.ConfirmRelease(t.Context(), id, destination.ID, &current.Runtime, &snap.Runtime); !errors.Is(err, ErrReleaseDrift) {
		t.Fatalf("stale restore: %v", err)
	}
	a, err := q.GetApplication(t.Context(), id)
	if err != nil || a.EnvText != "CONCURRENT=preserved" {
		t.Fatalf("editor overwritten: %v", err)
	}
	if _, err = st.LoadRelease(t.Context(), id, destination.ID); err == nil {
		t.Fatal("unconfirmed destination available")
	}
	var confirmed bool
	if err = st.pool.QueryRow(t.Context(), "SELECT confirmed FROM deployment_releases WHERE deployment_id=$1", destination.ID).Scan(&confirmed); err != nil {
		t.Fatal(err)
	}
	if confirmed {
		t.Fatal("stale destination confirmed")
	}
}

func TestRollbackPreparesRestoredVolumeOwner(t *testing.T) {
	q, st, d, e, id := releaseFixture(t)
	if _, err := st.pool.Exec(t.Context(), "INSERT INTO app_volumes(application_id,name,mount_path,owner) VALUES ($1,'data','/data','1000:1000')", id); err != nil {
		t.Fatal(err)
	}
	source := d.Enqueue(id, TriggerManual)
	runReleaseJob(t, d, source)
	requireDeployment(t, q, source, "done")
	setOwner := func(owner string) {
		t.Helper()
		if _, err := st.pool.Exec(t.Context(), "UPDATE app_volumes SET owner=$1 WHERE application_id=$2", owner, id); err != nil {
			t.Fatal(err)
		}
	}
	setOwner("2000:2000")
	next := d.Enqueue(id, TriggerManual)
	runReleaseJob(t, d, next)
	requireDeployment(t, q, next, "done")
	setOwner("1000:1000")
	rollback, err := d.EnqueueRollback(t.Context(), id, source)
	if err != nil {
		t.Fatal(err)
	}
	runReleaseJob(t, d, rollback)
	requireDeployment(t, q, rollback, "done")
	if !reflect.DeepEqual(e.owners, []int{1000, 2000, 1000}) {
		t.Fatalf("volume ownership preparations: %v", e.owners)
	}
}

func (e *releaseEngine) VolumeChown(_ context.Context, _ string, uid, gid int, _ string) error {
	e.owners = append(e.owners, uid)
	return nil
}

func (e *releaseEngine) RunPredeploy(_ context.Context, s docker.PredeploySpec, out io.Writer) error {
	e.calls = append(e.calls, s)
	_, err := io.WriteString(out, "migration executed\n")
	return errors.Join(err, e.err)
}
