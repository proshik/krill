package drivers

import (
	"strconv"

	"github.com/proshik/krill/internal/docker"
)

// postgresLinkFields — app-link fields the postgres engine exposes.
var postgresLinkFields = []string{"url", "password", "host", "port", "user", "dbname", "hostport"}

type postgresDriver struct{}

func (postgresDriver) Engine() string        { return "postgres" }
func (postgresDriver) Label() string         { return "PostgreSQL" }
func (postgresDriver) DefaultImage() string  { return "postgres:17" }
func (postgresDriver) SuperuserName() string { return "postgres" }
func (postgresDriver) MountTarget() string   { return "/var/lib/postgresql/data" }

// BuildSpec mirrors the legacy instanceSpec "postgres" case byte-for-byte: no
// POSTGRES_DB (it only matters on first volume init; databases are created by
// provisioning — converted instances have an initialized volume already).
func (d postgresDriver) BuildSpec(inst Instance, network string) docker.ServiceSpec {
	spec := docker.ServiceSpec{
		Name:        inst.AppName,
		Image:       inst.Image,
		Replicas:    1,
		Network:     network,
		DNSRR:       true,
		Constraints: []string{DBConstraint(inst.NodeHostname)},
	}
	spec.Env = map[string]string{
		"POSTGRES_USER":     inst.Superuser,
		"POSTGRES_PASSWORD": inst.SuperuserPassword,
	}
	spec.Mounts = []docker.MountSpec{{Type: "volume", Source: volumeName(inst.AppName), Target: d.MountTarget()}}
	return spec
}

func (postgresDriver) ExternalTargets(inst Instance) []ProxyTarget {
	return []ProxyTarget{{Suffix: "", HostPort: inst.ExternalPort, ContainerPort: 5432}}
}

func (postgresDriver) HasLogicalResource() bool { return true }

func (postgresDriver) LinkFields() []string { return postgresLinkFields }

// LinkValue mirrors the legacy dbLinkFieldValue logic (internal/deploy/store.go):
// host is always the instance's overlay DNS name, port is the fixed postgres
// port — an app link is always an internal (in-cluster) connection.
func (postgresDriver) LinkValue(src LinkSource, field string) (string, bool) {
	return fieldValue(field, src.Superuser, src.Password, src.AppName, "5432", src.DBName, src.Scheme), true
}

// ConnDisplay renders the instance's own superuser connection (dbname
// "postgres") for the DB-server detail page's "Connection" panel.
func (d postgresDriver) ConnDisplay(inst Instance, controlPlaneHost string) []ConnField {
	fields := []ConnField{
		{Label: "db.internal", Value: URLString("postgresql", inst.Superuser, inst.SuperuserPassword, inst.AppName, "5432", "postgres"), Secret: true},
	}
	if inst.ExternalPort != nil {
		fields = append(fields, ConnField{
			Label:  "db.external",
			Value:  URLString("postgresql", inst.Superuser, inst.SuperuserPassword, controlPlaneHost, strconv.Itoa(int(*inst.ExternalPort)), "postgres"),
			Secret: true,
		})
	}
	return fields
}
