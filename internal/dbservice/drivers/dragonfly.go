package drivers

import "github.com/proshik/krill/internal/docker"

// dragonflyDefaultImage — the official DragonFly image. Its ENTRYPOINT is
// entrypoint.sh (execs its args), and its default CMD is
// ["dragonfly", "--logtostderr"] — so BuildSpec's Args mirror that CMD shape
// ("dragonfly" as argv[0]) rather than the bare-binary-entrypoint form redis
// uses for its own image.
const dragonflyDefaultImage = "docker.dragonflydb.io/dragonflydb/dragonfly:latest"

// dragonflyDriver — DragonFly is RESP wire-protocol compatible with Redis, so
// its connection/link shape (redis://default:pw@app:6379) and app-link field
// set are identical to redisDriver's; it embeds redisDriver and only
// overrides the identity/image/spec bits that actually differ.
type dragonflyDriver struct {
	redisDriver
}

func (dragonflyDriver) Engine() string       { return "dragonfly" }
func (dragonflyDriver) Label() string        { return "DragonFly" }
func (dragonflyDriver) DefaultImage() string { return dragonflyDefaultImage }

// SuperuserName — unlike the embedded redisDriver (which reports "" since
// redis has never stored a superuser identity of its own — LinkValue/
// ConnDisplay hardcode the fixed ACL user "default"), dragonflyDriver reports
// "default" explicitly: the create handler persists this into the instance
// row's Superuser column for display, even though DragonFly's own
// LinkValue/ConnDisplay (inherited from redisDriver) still use the same
// hardcoded "default" internally, not inst.Superuser.
func (dragonflyDriver) SuperuserName() string { return "default" }

// BuildSpec mirrors redisDriver.BuildSpec but with the DragonFly image's own
// CMD shape: exec form (no shell) so the password is a discrete argv element,
// same as redis-server.
func (d dragonflyDriver) BuildSpec(inst Instance, network string) docker.ServiceSpec {
	spec := docker.ServiceSpec{
		Name:        inst.AppName,
		Image:       inst.Image,
		Replicas:    1,
		Network:     network,
		DNSRR:       true,
		Constraints: []string{DBConstraint(inst.NodeHostname)},
	}
	spec.Args = []string{"dragonfly", "--requirepass", inst.SuperuserPassword}
	spec.Mounts = []docker.MountSpec{{Type: "volume", Source: volumeName(inst.AppName), Target: d.MountTarget()}}
	return spec
}
