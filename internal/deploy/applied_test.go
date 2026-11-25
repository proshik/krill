package deploy

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/secret"
)

func fingerprintApp() App {
	return App{
		ID: 7, SourceType: "image", Image: "ghcr.io/o/web", Tag: "1.0.0", Port: 8080,
		Env:             map[string]string{"A": "1", "DB_URL": "postgres://u:p@db/app", "METRICS_TOKEN": "tok"},
		MetricsTokenEnv: "METRICS_TOKEN",
		Args:            []string{"serve"}, Replicas: 1, RestartCondition: "any",
		Mounts:         []docker.MountSpec{{Type: "volume", Source: "v-a", Target: "/a"}, {Type: "volume", Source: "v-b", Target: "/b"}},
		Ports:          []docker.PortSpec{{Target: 22, Published: 2222, Mode: "host"}},
		PlacementMode:  "pin",
		PlacementNodes: []string{"n2", "n1"},
		// Not part of any section: routing and observability apply at once
		// or are not the user's settings.
		Domains: nil, ContainerLabels: map[string]string{"krill.app": "web"},
	}
}

func changedSections(a, b AppliedConfig) []string {
	var out []string
	for _, s := range Sections {
		if a.Sections[s] != b.Sections[s] {
			out = append(out, s)
		}
	}
	return out
}

func TestConfigFingerprintEqualForEqualConfigs(t *testing.T) {
	secret.Init("")
	d := newDeployer(&mockEngine{}, &mockBuilder{}, newFakeStore(imageApp()))
	a := fingerprintApp()
	b := fingerprintApp()
	// The same configuration listed in another order is the same.
	b.Mounts[0], b.Mounts[1] = b.Mounts[1], b.Mounts[0]
	b.PlacementNodes = []string{"n1", "n2"}
	b.Env = map[string]string{"METRICS_TOKEN": "tok", "DB_URL": "postgres://u:p@db/app", "A": "1"}
	// Routing and identity labels are no section.
	b.Domain, b.ContainerLabels, b.Network = "other.example", map[string]string{"krill.app": "renamed"}, "krill-org-9"
	if got := changedSections(d.configFingerprint(a), d.configFingerprint(b)); len(got) != 0 {
		t.Fatalf("equal configurations differ in %v", got)
	}
	fp := d.configFingerprint(a)
	if fp.Version != fingerprintVersion || fp.Key != "sha256" || len(fp.Sections) != len(Sections) {
		t.Fatalf("fingerprint = %+v", fp)
	}
	for _, v := range fp.Sections {
		if strings.Contains(v, "postgres") || len(v) != 64 {
			t.Fatalf("section digest %q", v)
		}
	}
}

func TestConfigFingerprintChangesExactlyOneSection(t *testing.T) {
	secret.Init("")
	d := newDeployer(&mockEngine{}, &mockBuilder{}, newFakeStore(imageApp()))
	cases := []struct {
		name    string
		mutate  func(*App)
		section string
	}{
		{"tag", func(a *App) { a.Tag = "1.0.1" }, SectionSource},
		{"image", func(a *App) { a.Image = "ghcr.io/o/other" }, SectionSource},
		{"env value", func(a *App) { a.Env["A"] = "2" }, SectionEnv},
		{"env added", func(a *App) { a.Env["B"] = "x" }, SectionEnv},
		{"link-injected value", func(a *App) { a.Env["DB_URL"] = "postgres://u:rotated@db/app" }, SectionEnv},
		{"metrics token", func(a *App) { a.Env["METRICS_TOKEN"] = "rotated" }, SectionMetrics},
		{"metrics token variable", func(a *App) {
			a.Env["SCRAPE_TOKEN"] = a.Env["METRICS_TOKEN"]
			delete(a.Env, "METRICS_TOKEN")
			a.MetricsTokenEnv = "SCRAPE_TOKEN"
		}, SectionMetrics},
		{"command", func(a *App) { a.Args = []string{"worker"} }, SectionRuntime},
		{"port", func(a *App) { a.Port = 9090 }, SectionRuntime},
		{"replicas", func(a *App) { a.Replicas = 3 }, SectionRuntime},
		{"memory", func(a *App) { a.MemoryLimitBytes = 128 << 20 }, SectionRuntime},
		{"cpu", func(a *App) { a.NanoCPUs = 5e8 }, SectionRuntime},
		{"restart", func(a *App) { a.RestartCondition = "on-failure" }, SectionRuntime},
		{"restart attempts", func(a *App) { a.RestartMaxAttempts = 5 }, SectionRuntime},
		{"healthcheck", func(a *App) { a.Healthcheck = &docker.HealthcheckSpec{Test: []string{"CMD-SHELL", "true"}} }, SectionRuntime},
		{"predeploy", func(a *App) { a.PredeployCommand = "migrate" }, SectionRuntime},
		{"volume added", func(a *App) {
			a.Mounts = append(a.Mounts, docker.MountSpec{Type: "volume", Source: "v-c", Target: "/c"})
		}, SectionVolumes},
		{"volume owner", func(a *App) { a.Mounts[0].Owner = "1000:1000" }, SectionVolumes},
		{"port published", func(a *App) { a.Ports[0].Published = 2223 }, SectionPorts},
		{"port protocol", func(a *App) { a.Ports[0].UDP = true }, SectionPorts},
		{"placement mode", func(a *App) { a.PlacementMode = "global" }, SectionPlacement},
		{"placement nodes", func(a *App) { a.PlacementNodes = []string{"n1"} }, SectionPlacement},
	}
	base := d.configFingerprint(fingerprintApp())
	for _, c := range cases {
		a := fingerprintApp()
		c.mutate(&a)
		if got := changedSections(base, d.configFingerprint(a)); !reflect.DeepEqual(got, []string{c.section}) {
			t.Errorf("%s: changed %v, want [%s]", c.name, got, c.section)
		}
	}
}

func TestConfigFingerprintDockerfileSource(t *testing.T) {
	secret.Init("")
	d := newDeployer(&mockEngine{}, &mockBuilder{}, newFakeStore(imageApp()))
	base := dockerfileApp()
	base.BuildArgs, base.BuildSecrets = map[string]string{"V": "1"}, map[string]string{"NPM": "s"}
	fp := d.configFingerprint(base)
	for name, mutate := range map[string]func(*App){
		"git url":      func(a *App) { a.GitURL = "https://github.com/x/z.git" },
		"branch":       func(a *App) { a.GitBranch = "dev" },
		"dockerfile":   func(a *App) { a.DockerfilePath = "docker/Dockerfile" },
		"build arg":    func(a *App) { a.BuildArgs = map[string]string{"V": "2"} },
		"build secret": func(a *App) { a.BuildSecrets = map[string]string{"NPM": "t"} },
	} {
		a := base
		mutate(&a)
		if got := changedSections(fp, d.configFingerprint(a)); !reflect.DeepEqual(got, []string{SectionSource}) {
			t.Errorf("%s: changed %v", name, got)
		}
	}
	// Credentials in the URL are not settings of the build.
	a := base
	a.GitURL = "https://" + "user:pw" + "@" + "github.com/x/y.git" // credentials embedded in the URL
	if got := changedSections(fp, d.configFingerprint(a)); len(got) != 0 {
		t.Errorf("credentials in the git URL changed %v", got)
	}
}

func TestConfigFingerprintUsesEffectiveDefaultLimits(t *testing.T) {
	secret.Init("")
	d := newDeployer(&mockEngine{}, &mockBuilder{}, newFakeStore(imageApp()))
	a := fingerprintApp() // no limits of its own
	d.SetResourceDefaults(256<<20, 5e8)
	withDefault := d.configFingerprint(a)
	explicit := a
	explicit.MemoryLimitBytes, explicit.NanoCPUs = 256<<20, 5e8
	if got := changedSections(withDefault, d.configFingerprint(explicit)); len(got) != 0 {
		t.Fatalf("an explicit limit equal to the default differs in %v", got)
	}
	d.SetResourceDefaults(512<<20, 5e8)
	if got := changedSections(withDefault, d.configFingerprint(a)); !reflect.DeepEqual(got, []string{SectionRuntime}) {
		t.Fatalf("a new instance default changed %v, want [runtime]", got)
	}
}

func TestConfigFingerprintKeyedMode(t *testing.T) {
	defer secret.Init("")
	d := newDeployer(&mockEngine{}, &mockBuilder{}, newFakeStore(imageApp()))
	secret.Init("")
	plain := d.configFingerprint(fingerprintApp())
	secret.Init("fingerprint-key")
	keyed := d.configFingerprint(fingerprintApp())
	if keyed.Key == plain.Key || keyed.Sections[SectionEnv] == plain.Sections[SectionEnv] {
		t.Fatal("a keyed fingerprint must differ from the plain one")
	}
	if !reflect.DeepEqual(keyed, d.configFingerprint(fingerprintApp())) {
		t.Fatal("keyed fingerprints must be stable")
	}
	if plain.comparable(keyed) || keyed.comparable(plain) {
		t.Fatal("fingerprints under different keys must not be compared")
	}
	secret.Init("another-key")
	if keyed.comparable(d.configFingerprint(fingerprintApp())) {
		t.Fatal("a rotated key must make old fingerprints unknown, not changed")
	}
}

// configFixture is releaseFixture with a counting engine: every registry
// digest lookup is counted, so an apply can be proven not to resolve a tag.
func configFixture(t *testing.T) (*db.Queries, *DBStore, *Deployer, *releaseEngine, int64, *atomic.Int32) {
	q, st, d, e, appID := releaseFixture(t)
	resolves := &atomic.Int32{}
	e.digestMockEngine.onResolve = func() { resolves.Add(1) }
	return q, st, d, e, appID, resolves
}

func appliedConfigOf(t *testing.T, st *DBStore, id int64) (AppliedConfig, bool) {
	t.Helper()
	c, ok, err := st.DeploymentAppliedConfig(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return c, ok
}

func TestDeployRecordsAppliedConfigOnSuccessOnly(t *testing.T) {
	q, st, d, e, appID, _ := configFixture(t)
	ok1 := d.Enqueue(appID, TriggerManual)
	runReleaseJob(t, d, ok1)
	requireDeployment(t, q, ok1, "done")
	c, found := appliedConfigOf(t, st, ok1)
	if !found || c.Key != secret.FingerprintKeyID() || len(c.Sections) != len(Sections) {
		t.Fatalf("applied config after success = %+v found=%v", c, found)
	}
	// What it stored is the current fingerprint: nothing pending.
	if p := d.PendingChanges(t.Context(), []int64{appID}); p[appID].Any() || len(p) != 1 {
		t.Fatalf("pending right after a deploy = %+v", p)
	}

	e.setUpdateState("rollback_started") // Swarm rolls the update back
	failed := d.Enqueue(appID, TriggerManual)
	runReleaseJob(t, d, failed)
	requireDeployment(t, q, failed, "error")
	if _, found := appliedConfigOf(t, st, failed); found {
		t.Fatal("a failed deploy must record no applied config")
	}
}

func TestPendingChangesAndApplySettings(t *testing.T) {
	q, st, d, e, appID, resolves := configFixture(t)
	ctx := t.Context()
	ident, err := q.GetApplicationIdentity(ctx, appID)
	if err != nil {
		t.Fatal(err)
	}
	redis, err := q.CreateDBInstance(ctx, db.CreateDBInstanceParams{OrganizationID: ident.OrgID, Engine: "redis", Name: "cache", AppName: "krill-redis-cache", Image: "redis:7-alpine", SuperuserPassword: secret.Enc("one")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.CreateDBLink(ctx, db.CreateDBLinkParams{ApplicationID: appID, InstanceID: &redis.ID, VarName: "REDIS_URL", Scheme: "redis", Field: "url"}); err != nil {
		t.Fatal(err)
	}
	first := d.Enqueue(appID, TriggerManual)
	runReleaseJob(t, d, first)
	requireDeployment(t, q, first, "done")
	pinned := e.digest

	// A rotated password of a linked server changes only what the link injects.
	if _, err := st.pool.Exec(ctx, "UPDATE db_instances SET superuser_password=$2 WHERE id=$1", redis.ID, secret.Enc("two")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, "INSERT INTO app_volumes (application_id, name, mount_path) VALUES ($1, 'data', '/data')", appID); err != nil {
		t.Fatal(err)
	}
	p := d.PendingChanges(ctx, []int64{appID})[appID]
	if !reflect.DeepEqual(p.Changed, []string{SectionEnv, SectionVolumes}) || !p.CanApply {
		t.Fatalf("pending = %+v, want env+volumes, applicable", p)
	}

	before := resolves.Load()
	id, err := d.EnqueueApplySettings(ctx, appID, TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	// Queued: nothing is shown while a deploy is in flight.
	if got := d.PendingChanges(ctx, []int64{appID}); len(got) != 0 {
		t.Fatalf("pending while a deploy is queued = %+v", got)
	}
	if _, err := d.EnqueueApplySettings(ctx, appID, TriggerManual); !errors.Is(err, ErrDeployInFlight) {
		t.Fatalf("second apply = %v, want ErrDeployInFlight", err)
	}
	e.digest = "nginx@sha256:" + strings.Repeat("9", 64) // the tag moved meanwhile
	runReleaseJob(t, d, id)
	requireDeployment(t, q, id, "done")
	dep, _ := q.GetDeployment(ctx, id)
	if dep.ImageTag != pinned || dep.Trigger != TriggerManual {
		t.Fatalf("apply deployed %q (%s), want the pinned %q", dep.ImageTag, dep.Trigger, pinned)
	}
	e.mu.Lock()
	last := e.deployed[len(e.deployed)-1]
	e.mu.Unlock()
	if last.Image != pinned || last.Env["REDIS_URL"] == "" || len(last.Mounts) != 1 {
		t.Fatalf("applied spec image=%q env=%v mounts=%v", last.Image, last.Env, last.Mounts)
	}
	if resolves.Load() != before {
		t.Fatal("apply settings must not resolve the tag in the registry")
	}
	if !strings.Contains(dep.Log, "no pull of a tag, no build") {
		t.Fatalf("deploy log does not say what was skipped:\n%s", dep.Log)
	}
	if got := d.PendingChanges(ctx, []int64{appID})[appID]; got.Any() {
		t.Fatalf("pending after apply = %+v", got)
	}
	if _, err := d.EnqueueApplySettings(ctx, appID, TriggerManual); !errors.Is(err, ErrNothingToApply) {
		t.Fatalf("apply with nothing pending = %v", err)
	}

	// A changed tag is a deploy, not an apply.
	if err := q.UpdateApplicationImage(ctx, db.UpdateApplicationImageParams{ID: appID, Image: "nginx", Tag: "next"}); err != nil {
		t.Fatal(err)
	}
	p = d.PendingChanges(ctx, []int64{appID})[appID]
	if !reflect.DeepEqual(p.Changed, []string{SectionSource}) || p.CanApply {
		t.Fatalf("pending after a tag change = %+v", p)
	}
	if _, err := d.EnqueueApplySettings(ctx, appID, TriggerManual); !errors.Is(err, ErrApplyNeedsDeploy) {
		t.Fatalf("apply after a tag change = %v", err)
	}
}

func TestApplySettingsRefusedWhenTheSourceChangesWhileQueued(t *testing.T) {
	q, _, d, _, appID, _ := configFixture(t)
	ctx := t.Context()
	first := d.Enqueue(appID, TriggerManual)
	runReleaseJob(t, d, first)
	if err := q.UpdateApplicationEnvClassified(ctx, db.UpdateApplicationEnvClassifiedParams{ID: appID, EnvText: "PUBLIC=changed", EnvPublicKeys: []string{}}); err != nil {
		t.Fatal(err)
	}
	id, err := d.EnqueueApplySettings(ctx, appID, TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.UpdateApplicationImage(ctx, db.UpdateApplicationImageParams{ID: appID, Image: "nginx", Tag: "next"}); err != nil {
		t.Fatal(err)
	}
	runReleaseJob(t, d, id)
	requireDeployment(t, q, id, "error")
	dep, _ := q.GetDeployment(ctx, id)
	if dep.ErrorMessage != ErrApplyNeedsDeploy.Error() {
		t.Fatalf("error = %q", dep.ErrorMessage)
	}
}

func TestApplySettingsNeedsAPinnedImage(t *testing.T) {
	q, _, d, e, appID, _ := configFixture(t)
	ctx := t.Context()
	e.digestErr = errors.New("registry unreachable") // deployed by plain tag
	first := d.Enqueue(appID, TriggerManual)
	runReleaseJob(t, d, first)
	requireDeployment(t, q, first, "done")
	if err := q.UpdateApplicationEnvClassified(ctx, db.UpdateApplicationEnvClassifiedParams{ID: appID, EnvText: "PUBLIC=changed", EnvPublicKeys: []string{}}); err != nil {
		t.Fatal(err)
	}
	p := d.PendingChanges(ctx, []int64{appID})[appID]
	if !reflect.DeepEqual(p.Changed, []string{SectionEnv}) || p.CanApply {
		t.Fatalf("pending on a tag-deployed app = %+v", p)
	}
	if _, err := d.EnqueueApplySettings(ctx, appID, TriggerManual); !errors.Is(err, ErrApplyNeedsDeploy) {
		t.Fatalf("apply = %v", err)
	}
}

func TestPendingChangesUnknownWithoutAFingerprint(t *testing.T) {
	q, st, d, _, appID, _ := configFixture(t)
	ctx := t.Context()
	first := d.Enqueue(appID, TriggerManual)
	runReleaseJob(t, d, first)
	// As if deployed by a release that recorded nothing.
	if _, err := st.pool.Exec(ctx, "UPDATE deployments SET applied_config=NULL WHERE id=$1", first); err != nil {
		t.Fatal(err)
	}
	if err := q.UpdateApplicationEnvClassified(ctx, db.UpdateApplicationEnvClassifiedParams{ID: appID, EnvText: "PUBLIC=changed", EnvPublicKeys: []string{}}); err != nil {
		t.Fatal(err)
	}
	if got := d.PendingChanges(ctx, []int64{appID}); len(got) != 0 {
		t.Fatalf("pending without a fingerprint = %+v", got)
	}
	if _, err := d.EnqueueApplySettings(ctx, appID, TriggerManual); !errors.Is(err, ErrNothingToApply) {
		t.Fatalf("apply = %v", err)
	}
	// A section missing from a stored fingerprint is unknown, not changed.
	if _, err := st.pool.Exec(ctx, `UPDATE deployments SET applied_config = jsonb_set($2::jsonb, '{sections,env}', '"x"') #- '{sections,source}' WHERE id=$1`, first, mustJSON(t, d.configFingerprint(mustApp(t, st, appID)))); err != nil {
		t.Fatal(err)
	}
	p := d.PendingChanges(ctx, []int64{appID})[appID]
	if !reflect.DeepEqual(p.Changed, []string{SectionEnv}) || p.CanApply {
		t.Fatalf("pending with an unknown source = %+v", p)
	}
}

func TestRollbackRecordsAppliedConfig(t *testing.T) {
	q, st, d, e, appID, _ := configFixture(t)
	ctx := t.Context()
	v1 := d.Enqueue(appID, TriggerManual)
	runReleaseJob(t, d, v1)
	v1Config, _ := appliedConfigOf(t, st, v1)
	if err := q.UpdateApplicationEnvClassified(ctx, db.UpdateApplicationEnvClassifiedParams{ID: appID, EnvText: "PUBLIC=two", EnvPublicKeys: []string{}}); err != nil {
		t.Fatal(err)
	}
	e.digest = strings.ReplaceAll(e.digest, "1", "2")
	v2 := d.Enqueue(appID, TriggerManual)
	runReleaseJob(t, d, v2)
	rb, err := d.EnqueueRollback(ctx, appID, v1)
	if err != nil {
		t.Fatal(err)
	}
	runReleaseJob(t, d, rb)
	requireDeployment(t, q, rb, "done")
	c, found := appliedConfigOf(t, st, rb)
	if !found || !reflect.DeepEqual(c, v1Config) {
		t.Fatalf("rollback applied %+v, want v1's %+v", c, v1Config)
	}
	if p := d.PendingChanges(ctx, []int64{appID})[appID]; p.Any() {
		t.Fatalf("pending after a rollback = %+v", p)
	}
}

func mustApp(t *testing.T, st *DBStore, id int64) App {
	t.Helper()
	a, err := st.GetApplication(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func mustJSON(t *testing.T, c AppliedConfig) string {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A Dockerfile app built with a key set runs its local image ID; applying
// settings reuses it without a clone or build, after checking it is on the
// nodes the app may run on.
func TestApplySettingsOnALocalBuildSkipsTheBuild(t *testing.T) {
	q, st, d, e, appID, _ := configFixture(t)
	ctx := t.Context()
	if _, err := st.pool.Exec(ctx, "UPDATE applications SET source_type='dockerfile', git_url='https://github.com/x/y.git', git_branch='main' WHERE id=$1", appID); err != nil {
		t.Fatal(err)
	}
	local := "sha256:" + strings.Repeat("a", 64)
	e.localID = local
	b := &mockBuilder{}
	d.builder = b
	first := d.Enqueue(appID, TriggerManual)
	runReleaseJob(t, d, first)
	requireDeployment(t, q, first, "done")
	if !b.wasCalled() {
		t.Fatal("the first deploy must build")
	}
	b.called = false
	if err := q.UpdateApplicationEnvClassified(ctx, db.UpdateApplicationEnvClassifiedParams{ID: appID, EnvText: "PUBLIC=changed", EnvPublicKeys: []string{}}); err != nil {
		t.Fatal(err)
	}
	if p := d.PendingChanges(ctx, []int64{appID})[appID]; !p.CanApply {
		t.Fatalf("pending = %+v", p)
	}

	e.validateErr = errors.New("release image unavailable on placement node")
	id, err := d.EnqueueApplySettings(ctx, appID, TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	runReleaseJob(t, d, id)
	requireDeployment(t, q, id, "error")
	if dep, _ := q.GetDeployment(ctx, id); !strings.Contains(dep.ErrorMessage, "not on every node") {
		t.Fatalf("error = %q", dep.ErrorMessage)
	}

	e.validateErr = nil
	id, err = d.EnqueueApplySettings(ctx, appID, TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	runReleaseJob(t, d, id)
	requireDeployment(t, q, id, "done")
	if b.wasCalled() {
		t.Fatal("apply settings must not build")
	}
	if dep, _ := q.GetDeployment(ctx, id); dep.ImageTag != local {
		t.Fatalf("applied image %q, want %q", dep.ImageTag, local)
	}
}
