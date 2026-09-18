package drivers

import (
	"strconv"

	"github.com/proshik/krill/internal/docker"
)

// redisLinkFields — app-link fields the redis engine exposes (no per-database
// user/dbname: redis has no logical sub-resource).
var redisLinkFields = []string{"url", "password", "host", "port", "hostport"}

// redisSuperuser — the fixed username redis connection strings use (ACL
// default user); redis has no stored superuser identity of its own.
const redisSuperuser = "default"

type redisDriver struct{}

func (redisDriver) Engine() string        { return "redis" }
func (redisDriver) Label() string         { return "Redis" }
func (redisDriver) DefaultImage() string  { return "redis:7" }
func (redisDriver) SuperuserName() string { return "" }
func (redisDriver) MountTarget() string   { return "/data" }

// BuildSpec mirrors the legacy instanceSpec "redis" case byte-for-byte: exec
// form (no shell) so the password is a discrete argv element.
//
// Command wraps the official image's own entrypoint in a portable flock on
// the volume's mount-point directory — see flockCommand and the identical
// comment on postgresDriver.BuildSpec for why the lock is on the directory
// (not a file inside it) and why it's not `flock --no-fork` (redis:7-alpine's
// busybox flock doesn't support that flag).
func (d redisDriver) BuildSpec(inst Instance, network string) docker.ServiceSpec {
	spec := docker.ServiceSpec{
		Name:            inst.AppName,
		Image:           inst.Image,
		Command:         flockCommand(d.MountTarget(), "docker-entrypoint.sh"),
		Replicas:        1,
		Network:         network,
		DNSRR:           true,
		Constraints:     []string{DBConstraint(inst.NodeHostname)},
		UpdateStopFirst: true, // single-writer protection; see the Command comment above
	}
	spec.Args = []string{"redis-server", "--requirepass", inst.SuperuserPassword}
	spec.Mounts = []docker.MountSpec{{Type: "volume", Source: volumeName(inst.AppName), Target: d.MountTarget()}}
	return spec
}

func (redisDriver) ExternalTargets(inst Instance) []ProxyTarget {
	return []ProxyTarget{{Suffix: "", HostPort: inst.ExternalPort, ContainerPort: 6379}}
}

func (redisDriver) HasLogicalResource() bool { return false }

func (redisDriver) LinkFields() []string { return redisLinkFields }

// LinkValue mirrors the legacy dbLinkFieldValue logic (internal/deploy/store.go):
// user is always "default", dbname is always "" (no per-database scoping).
func (redisDriver) LinkValue(src LinkSource, field string) (string, bool) {
	return fieldValue(field, redisSuperuser, src.Password, src.AppName, "6379", "", src.Scheme), true
}

// ConnDisplay renders the instance's own connection for the DB-server detail
// page's "Connection" panel.
func (redisDriver) ConnDisplay(inst Instance, controlPlaneHost string) []ConnField {
	fields := []ConnField{
		{Label: "db.internal", Value: URLString("redis", redisSuperuser, inst.SuperuserPassword, inst.AppName, "6379", ""), Secret: true},
	}
	if inst.ExternalPort != nil {
		fields = append(fields, ConnField{
			Label:  "db.external",
			Value:  URLString("redis", redisSuperuser, inst.SuperuserPassword, controlPlaneHost, strconv.Itoa(int(*inst.ExternalPort)), ""),
			Secret: true,
		})
	}
	return fields
}
