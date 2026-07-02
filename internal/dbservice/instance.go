package dbservice

import (
	"strconv"

	"github.com/proshik/krill/internal/docker"
)

// Instance — a DBMS server (org-level): one Swarm service on a chosen node,
// holding N logical databases (postgres) or serving apps directly (redis).
type Instance struct {
	ID                int64
	OrganizationID    int64
	Engine            string // "postgres" | "redis"
	Name              string
	AppName           string // Swarm service name = overlay DNS host = volume prefix
	Image             string
	Superuser         string // postgres only; "" for redis
	SuperuserPassword string // redis: the requirepass value
	ExternalPort      *int32
	Status            string
	NodeHostname      string // "" = control-plane (manager)
}

// LogicalDB — a database inside a postgres Instance, owned by an environment.
type LogicalDB struct {
	ID            int64
	InstanceID    int64
	EnvironmentID int64
	Name          string
	DBName        string
	Username      string
	Password      string
}

// InstanceFeedID — the DB log-hub feed for an instance (negative, single id
// space: instances live in one table, unlike the legacy pg/redis pair).
func InstanceFeedID(id int64) int64 { return -id }

func instanceSpec(inst Instance, network string) docker.ServiceSpec {
	spec := docker.ServiceSpec{
		Name:        inst.AppName,
		Image:       inst.Image,
		Replicas:    1,
		Network:     network,
		DNSRR:       true,
		Constraints: []string{dbConstraint(inst.NodeHostname)},
	}
	switch inst.Engine {
	case "postgres":
		// No POSTGRES_DB: it only matters on first volume init; databases are
		// created by provisioning (converted instances have an initialized volume).
		spec.Env = map[string]string{
			"POSTGRES_USER":     inst.Superuser,
			"POSTGRES_PASSWORD": inst.SuperuserPassword,
		}
		spec.Mounts = []docker.MountSpec{{Type: "volume", Source: volumeName(inst.AppName), Target: "/var/lib/postgresql/data"}}
		if inst.ExternalPort != nil {
			spec.Ports = []docker.PortSpec{{Target: 5432, Published: uint32(*inst.ExternalPort), Mode: "host"}}
		}
	case "redis":
		// Exec form (no shell): the password is a discrete argv element.
		spec.Args = []string{"redis-server", "--requirepass", inst.SuperuserPassword}
		spec.Mounts = []docker.MountSpec{{Type: "volume", Source: volumeName(inst.AppName), Target: "/data"}}
		if inst.ExternalPort != nil {
			spec.Ports = []docker.PortSpec{{Target: 6379, Published: uint32(*inst.ExternalPort), Mode: "host"}}
		}
	}
	return spec
}

// PostgresURL — connection string for a logical DB over the overlay network.
// scheme is "postgresql" or "postgres" (validated by the caller).
func PostgresURL(scheme string, inst Instance, ldb LogicalDB) string {
	return scheme + "://" + ldb.Username + ":" + ldb.Password + "@" + inst.AppName + ":5432/" + ldb.DBName
}

func PostgresExternalURL(inst Instance, ldb LogicalDB, host string) string {
	p := ""
	if inst.ExternalPort != nil {
		p = strconv.Itoa(int(*inst.ExternalPort))
	}
	return "postgresql://" + ldb.Username + ":" + ldb.Password + "@" + host + ":" + p + "/" + ldb.DBName
}

// RedisInternalURL2/RedisExternalURL2 — instance-model URL builders. The "2"
// suffix avoids clashing with the legacy builders; renamed when those go away.
func RedisInternalURL2(inst Instance) string {
	return "redis://default:" + inst.SuperuserPassword + "@" + inst.AppName + ":6379"
}

func RedisExternalURL2(inst Instance, host string) string {
	p := ""
	if inst.ExternalPort != nil {
		p = strconv.Itoa(int(*inst.ExternalPort))
	}
	return "redis://default:" + inst.SuperuserPassword + "@" + host + ":" + p
}
