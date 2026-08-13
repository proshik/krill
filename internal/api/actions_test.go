package api_test

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/api"
	"github.com/proshik/krill/internal/builder"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
)

// noopEngine is a docker.Engine that does nothing and always reports a single
// healthy task, so a real deployer's deploy/rebuild success paths converge
// instantly without a live Swarm. Mirrors internal/server/fakes_test.go's
// noopEngine (unexported there, so it can't be reused directly).
type noopEngine struct{}

func (noopEngine) NetworkEnsure(context.Context, string) error             { return nil }
func (noopEngine) ServiceDeploy(context.Context, docker.ServiceSpec) error { return nil }
func (noopEngine) ServiceRemove(context.Context, string) error             { return nil }
func (noopEngine) ServiceState(context.Context, string) (docker.ServiceState, error) {
	return docker.ServiceState{Found: true, Running: 1, Desired: 1}, nil
}
func (noopEngine) ServiceProgress(context.Context, string, []string) (docker.ServiceProgress, error) {
	return docker.ServiceProgress{Found: true, Running: 1, Desired: 1}, nil
}
func (noopEngine) ServiceStates(_ context.Context, names []string) (map[string]docker.ServiceState, error) {
	m := map[string]docker.ServiceState{}
	for _, n := range names {
		m[n] = docker.ServiceState{Found: true, Running: 1, Desired: 1}
	}
	return m, nil
}
func (noopEngine) ServiceLogs(context.Context, string, bool, int) (io.ReadCloser, error) {
	return nil, nil
}
func (noopEngine) ServiceScale(context.Context, string, uint64) error { return nil }
func (noopEngine) ServiceRestart(context.Context, string) error       { return nil }
func (noopEngine) ImagePull(context.Context, string, io.Writer) error { return nil }
func (noopEngine) VolumeRemove(context.Context, string) error         { return nil }
func (noopEngine) VolumeArchive(context.Context, string, io.Writer, string) error {
	return nil
}
func (noopEngine) VolumeRestore(context.Context, string, io.Reader, string) error {
	return nil
}
func (noopEngine) VolumeRemoveOn(context.Context, string, string) error { return nil }
func (noopEngine) VolumeExistsOn(context.Context, string, string) (bool, error) {
	return false, nil
}
func (noopEngine) VolumeChown(context.Context, string, int, int, string) error { return nil }
func (noopEngine) ServiceUpdateLabels(context.Context, string, map[string]string) error {
	return nil
}
func (noopEngine) Exec(context.Context, string, []string, []string, io.Reader, io.Writer) error {
	return nil
}
func (noopEngine) ExecInteractive(context.Context, string, []string) (docker.ExecSession, error) {
	return nil, errors.New("exec not supported")
}
func (noopEngine) RegistryCheck(context.Context, string, string, string) error { return nil }
func (noopEngine) ListContainerStats(context.Context) ([]docker.ContainerStat, error) {
	return nil, nil
}
func (noopEngine) NodeInfo(context.Context) (docker.NodeInfo, error)         { return docker.NodeInfo{}, nil }
func (noopEngine) Nodes(context.Context) ([]docker.SwarmNode, error)         { return nil, nil }
func (noopEngine) NodeSetAvailability(context.Context, string, string) error { return nil }
func (noopEngine) NodeRemove(context.Context, string, bool) error            { return nil }
func (noopEngine) SwarmWorkerToken(context.Context) (string, error)          { return "", nil }
func (noopEngine) ServiceTasks(context.Context, string) ([]docker.TaskPlacement, error) {
	return nil, nil
}
func (noopEngine) Tasks(context.Context) ([]docker.TaskInfo, error)           { return nil, nil }
func (noopEngine) NodeSetLabel(context.Context, string, string, string) error { return nil }
func (noopEngine) NodeDeleteLabel(context.Context, string, string) error      { return nil }
func (noopEngine) ResolveDigest(_ context.Context, ref, _ string) (string, error) {
	return ref, nil
}

type noopBuilder struct{}

func (noopBuilder) Build(_ context.Context, _ builder.BuildRequest, _ io.Writer) error { return nil }

// newWriteFixture builds on newAPIFixture, additionally wiring a real
// deployer (noop engine + noop builder) so Deploy/Rebuild/Reload/Stop success
// paths are exercisable, not just their rejection paths. The base fixture
// passes nil engine/deployer since its own tests never touch docker.
func newWriteFixture(t *testing.T) (*apiFixture, *api.Service) {
	t.Helper()
	f := newAPIFixture(t)
	hub := deploy.NewLogHub()
	eng := noopEngine{}
	dep := deploy.New(eng, noopBuilder{}, deploy.NewDBStore(f.q), hub, "krill-net")
	dep.Start(context.Background())
	t.Cleanup(dep.Stop)
	svc := api.NewService(f.q, eng, dep, hub)
	return f, svc
}

// createDockerfileApp adds a second app to the fixture's org, sourced from a
// dockerfile — Deploy/Rebuild treat image and dockerfile apps differently, so
// tests need one of each.
func createDockerfileApp(t *testing.T, f *apiFixture) (int64, string) {
	t.Helper()
	app, err := f.q.CreateApplication(t.Context(), db.CreateApplicationParams{
		EnvironmentID:  mustAppEnvironmentID(t, f),
		Name:           "builder-app",
		Domain:         "builder.example.test",
		Port:           80,
		SourceType:     "dockerfile",
		GitUrl:         "https://example.test/repo.git",
		GitBranch:      "main",
		DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create dockerfile app: %v", err)
	}
	return app.ID, strconv.FormatInt(app.ID, 10)
}

func mustAppEnvironmentID(t *testing.T, f *apiFixture) int64 {
	t.Helper()
	app, err := f.q.GetApplication(t.Context(), f.appID)
	if err != nil {
		t.Fatalf("get application: %v", err)
	}
	return app.EnvironmentID
}

// TestWriteOpsRejectReadToken is the level-gate table test: every mutating
// operation must reject a read-level token with Forbidden, and it must do so
// BEFORE resolving the app — otherwise a read token could tell "app exists"
// (forbidden) apart from "app doesn't exist" (not_found) and enumerate the
// org's applications through that difference.
func TestWriteOpsRejectReadToken(t *testing.T) {
	f := newAPIFixture(t)
	ops := map[string]func(api.Identity) error{
		"deploy":  func(id api.Identity) error { _, err := f.svc.Deploy(t.Context(), id, f.appIDString, ""); return err },
		"rebuild": func(id api.Identity) error { _, err := f.svc.Rebuild(t.Context(), id, f.appIDString); return err },
		"reload":  func(id api.Identity) error { return f.svc.Reload(t.Context(), id, f.appIDString) },
		"stop":    func(id api.Identity) error { return f.svc.Stop(t.Context(), id, f.appIDString) },
		"set_env": func(id api.Identity) error { return f.svc.SetEnv(t.Context(), id, f.appIDString, "K", "V", false) },
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			err := op(f.readIdent)
			var aerr *api.Error
			if !errors.As(err, &aerr) || aerr.Code != api.CodeForbidden {
				t.Fatalf("read token was allowed to %s (err=%v)", name, err)
			}
		})
	}
}

// TestWriteOpsRejectReadTokenEvenForUnknownApp proves the ordering directly:
// a read token must get the identical Forbidden response for an app that
// does not exist at all as for one that does. If resolveApp ran first, a
// nonexistent ref would come back not_found instead — leaking which ids are
// real to a token that has no business knowing.
func TestWriteOpsRejectReadTokenEvenForUnknownApp(t *testing.T) {
	f := newAPIFixture(t)
	const bogus = "999999"
	ops := map[string]func(api.Identity) error{
		"deploy":  func(id api.Identity) error { _, err := f.svc.Deploy(t.Context(), id, bogus, ""); return err },
		"rebuild": func(id api.Identity) error { _, err := f.svc.Rebuild(t.Context(), id, bogus); return err },
		"reload":  func(id api.Identity) error { return f.svc.Reload(t.Context(), id, bogus) },
		"stop":    func(id api.Identity) error { return f.svc.Stop(t.Context(), id, bogus) },
		"set_env": func(id api.Identity) error { return f.svc.SetEnv(t.Context(), id, bogus, "K", "V", false) },
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			err := op(f.readIdent)
			var aerr *api.Error
			if !errors.As(err, &aerr) || aerr.Code != api.CodeForbidden {
				t.Fatalf("want forbidden for a nonexistent app (not not_found), got %v", err)
			}
		})
	}
}

func TestDeploySuccessEnqueuesAndReturnsRunning(t *testing.T) {
	f, svc := newWriteFixture(t)
	got, err := svc.Deploy(t.Context(), f.ident, f.appIDString, "")
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if got.DeploymentID == 0 {
		t.Fatalf("want nonzero deployment id, got 0")
	}
	if got.Status != "running" {
		t.Fatalf("want status running, got %q", got.Status)
	}
}

func TestDeployWithTagUpdatesImageOnImageApp(t *testing.T) {
	f, svc := newWriteFixture(t)
	if _, err := svc.Deploy(t.Context(), f.ident, f.appIDString, "v2"); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	app, err := f.q.GetApplication(t.Context(), f.appID)
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	if app.Tag != "v2" {
		t.Fatalf("want tag v2, got %q", app.Tag)
	}
}

// TestDeployWithTagRejectsDockerfileApp pins the error-message requirement:
// an agent that passes a tag against a dockerfile app must be told tags are
// for image apps and rebuild is the right operation here, not just "invalid".
func TestDeployWithTagRejectsDockerfileApp(t *testing.T) {
	f, svc := newWriteFixture(t)
	_, ref := createDockerfileApp(t, f)
	_, err := svc.Deploy(t.Context(), f.ident, ref, "v2")
	var aerr *api.Error
	if !errors.As(err, &aerr) || aerr.Code != api.CodeInvalid {
		t.Fatalf("want invalid, got %v", err)
	}
	if !strings.Contains(aerr.Message, "image") || !strings.Contains(aerr.Message, "rebuild") {
		t.Fatalf("error should point the agent at image apps and rebuild, got %q", aerr.Message)
	}
}

func TestRebuildSuccessOnDockerfileApp(t *testing.T) {
	f, svc := newWriteFixture(t)
	_, ref := createDockerfileApp(t, f)
	got, err := svc.Rebuild(t.Context(), f.ident, ref)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if got.DeploymentID == 0 {
		t.Fatalf("want nonzero deployment id, got 0")
	}
	if got.Status != "running" {
		t.Fatalf("want status running, got %q", got.Status)
	}
}

// TestRebuildRejectsImageApp pins the converse error-message requirement.
func TestRebuildRejectsImageApp(t *testing.T) {
	f, svc := newWriteFixture(t)
	_, err := svc.Rebuild(t.Context(), f.ident, f.appIDString)
	var aerr *api.Error
	if !errors.As(err, &aerr) || aerr.Code != api.CodeInvalid {
		t.Fatalf("want invalid, got %v", err)
	}
	if !strings.Contains(aerr.Message, "dockerfile") || !strings.Contains(aerr.Message, "deploy") {
		t.Fatalf("error should point the agent at dockerfile apps and deploy, got %q", aerr.Message)
	}
}

// stubActionsEngine is a docker.Engine that records the service name it was
// called with for Reload/Stop, without needing a full deployer wired up.
// Every other method panics if called (embedded nil docker.Engine) — Reload
// and Stop must never touch them.
type stubActionsEngine struct {
	docker.Engine
	restarted string
	scaled    string
	replicas  uint64
	err       error
}

func (e *stubActionsEngine) ServiceRestart(_ context.Context, name string) error {
	e.restarted = name
	return e.err
}

func (e *stubActionsEngine) ServiceScale(_ context.Context, name string, replicas uint64) error {
	e.scaled = name
	e.replicas = replicas
	return e.err
}

func TestReloadRestartsTheAppsService(t *testing.T) {
	f := newAPIFixture(t)
	eng := &stubActionsEngine{}
	svc := api.NewService(f.q, eng, nil, nil)
	if err := svc.Reload(t.Context(), f.ident, f.appIDString); err != nil {
		t.Fatalf("reload: %v", err)
	}
	want := docker.ServiceName(f.appID)
	if eng.restarted != want {
		t.Fatalf("want restart of %q, got %q", want, eng.restarted)
	}
}

func TestStopScalesTheAppsServiceToZero(t *testing.T) {
	f := newAPIFixture(t)
	eng := &stubActionsEngine{}
	svc := api.NewService(f.q, eng, nil, nil)
	if err := svc.Stop(t.Context(), f.ident, f.appIDString); err != nil {
		t.Fatalf("stop: %v", err)
	}
	want := docker.ServiceName(f.appID)
	if eng.scaled != want || eng.replicas != 0 {
		t.Fatalf("want scale(%q, 0), got scale(%q, %d)", want, eng.scaled, eng.replicas)
	}
}

// TestReloadNilEngineReturnsError guards the "misconfigured service errors
// instead of panicking" requirement: the base fixture wires a nil engine.
func TestReloadNilEngineReturnsError(t *testing.T) {
	f := newAPIFixture(t)
	if err := f.svc.Reload(t.Context(), f.ident, f.appIDString); err == nil {
		t.Fatalf("want an error with no engine configured, got nil")
	}
}

func TestStopNilEngineReturnsError(t *testing.T) {
	f := newAPIFixture(t)
	if err := f.svc.Stop(t.Context(), f.ident, f.appIDString); err == nil {
		t.Fatalf("want an error with no engine configured, got nil")
	}
}

func TestDeployNilDeployerReturnsError(t *testing.T) {
	f := newAPIFixture(t)
	if _, err := f.svc.Deploy(t.Context(), f.ident, f.appIDString, ""); err == nil {
		t.Fatalf("want an error with no deployer configured, got nil")
	}
}

func TestRebuildNilDeployerReturnsError(t *testing.T) {
	f := newAPIFixture(t) // f.svc has a nil deployer
	_, ref := createDockerfileApp(t, f)
	if _, err := f.svc.Rebuild(t.Context(), f.ident, ref); err == nil {
		t.Fatalf("want an error with no deployer configured, got nil")
	}
}

// TestSetEnvPreservesOrderAndComments is the core regression test for the
// task: SetEnv must edit exactly one line of env_text and leave everything
// else — order, comments, blank lines, other keys — byte-for-byte untouched.
// The fixture's app starts with env_text "# db\nPORT=8080\nSECRET_TOKEN=hunter2".
func TestSetEnvPreservesOrderAndComments(t *testing.T) {
	f := newAPIFixture(t)
	if err := f.svc.SetEnv(t.Context(), f.ident, f.appIDString, "PORT", "9090", false); err != nil {
		t.Fatalf("set env: %v", err)
	}
	app, err := f.q.GetApplication(t.Context(), f.appID)
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	want := "# db\nPORT=9090\nSECRET_TOKEN=hunter2"
	// Application.EnvText is a plain string, not pgtype.Text.
	if strings.TrimSpace(app.EnvText) != want {
		t.Fatalf("env_text mangled:\ngot  %q\nwant %q", app.EnvText, want)
	}
}

func TestSetEnvAppendsNewKeyAtEnd(t *testing.T) {
	f := newAPIFixture(t)
	if err := f.svc.SetEnv(t.Context(), f.ident, f.appIDString, "NEW_KEY", "v1", false); err != nil {
		t.Fatalf("set env: %v", err)
	}
	app, err := f.q.GetApplication(t.Context(), f.appID)
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	want := "# db\nPORT=8080\nSECRET_TOKEN=hunter2\nNEW_KEY=v1"
	if strings.TrimSpace(app.EnvText) != want {
		t.Fatalf("env_text:\ngot  %q\nwant %q", app.EnvText, want)
	}
}

func TestSetEnvRemoveDeletesOnlyThatLine(t *testing.T) {
	f := newAPIFixture(t)
	if err := f.svc.SetEnv(t.Context(), f.ident, f.appIDString, "SECRET_TOKEN", "", true); err != nil {
		t.Fatalf("set env: %v", err)
	}
	app, err := f.q.GetApplication(t.Context(), f.appID)
	if err != nil {
		t.Fatalf("get app: %v", err)
	}
	want := "# db\nPORT=8080"
	if strings.TrimSpace(app.EnvText) != want {
		t.Fatalf("env_text:\ngot  %q\nwant %q", app.EnvText, want)
	}
}

func TestSetEnvInvalidKeyRejected(t *testing.T) {
	f := newAPIFixture(t)
	err := f.svc.SetEnv(t.Context(), f.ident, f.appIDString, "not a key!", "v", false)
	var aerr *api.Error
	if !errors.As(err, &aerr) || aerr.Code != api.CodeInvalid {
		t.Fatalf("want invalid, got %v", err)
	}
}

func TestSetEnvDuplicateKeyIsConflict(t *testing.T) {
	f := newAPIFixture(t)
	if err := f.q.UpdateApplicationEnv(t.Context(), db.UpdateApplicationEnvParams{
		ID:      f.appID,
		EnvText: "PORT=1\nPORT=2",
	}); err != nil {
		t.Fatalf("seed duplicate env: %v", err)
	}
	err := f.svc.SetEnv(t.Context(), f.ident, f.appIDString, "PORT", "3", false)
	var aerr *api.Error
	if !errors.As(err, &aerr) || aerr.Code != api.CodeConflict {
		t.Fatalf("want conflict, got %v", err)
	}
}

// TestSetEnvCrossOrgIsNotFound confirms SetEnv still uses resolveApp's
// tenancy check (not_found, never forbidden) for a write-level token in the
// wrong org — distinct from the Forbidden-before-resolution gate covered by
// TestWriteOpsRejectReadToken, which is about token level, not org
// membership.
func TestSetEnvCrossOrgIsNotFound(t *testing.T) {
	f := newAPIFixture(t)
	err := f.svc.SetEnv(t.Context(), f.otherIdent, f.appIDString, "PORT", "1", false)
	var aerr *api.Error
	if !errors.As(err, &aerr) || aerr.Code != api.CodeNotFound {
		t.Fatalf("want not_found, got %v", err)
	}
}
