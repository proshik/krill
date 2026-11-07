package drivers

import (
	"time"

	"github.com/proshik/krill/internal/docker"
)

// valkeyDefaultImage — the official Valkey image, pinned to a release tag so
// what an instance runs is visible and reproducible.
const valkeyDefaultImage = "valkey/valkey:8.1.4"

// valkeyDriver — Valkey speaks RESP like Redis, so its connection/link shape
// (redis://default:pw@app:6379) and app-link fields are redisDriver's. It is
// its own driver, not the redis driver with another image, because the image
// is not interchangeable: its docker-entrypoint.sh chowns /data and drops to
// the valkey user only when the command is `valkey-server`. The image does ship
// a redis-server compatibility link, so the redis driver's args would start
// it — as root, on a volume owned by root.
//
// Durable by default: append-only file plus the default RDB snapshots on the
// /data volume, so a restart or a redeploy keeps the data.
type valkeyDriver struct {
	redisDriver
}

func (valkeyDriver) Engine() string       { return "valkey" }
func (valkeyDriver) Label() string        { return "Valkey" }
func (valkeyDriver) DefaultImage() string { return valkeyDefaultImage }

// SuperuserName reports the fixed ACL user the connection strings use, stored
// on the row for display like DragonFly's.
func (valkeyDriver) SuperuserName() string { return "default" }

// BuildSpec mirrors redisDriver.BuildSpec: exec form so the password is one
// argv element, the image's own entrypoint wrapped in the portable directory
// flock (the image is Debian-based and ships util-linux's flock; alpine tags
// ship busybox's, which the idiom also supports), stop-first updates.
//
// The healthcheck pings through valkey-cli with the password from the
// environment (REDISCLI_AUTH, which valkey-cli honours), so a task that
// accepts connections but refuses authenticated commands is not reported
// healthy.
func (d valkeyDriver) BuildSpec(inst Instance, network string) docker.ServiceSpec {
	spec := docker.ServiceSpec{
		Name:            inst.AppName,
		Image:           inst.Image,
		Command:         flockCommand(d.MountTarget(), "docker-entrypoint.sh"),
		Replicas:        1,
		Network:         network,
		DNSRR:           true,
		Constraints:     []string{DBConstraint(inst.NodeHostname)},
		UpdateStopFirst: true,
		Env:             map[string]string{"REDISCLI_AUTH": inst.SuperuserPassword},
		Healthcheck: &docker.HealthcheckSpec{
			Test:        []string{"CMD-SHELL", "valkey-cli ping | grep -q PONG"},
			Interval:    10 * time.Second,
			Timeout:     5 * time.Second,
			StartPeriod: 30 * time.Second,
			Retries:     3,
		},
	}
	spec.Args = []string{"valkey-server", "--requirepass", inst.SuperuserPassword, "--appendonly", "yes"}
	spec.Mounts = []docker.MountSpec{{Type: "volume", Source: volumeName(inst.AppName), Target: d.MountTarget()}}
	return spec
}
