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
//
// Command wraps the official image's own entrypoint in `flock --no-fork` on
// the volume's mount-point directory (which IS PGDATA — never a file inside
// it, since initdb needs an empty directory on first start): this is the
// other half of the single-writer protection, closing the case a service-spec
// update (UpdateStopFirst above) cannot see — a second container placed on
// the same volume by something other than a Swarm rolling update (e.g. a
// worker node returning mid-reconciliation). `--no-fork` is mandatory:
// without it, flock itself stays PID 1 and never forwards the stop signal to
// postgres, turning every clean stop into a SIGKILL and a crash recovery.
// Overriding ENTRYPOINT resets the image's own CMD, so Args restates it
// explicitly.
func (d postgresDriver) BuildSpec(inst Instance, network string) docker.ServiceSpec {
	spec := docker.ServiceSpec{
		Name:        inst.AppName,
		Image:       inst.Image,
		Command:     []string{"flock", "--no-fork", d.MountTarget(), "docker-entrypoint.sh"},
		Args:        []string{"postgres"},
		Replicas:    1,
		Network:     network,
		DNSRR:       true,
		Constraints: []string{DBConstraint(inst.NodeHostname)},
		// UpdateStopFirst: two postmasters must never share one PGDATA — see
		// the Command comment above for the other half of the single-writer
		// protection (a service-spec update is not the only way a second
		// container can appear on the same volume).
		UpdateStopFirst: true,
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
