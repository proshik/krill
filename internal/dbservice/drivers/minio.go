package drivers

import (
	"strconv"

	"github.com/proshik/krill/internal/docker"
)

// minioLinkFields — app-link fields the minio engine exposes: an S3 endpoint
// URL plus the access/secret key pair and a fixed region (minio doesn't
// enforce regions, but the S3 SDKs require one to be set).
var minioLinkFields = []string{"endpoint", "access_key", "secret_key", "region"}

// minioRegion — minio doesn't validate/enforce AWS regions, but every S3 SDK
// requires one to be configured; "us-east-1" is the conventional default.
const minioRegion = "us-east-1"

type minioDriver struct{}

func (minioDriver) Engine() string       { return "minio" }
func (minioDriver) Label() string        { return "MinIO" }
func (minioDriver) DefaultImage() string { return MinIOImage }

// MinIOImage is the image a new MinIO instance runs. Docker Hub removed the
// minio/minio repository, so the image comes from quay.io, where MinIO still
// publishes it. The tag is pinned rather than "latest": MinIO has stopped
// publishing new community builds, so "latest" is frozen at this release anyway,
// and naming it keeps what an instance runs visible in the UI and reproducible.
// Migration 000044 moves instances created earlier off the Docker Hub reference.
const MinIOImage = "quay.io/minio/minio:RELEASE.2025-09-07T16-13-09Z"

// SuperuserName — unlike postgres ("postgres") and redis ("default"), MinIO's
// root user is USER-CHOSEN at create time (MINIO_ROOT_USER); the driver has
// no fixed identity to report. The create handler reads a "root_user" form
// field and stores it into the instance row's Superuser column itself;
// BuildSpec reads it back via inst.Superuser.
func (minioDriver) SuperuserName() string { return "" }

func (minioDriver) MountTarget() string { return "/data" }

// BuildSpec: the minio image's ENTRYPOINT is docker-entrypoint.sh, which
// execs its CMD/Args as given — passing ["server","/data",...] as Args yields
// `minio server /data --console-address :9001`, serving the S3 API on :9000
// and the web console on :9001. Healthcheck is intentionally OMITTED in v1:
// the minio image may not ship a shell/curl, and verifying that by pulling
// the image is disk/network-costly here — rely on Swarm convergence instead.
func (d minioDriver) BuildSpec(inst Instance, network string) docker.ServiceSpec {
	spec := docker.ServiceSpec{
		Name:        inst.AppName,
		Image:       inst.Image,
		Replicas:    1,
		Network:     network,
		DNSRR:       true,
		Constraints: []string{DBConstraint(inst.NodeHostname)},
		// UpdateStopFirst: single-writer protection. Unlike postgres/redis/
		// dragonfly, MinIO gets stop-first only — its image has no `flock`, so
		// it doesn't get the directory-lock half of the protection (accepted
		// gap, see CLAUDE.md §3 and docs/architecture.md's "Nodes" section).
		UpdateStopFirst: true,
	}
	spec.Args = []string{"server", "/data", "--console-address", ":9001"}
	spec.Env = map[string]string{
		"MINIO_ROOT_USER":     inst.Superuser,
		"MINIO_ROOT_PASSWORD": inst.SuperuserPassword,
	}
	spec.Mounts = []docker.MountSpec{{Type: "volume", Source: volumeName(inst.AppName), Target: d.MountTarget()}}
	return spec
}

// ExternalTargets — minio is the first two-target engine: the S3 API (:9000)
// and the web console (:9001) are independent published ports, proxied
// separately ("" and "-console" suffixes).
func (minioDriver) ExternalTargets(inst Instance) []ProxyTarget {
	return []ProxyTarget{
		{Suffix: "", HostPort: inst.ExternalPort, ContainerPort: 9000},
		{Suffix: "-console", HostPort: inst.ConsoleExternalPort, ContainerPort: 9001},
	}
}

func (minioDriver) HasLogicalResource() bool { return false }

func (minioDriver) LinkFields() []string { return minioLinkFields }

// LinkValue — an app link always resolves to the internal (overlay) S3
// endpoint; access/secret key mirror the instance's root credentials.
func (minioDriver) LinkValue(src LinkSource, field string) (string, bool) {
	switch field {
	case "endpoint":
		return "http://" + src.AppName + ":9000", true
	case "access_key":
		return src.Superuser, true
	case "secret_key":
		return src.Password, true
	case "region":
		return minioRegion, true
	default:
		return "", false
	}
}

// ConnDisplay renders the instance's S3 endpoint + credentials for the
// DB-server detail page's "Connection" panel. The internal endpoint and
// (if published) external API/console URLs are plain host references — the
// access/secret keys are the actual credential fields, so only the secret
// key is masked.
func (minioDriver) ConnDisplay(inst Instance, controlPlaneHost string) []ConnField {
	fields := []ConnField{
		{Label: "dbi.s3_endpoint", Value: "http://" + inst.AppName + ":9000"},
	}
	if inst.ExternalPort != nil {
		fields = append(fields, ConnField{Label: "dbi.s3_external_api", Value: controlPlaneHost + ":" + strconv.Itoa(int(*inst.ExternalPort))})
	}
	if inst.ConsoleExternalPort != nil {
		fields = append(fields, ConnField{Label: "dbi.s3_console_url", Value: "http://" + controlPlaneHost + ":" + strconv.Itoa(int(*inst.ConsoleExternalPort))})
	}
	fields = append(fields,
		ConnField{Label: "dbi.s3_access_key", Value: inst.Superuser},
		ConnField{Label: "dbi.s3_secret_key", Value: inst.SuperuserPassword, Secret: true},
	)
	return fields
}
