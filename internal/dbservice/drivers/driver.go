// Package drivers holds per-engine knowledge (image/spec/ports/connection/link)
// for managed DB instances. Leaf package: imports only docker (+ stdlib) so
// both dbservice and deploy can depend on it without an import cycle.
package drivers

import "github.com/proshik/krill/internal/docker"

// Instance is the canonical managed-instance shape (dbservice.Instance aliases this).
type Instance struct {
	ID                  int64
	OrganizationID      int64
	Engine              string
	Name                string
	AppName             string // Swarm service name = overlay DNS host = volume prefix
	Image               string
	Superuser           string
	SuperuserPassword   string // DECRYPTED at the dbservice/store boundary
	ExternalPort        *int32
	ConsoleExternalPort *int32 // minio console (:9001); nil otherwise
	Status              string
	NodeHostname        string
}

// ProxyTarget is one external-access forward: publish HostPort on the manager,
// forward over the overlay to ContainerPort of the instance's service. Suffix
// disambiguates multiple proxies for one instance ("" primary, "-console" minio).
type ProxyTarget struct {
	Suffix        string
	HostPort      *int32
	ContainerPort uint32
}

// ConnField is one masked connection-detail row for the instance detail page.
type ConnField struct {
	Label  string // i18n key
	Value  string
	Secret bool // render masked + reveal/copy
}

// LinkSource is the resolved instance identity for app-link field extraction.
type LinkSource struct {
	AppName   string
	Superuser string
	Password  string // decrypted
	DBName    string // logical DB name (postgres) or ""
	Scheme    string // caller-chosen (postgres/postgresql/redis)
}

// Driver is the per-engine strategy for a managed DB instance: image/spec,
// external-access ports, app-link field extraction, and connection display.
type Driver interface {
	Engine() string
	Label() string
	DefaultImage() string
	SuperuserName() string // default superuser stored on create ("" if engine has none)
	MountTarget() string   // volume mount path
	BuildSpec(inst Instance, network string) docker.ServiceSpec
	ExternalTargets(inst Instance) []ProxyTarget
	HasLogicalResource() bool // postgres true (logical DBs); others false (v1)
	LinkFields() []string     // app-link fields this engine exposes
	LinkValue(src LinkSource, field string) (string, bool)
	ConnDisplay(inst Instance, controlPlaneHost string) []ConnField
}

// volumeName is the named-volume convention shared by every driver.
func volumeName(appName string) string { return appName + "-data" }

// VolumeName is the exported form of volumeName, for dbservice callers that
// need the volume name outside of BuildSpec (e.g. volume removal on delete).
func VolumeName(appName string) string { return volumeName(appName) }

// DBConstraint pins a managed DB to a chosen node, or to the control-plane
// (manager) when none is selected. The named volume lives on that node.
func DBConstraint(nodeHostname string) string {
	if nodeHostname != "" {
		return "node.hostname==" + nodeHostname
	}
	return "node.role==manager"
}

// URLString builds a scheme://user:pass@host:port[/dbname] connection string —
// the single place engine connection-string formatting lives.
func URLString(scheme, user, pass, host, port, dbname string) string {
	u := scheme + "://" + user + ":" + pass + "@" + host + ":" + port
	if dbname != "" {
		u += "/" + dbname
	}
	return u
}

// fieldValue extracts one field of a connection from its parts. An
// empty/unknown field yields the full connection URL (backward-compatible
// default) — mirrors the field semantics app-link injection has always used.
func fieldValue(field, user, pass, host, port, dbname, scheme string) string {
	switch field {
	case "password":
		return pass
	case "host":
		return host
	case "port":
		return port
	case "user":
		return user
	case "dbname":
		return dbname
	case "hostport":
		return host + ":" + port
	default: // "url"
		return URLString(scheme, user, pass, host, port, dbname)
	}
}

// LinkValue is the pure dispatch helper: resolve field for engine's driver,
// or ("", false) when the engine is unknown.
func LinkValue(engine string, src LinkSource, field string) (string, bool) {
	d, ok := Registry.Get(engine)
	if !ok {
		return "", false
	}
	return d.LinkValue(src, field)
}
