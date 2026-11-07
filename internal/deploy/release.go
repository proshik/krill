package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"

	"github.com/proshik/krill/internal/appmetrics"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/secret"
)

var ErrReleaseUnavailable = errors.New("release unavailable: encryption key, confirmed snapshot and immutable image required")
var ErrReleaseDrift = errors.New("rollback refused: settings changed or the image, volumes, nodes, database links or metrics credentials are unavailable or incompatible")

// RuntimeSettings contains only editor fields that define the runtime release.
// Build arguments, credentials, routes and infrastructure are deliberately absent.
type RuntimeSettings struct {
	Image                  string   `json:"image"`
	Tag                    string   `json:"tag"`
	SourceType             string   `json:"source_type"`
	GitURL                 string   `json:"git_url"`
	GitBranch              string   `json:"git_branch"`
	DockerfilePath         string   `json:"dockerfile_path"`
	EnvText                string   `json:"env_text"`
	EnvPublicKeys          []string `json:"env_public_keys"`
	Command                *string  `json:"command"`
	Port                   int32    `json:"port"`
	Replicas               int32    `json:"replicas"`
	MemoryLimit            *string  `json:"memory_limit"`
	CPULimit               *string  `json:"cpu_limit"`
	RestartCondition       string   `json:"restart_condition"`
	RestartMaxAttempts     int32    `json:"restart_max_attempts"`
	HealthcheckCmd         *string  `json:"healthcheck_cmd"`
	HealthcheckInterval    *string  `json:"healthcheck_interval"`
	HealthcheckTimeout     *string  `json:"healthcheck_timeout"`
	HealthcheckRetries     *int32   `json:"healthcheck_retries"`
	HealthcheckStartPeriod *string  `json:"healthcheck_start_period"`
	PredeployCommand       string   `json:"predeploy_command"`
}

func runtimeSettings(a db.Application) RuntimeSettings {
	keys := a.EnvPublicKeys
	if keys == nil {
		keys = []string{}
	}
	return RuntimeSettings{a.Image, a.Tag, a.SourceType, a.GitUrl, a.GitBranch, a.DockerfilePath, a.EnvText, keys, a.Command, a.Port, a.Replicas, a.MemoryLimit, a.CpuLimit, a.RestartCondition, a.RestartMaxAttempts, a.HealthcheckCmd, a.HealthcheckInterval, a.HealthcheckTimeout, a.HealthcheckRetries, a.HealthcheckStartPeriod, a.PredeployCommand}
}

type releaseGuards struct {
	Dependencies   string
	Mounts         []docker.MountSpec
	Ports          []docker.PortSpec
	PlacementMode  string
	PlacementNodes []string
}

func guards(a App) releaseGuards {
	return releaseGuards{a.Dependencies, a.Mounts, a.Ports, a.PlacementMode, a.PlacementNodes}
}

type releaseSnapshot struct {
	Version          int
	MemoryLimitBytes int64
	NanoCPUs         int64
	Image            string
	Runtime          RuntimeSettings
	Env              map[string]string
	Guards           releaseGuards
}

func releaseIdentity(appID, deployID int64) string {
	return fmt.Sprintf("app:%d/deployment:%d", appID, deployID)
}
func encodeRelease(appID, deployID int64, r releaseSnapshot) (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return secret.Seal(string(b), releaseIdentity(appID, deployID))
}
func decodeRelease(appID, deployID int64, payload string) (releaseSnapshot, error) {
	var r releaseSnapshot
	plain, err := secret.Open(payload, releaseIdentity(appID, deployID))
	if err != nil {
		return r, ErrReleaseUnavailable
	}
	if json.Unmarshal([]byte(plain), &r) != nil || r.Version != 1 || !immutableImage(r.Image) {
		return r, ErrReleaseUnavailable
	}
	return r, nil
}
func immutableImage(s string) bool { return docker.IsImmutableImage(s) }

// releaseStore keeps snapshots optional for small test stores and engines.
type releaseStore interface {
	SaveRelease(context.Context, int64, int64, releaseSnapshot, int64) error
	RecordRollbackSource(context.Context, int64, int64) error
	LoadRelease(context.Context, int64, int64) (releaseSnapshot, error)
	RollbackApplication(context.Context, int64, string) (App, error)
	CheckRollback(context.Context, int64, RuntimeSettings, releaseGuards, string) error
	ConfirmRelease(context.Context, int64, int64, *RuntimeSettings, *RuntimeSettings) error
}

func (s *DBStore) SaveRelease(ctx context.Context, appID, deployID int64, r releaseSnapshot, source int64) error {
	payload, err := encodeRelease(appID, deployID, r)
	if err != nil {
		return err
	}
	var src *int64
	if source != 0 {
		src = &source
	}
	return s.q.SaveDeploymentRelease(ctx, db.SaveDeploymentReleaseParams{DeploymentID: deployID, ApplicationID: appID, Payload: payload, SourceDeploymentID: src})
}
func (s *DBStore) LoadRelease(ctx context.Context, appID, deployID int64) (releaseSnapshot, error) {
	row, err := s.q.GetDeploymentRelease(ctx, db.GetDeploymentReleaseParams{DeploymentID: deployID, ApplicationID: appID})
	if err != nil {
		return releaseSnapshot{}, ErrReleaseUnavailable
	}
	return decodeRelease(appID, deployID, row.Payload)
}
func (s *DBStore) RollbackApplication(ctx context.Context, appID int64, image string) (App, error) {
	return s.getApplication(ctx, appID, ImageRef{SkipBuild: true, CredentialImage: image})
}
func (s *DBStore) CheckRollback(ctx context.Context, id int64, expected RuntimeSettings, want releaseGuards, image string) error {
	a, err := s.RollbackApplication(ctx, id, image)
	if err != nil {
		return ErrReleaseDrift
	}
	if !reflect.DeepEqual(a.Runtime, expected) || !reflect.DeepEqual(guards(a), want) {
		return ErrReleaseDrift
	}
	return nil
}
func (s *DBStore) ConfirmRelease(ctx context.Context, appID, deployID int64, expected, restore *RuntimeSettings) error {
	if restore == nil {
		return s.q.ConfirmDeploymentRelease(ctx, deployID)
	}
	before, _ := json.Marshal(expected)
	after, _ := json.Marshal(restore)
	n, err := s.q.RestoreReleaseRuntime(ctx, db.RestoreReleaseRuntimeParams{ID: appID, DeploymentID: deployID, Expected: before, Restored: after})
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrReleaseDrift
	}
	return nil
}

func (d *Deployer) EnqueueRollback(ctx context.Context, appID, sourceID int64) (int64, error) {
	st, ok := d.store.(releaseStore)
	if !ok || !secret.Enabled() {
		return 0, ErrReleaseUnavailable
	}
	snapshot, err := st.LoadRelease(ctx, appID, sourceID)
	if err != nil {
		return 0, err
	}
	current, err := st.RollbackApplication(ctx, appID, snapshot.Image)
	if err != nil || !reflect.DeepEqual(guards(current), snapshot.Guards) {
		return 0, ErrReleaseDrift
	}
	expected := current.Runtime
	id, qerr := d.enqueue(appID, TriggerManual, job{rollbackID: sourceID, skipPredeploy: true, rollbackExpected: &expected})
	if id == 0 {
		if qerr != nil {
			return 0, qerr
		}
		return 0, errors.New("deployment queue refused rollback")
	}
	return id, nil
}

// snapshotFor stores the effective limits, not defaults that may change later.
func (d *Deployer) snapshotFor(a App, image string) releaseSnapshot {
	a = d.withResourceDefaults(a)
	r := a.Runtime
	// Source coordinates are provenance; embedded Git credentials must not be archived.
	if u, err := url.Parse(r.GitURL); err == nil && u.User != nil {
		u.User = nil
		r.GitURL = u.String()
	}
	return releaseSnapshot{Version: 1, Image: image, Runtime: r, Env: a.Env, Guards: guards(a), MemoryLimitBytes: a.MemoryLimitBytes, NanoCPUs: a.NanoCPUs}
}
func applyRelease(a App, r releaseSnapshot) App {
	a.RuntimePinned = true
	a.Runtime = r.Runtime
	a.Image = r.Runtime.Image
	a.Tag = r.Runtime.Tag
	a.SourceType = r.Runtime.SourceType
	a.Env = r.Env
	a.Args = docker.SplitCommand(strDeref(r.Runtime.Command))
	a.Port = r.Runtime.Port
	a.MetricsHiddenPaths = appmetrics.HiddenPaths(a.Port, a.MetricsEndpoints)
	a.Replicas = uint64(r.Runtime.Replicas)
	a.MemoryLimitBytes = r.MemoryLimitBytes
	a.NanoCPUs = r.NanoCPUs
	a.RestartCondition = r.Runtime.RestartCondition
	a.RestartMaxAttempts = uint64(r.Runtime.RestartMaxAttempts)
	a.Healthcheck = buildHealthcheck(db.Application{HealthcheckCmd: r.Runtime.HealthcheckCmd, HealthcheckInterval: r.Runtime.HealthcheckInterval, HealthcheckTimeout: r.Runtime.HealthcheckTimeout, HealthcheckRetries: r.Runtime.HealthcheckRetries, HealthcheckStartPeriod: r.Runtime.HealthcheckStartPeriod})
	a.PredeployCommand = r.Runtime.PredeployCommand
	return a
}

func (s *DBStore) RecordRollbackSource(ctx context.Context, id, source int64) error {
	return s.q.SetDeploymentRollbackSource(ctx, db.SetDeploymentRollbackSourceParams{ID: id, RollbackSourceID: &source})
}
