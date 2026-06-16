package dbservice

import (
	"strconv"

	"github.com/proshik/krill/internal/docker"
)

// PostgresDB / RedisDB — domain representations for building specs (mapped from sqlc rows in store.go).
type PostgresDB struct {
	ID               int64
	EnvironmentID    int64
	Name             string
	AppName          string
	DatabaseName     string
	DatabaseUser     string
	DatabasePassword string
	Image            string
	ExternalPort     *int32
	Status           string
	NodeHostname     string // "" = control-plane (manager)
}

type RedisDB struct {
	ID            int64
	EnvironmentID int64
	Name          string
	AppName       string
	Password      string
	Image         string
	ExternalPort  *int32
	Status        string
	NodeHostname  string // "" = control-plane (manager)
}

func volumeName(appName string) string { return appName + "-data" }

// dbConstraint pins a managed DB to a chosen node, or to the control-plane
// (manager) when none is selected. The named volume lives on that node.
func dbConstraint(nodeHostname string) string {
	if nodeHostname != "" {
		return "node.hostname==" + nodeHostname
	}
	return "node.role==manager"
}

func postgresSpec(pg PostgresDB, network string) docker.ServiceSpec {
	spec := docker.ServiceSpec{
		Name:  pg.AppName,
		Image: pg.Image,
		Env: map[string]string{
			"POSTGRES_DB":       pg.DatabaseName,
			"POSTGRES_USER":     pg.DatabaseUser,
			"POSTGRES_PASSWORD": pg.DatabasePassword,
		},
		Replicas:    1,
		Network:     network,
		DNSRR:       true,
		Constraints: []string{dbConstraint(pg.NodeHostname)},
		Mounts: []docker.MountSpec{
			{Type: "volume", Source: volumeName(pg.AppName), Target: "/var/lib/postgresql/data"},
		},
	}
	if pg.ExternalPort != nil {
		spec.Ports = []docker.PortSpec{{Target: 5432, Published: uint32(*pg.ExternalPort), Mode: "host"}}
	}
	return spec
}

func redisSpec(r RedisDB, network string) docker.ServiceSpec {
	spec := docker.ServiceSpec{
		Name:  r.AppName,
		Image: r.Image,
		// Exec form (no shell): the password is a discrete argv element, so even
		// a value with shell metacharacters can never be reinterpreted. Args go
		// through the image's docker-entrypoint.sh (which execs `redis-server`).
		Args:        []string{"redis-server", "--requirepass", r.Password},
		Replicas:    1,
		Network:     network,
		DNSRR:       true,
		Constraints: []string{dbConstraint(r.NodeHostname)},
		Mounts: []docker.MountSpec{
			{Type: "volume", Source: volumeName(r.AppName), Target: "/data"},
		},
	}
	if r.ExternalPort != nil {
		spec.Ports = []docker.PortSpec{{Target: 6379, Published: uint32(*r.ExternalPort), Mode: "host"}}
	}
	return spec
}

// PostgresInternalURL — connection string over the overlay network (host = service name).
func PostgresInternalURL(pg PostgresDB) string {
	return "postgresql://" + pg.DatabaseUser + ":" + pg.DatabasePassword + "@" + pg.AppName + ":5432/" + pg.DatabaseName
}

func PostgresExternalURL(pg PostgresDB, host string) string {
	p := ""
	if pg.ExternalPort != nil {
		p = strconv.Itoa(int(*pg.ExternalPort))
	}
	return "postgresql://" + pg.DatabaseUser + ":" + pg.DatabasePassword + "@" + host + ":" + p + "/" + pg.DatabaseName
}

func RedisInternalURL(r RedisDB) string {
	return "redis://default:" + r.Password + "@" + r.AppName + ":6379"
}

func RedisExternalURL(r RedisDB, host string) string {
	p := ""
	if r.ExternalPort != nil {
		p = strconv.Itoa(int(*r.ExternalPort))
	}
	return "redis://default:" + r.Password + "@" + host + ":" + p
}
