package dbservice

import (
	"strconv"

	"github.com/proshik/krill/internal/docker"
)

// PostgresDB / RedisDB — доменные представления для построения спеков (маппятся из sqlc-строк в store.go).
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
}

func volumeName(appName string) string { return appName + "-data" }

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
		Constraints: []string{"node.role==manager"},
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
		Name:        r.AppName,
		Image:       r.Image,
		Command:     []string{"/bin/sh"},
		Args:        []string{"-c", "redis-server --requirepass " + r.Password},
		Replicas:    1,
		Network:     network,
		DNSRR:       true,
		Constraints: []string{"node.role==manager"},
		Mounts: []docker.MountSpec{
			{Type: "volume", Source: volumeName(r.AppName), Target: "/data"},
		},
	}
	if r.ExternalPort != nil {
		spec.Ports = []docker.PortSpec{{Target: 6379, Published: uint32(*r.ExternalPort), Mode: "host"}}
	}
	return spec
}

// PostgresInternalURL — строка подключения по overlay (хост = имя сервиса).
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
