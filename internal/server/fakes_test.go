package server_test

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/backup"
	"github.com/proshik/krill/internal/builder"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/config"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/testutil"
)

// noopEngine is a docker.Engine that does nothing and always reports a single
// healthy task, so deploys converge instantly in handler tests.
type noopEngine struct{}

func (noopEngine) NetworkEnsure(context.Context, string) error          { return nil }
func (noopEngine) ServiceDeploy(context.Context, docker.ServiceSpec) error { return nil }
func (noopEngine) ServiceRemove(context.Context, string) error          { return nil }
func (noopEngine) ServiceState(context.Context, string) (docker.ServiceState, error) {
	return docker.ServiceState{Found: true, Running: 1, Desired: 1}, nil
}
func (noopEngine) ServiceStates(_ context.Context, names []string) (map[string]docker.ServiceState, error) {
	m := map[string]docker.ServiceState{}
	for _, n := range names {
		m[n] = docker.ServiceState{Found: true, Running: 1, Desired: 1}
	}
	return m, nil
}
func (noopEngine) ServiceLogs(context.Context, string, bool) (io.ReadCloser, error) { return nil, nil }
func (noopEngine) ServiceScale(context.Context, string, uint64) error               { return nil }
func (noopEngine) ServiceRestart(context.Context, string) error                     { return nil }
func (noopEngine) ImagePull(context.Context, string, io.Writer) error               { return nil }
func (noopEngine) VolumeRemove(context.Context, string) error                       { return nil }
func (noopEngine) ServiceUpdateLabels(context.Context, string, map[string]string) error {
	return nil
}
func (noopEngine) Exec(context.Context, string, []string, []string, io.Reader, io.Writer) error {
	return nil
}
func (noopEngine) RegistryCheck(context.Context, string, string, string) error { return nil }

type noopBuilder struct{}

func (noopBuilder) Build(_ context.Context, _ builder.BuildRequest, _ io.Writer) error { return nil }

// newDeployServer is like newServer but wires a real deployer with a no-op
// engine/builder, so the deploy/reload/rebuild success paths are exercisable.
func newDeployServer(t *testing.T) (http.Handler, *db.Queries, *org.Service) {
	t.Helper()
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	orgSvc := org.NewService(q)
	cfg := config.Config{BaseDomain: "127-0-0-1.sslip.io", Network: "krill-net"}
	hub := deploy.NewLogHub()
	eng := noopEngine{}
	dep := deploy.New(eng, noopBuilder{}, deploy.NewDBStore(q), hub, "krill-net")
	dep.Start(context.Background())
	t.Cleanup(dep.Stop)
	dbSvc := dbservice.New(eng, dbservice.NewDBStore(q), hub, "krill-net")
	srv := server.New(cfg, auth.NewService(q), orgSvc, q, dep, eng, hub, dbSvc)
	srv.SetBackups(backup.New(nil, backup.NewDBStore(q)), func() {})
	return srv.Router(), q, orgSvc
}
