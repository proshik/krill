//go:build integration

package dbservice_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/proshik/krill/internal/docker"
)

func skipNoSwarm(t *testing.T, e docker.Engine) {
	t.Helper()
	if err := e.NetworkEnsure(context.Background(), "krill-net"); err != nil {
		t.Skipf("swarm unavailable: %v", err)
	}
}

func TestPostgresDeployAndConnect(t *testing.T) {
	eng, err := docker.NewEngine("")
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	skipNoSwarm(t, eng)
	ctx := context.Background()
	name := "krill-it-pg"
	t.Cleanup(func() { _ = eng.ServiceRemove(context.Background(), name) })

	spec := docker.ServiceSpec{
		Name: name, Image: "postgres:17", Replicas: 1, Network: "krill-net", DNSRR: true,
		Constraints: []string{"node.role==manager"},
		Env:         map[string]string{"POSTGRES_DB": "app", "POSTGRES_USER": "u", "POSTGRES_PASSWORD": "p"},
		Mounts:      []docker.MountSpec{{Type: "volume", Source: name + "-data", Target: "/var/lib/postgresql/data"}},
		Ports:       []docker.PortSpec{{Target: 5432, Published: 54329, Mode: "host"}},
	}
	if err := eng.ImagePull(ctx, "postgres:17", &nopW{}); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if err := eng.ServiceDeploy(ctx, spec); err != nil {
		t.Fatalf("deploy: %v", err)
	}

	// дождаться готовности и подключиться по external port
	dsn := "postgres://u:p@127.0.0.1:54329/app?sslmode=disable"
	var conn *sql.DB
	deadline := time.Now().Add(90 * time.Second)
	for {
		st, _ := eng.ServiceState(ctx, name)
		if st.Found && st.Running >= 1 {
			conn, err = sql.Open("pgx", dsn)
			if err == nil {
				if pingErr := conn.PingContext(ctx); pingErr == nil {
					break
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("postgres not reachable in time")
		}
		time.Sleep(3 * time.Second)
	}
	defer conn.Close()
	var one int
	if err := conn.QueryRow("SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("select 1: %v (%d)", err, one)
	}
	fmt.Println("postgres reachable, SELECT 1 ok")

	// stop/start
	if err := eng.ServiceScale(ctx, name, 0); err != nil {
		t.Fatalf("scale 0: %v", err)
	}
	if err := eng.ServiceScale(ctx, name, 1); err != nil {
		t.Fatalf("scale 1: %v", err)
	}
}

type nopW struct{}

func (nopW) Write(p []byte) (int, error) { return len(p), nil }
