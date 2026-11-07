//go:build integration

package dbservice_test

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/dbservice/drivers"
	"github.com/proshik/krill/internal/docker"
)

// The connection budget of a real Postgres instance: a session of the logical
// database's role holding an advisory lock shows up in its database, role and
// advisory-lock counts, against the server's max_connections.
func TestConnectionStatsOnPostgres(t *testing.T) {
	eng, err := docker.NewEngine("")
	if err != nil {
		t.Fatal(err)
	}
	skipNoSwarm(t, eng)
	ctx := context.Background()
	d, _ := drivers.Registry.Lookup("postgres")
	inst := drivers.Instance{ID: 990002, Engine: "postgres", AppName: "krill-it-pgstats", Image: "postgres:17", Superuser: "postgres", SuperuserPassword: "root-pw-1"}
	clean := func() {
		_ = eng.ServiceRemove(context.Background(), inst.AppName)
		for i := 0; i < 15 && eng.VolumeRemove(context.Background(), drivers.VolumeName(inst.AppName)) != nil; i++ {
			time.Sleep(time.Second)
		}
	}
	clean()
	t.Cleanup(clean)
	if err := eng.ImagePull(ctx, inst.Image, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := eng.ServiceDeploy(ctx, d.BuildSpec(inst, "krill-net")); err != nil {
		t.Fatal(err)
	}
	svc := dbservice.New(eng, nil, nil, "krill-net")
	ldb := dbservice.LogicalDB{DBName: "penpot", Username: "penpot", Password: "role-pw-1"}
	deadline := time.Now().Add(120 * time.Second)
	for {
		if err = svc.ProvisionLogicalDB(ctx, inst, ldb); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("provision: %v", err)
		}
		time.Sleep(3 * time.Second)
	}
	before, err := svc.ConnectionStats(ctx, inst, ldb)
	if err != nil {
		t.Fatal(err)
	}
	if before.Max < 20 || before.Database != 0 || before.AdvisoryLocks != 0 {
		t.Fatalf("idle stats: %+v", before)
	}
	// A session of the app's role that takes an advisory lock and sits on it,
	// like a migration lock left behind by a stuck release.
	hold, cancel := context.WithCancel(ctx)
	defer cancel()
	go eng.Exec(hold, inst.AppName, []string{"psql", "-U", "penpot", "-d", "penpot", "-h", "127.0.0.1"},
		[]string{"PGPASSWORD=role-pw-1"}, strings.NewReader("SELECT pg_advisory_lock(42); SELECT pg_sleep(60);"), io.Discard)
	deadline = time.Now().Add(30 * time.Second)
	for {
		st, err := svc.ConnectionStats(ctx, inst, ldb)
		if err != nil {
			t.Fatal(err)
		}
		if st.Database >= 1 && st.Role >= 1 && st.AdvisoryLocks >= 1 && st.Total > before.Total {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the lock-holding session never showed up: %+v", st)
		}
		time.Sleep(time.Second)
	}
}
