package link

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/proshik/krill/internal/panel"
)

// CoreRelease is the first release that contains the link core (KRL-001's
// merge, 435dc53): its deployer keeps an app's krill-egress-* attachment and
// its listener refuses egress streams, so a rollback onto it fails closed.
// Egress may be enabled only while the rollback binary is at least this
// release, or there is none.
const CoreRelease = "v0.17.0"

// EgressNetworkPrefix names the per-app egress networks; it must stay the
// deployer's prefix (deploy.EgressNetworkPrefix).
const EgressNetworkPrefix = "krill-egress-"

// EgressNetwork is the egress network of one app.
func EgressNetwork(appID int64) string {
	return EgressNetworkPrefix + strconv.FormatInt(appID, 10)
}

// EgressNetworkApp is the app an egress network belongs to, false for any
// other name.
func EgressNetworkApp(name string) (int64, bool) {
	rest, ok := strings.CutPrefix(name, EgressNetworkPrefix)
	if !ok || rest == "" || rest[0] == '0' {
		return 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// Destination is one host and port an app may reach through the link: an
// exact, normalized DNS name (never an IP or a wildcard) and a TCP port.
type Destination struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// String is host:port.
func (d Destination) String() string { return net.JoinHostPort(d.Host, strconv.Itoa(d.Port)) }

// ErrBadDestination is a destination that is not an exact host name with a
// port in 1..65535.
var ErrBadDestination = errors.New("a destination is an exact host name and a port 1..65535")

// ParseDestination normalizes a host as the panel domain is (lower case, no
// IP, no wildcard) and parses the port with strconv.Atoi.
func ParseDestination(host, port string) (Destination, error) {
	h, err := panel.NormalizeHost(host)
	if err != nil {
		return Destination{}, ErrBadDestination
	}
	p, err := strconv.Atoi(strings.TrimSpace(port))
	if err != nil || p < 1 || p > 65535 {
		return Destination{}, ErrBadDestination
	}
	return Destination{Host: h, Port: p}, nil
}

// ParseDestinationString parses "host:port".
func ParseDestinationString(s string) (Destination, error) {
	h, p, err := net.SplitHostPort(strings.TrimSpace(s))
	if err != nil {
		return Destination{}, ErrBadDestination
	}
	return ParseDestination(h, p)
}

// SortDestinations orders destinations by host, then port, and drops
// duplicates.
func SortDestinations(ds []Destination) []Destination {
	out := slices.Clone(ds)
	slices.SortFunc(out, func(a, b Destination) int {
		if c := strings.Compare(a.Host, b.Host); c != 0 {
			return c
		}
		return a.Port - b.Port
	})
	return slices.Compact(out)
}

// Reasons an egress connection is refused; they are logged, never the app's
// data.
var (
	ErrEgressNoSession   = errors.New("the link session is down")
	ErrEgressNotGranted  = errors.New("the app's organization is not granted the link")
	ErrEgressNotSelected = errors.New("the destination is not in the app's selection")
	ErrEgressNotAllowed  = errors.New("the relay does not allow the destination")
)

// egressFacts are what one egress connection is decided on.
type egressFacts struct {
	session  bool // the link session is up
	granted  bool // the app's organization is granted
	selected bool // (SNI, port) is in the app's saved selection
	relay    bool // (SNI, port) is on the relay's list
}

// decideEgress lets a connection through only when every fact holds.
func decideEgress(f egressFacts) error {
	switch {
	case !f.session:
		return ErrEgressNoSession
	case !f.granted:
		return ErrEgressNotGranted
	case !f.selected:
		return ErrEgressNotSelected
	case !f.relay:
		return ErrEgressNotAllowed
	}
	return nil
}

// RollbackRefusal refuses enabling egress while the rollback binary is older
// than CoreRelease or cannot be read: a rollback onto a release without the
// link core would drop the apps' egress attachments, and their traffic would
// leave directly.
type RollbackRefusal struct {
	// Prev is the rollback binary's version, "" when it cannot be read.
	Prev string
}

func (e *RollbackRefusal) Error() string {
	if e.Prev == "" {
		return fmt.Sprintf("the rollback binary cannot be read; egress needs it to be at least %s", CoreRelease)
	}
	return fmt.Sprintf("the rollback binary is %s, older than %s", e.Prev, CoreRelease)
}

// Kinds of rollback binary RollbackGate understands; they are the values of
// selfupdate.RollbackBinary.Kind.
const (
	RollbackAbsent  = "absent"
	RollbackRelease = "release"
)

// RollbackGate decides whether egress may be enabled given the rollback
// binary: when there is none, or it is at least CoreRelease. Anything else —
// an older release, or a binary whose version is unknown — is a
// *RollbackRefusal (fail closed).
func RollbackGate(kind, version string) error {
	switch kind {
	case RollbackAbsent:
		return nil
	case RollbackRelease:
		if semver.IsValid(version) && semver.Compare(version, CoreRelease) >= 0 {
			return nil
		}
		if semver.IsValid(version) {
			return &RollbackRefusal{Prev: version}
		}
	}
	return &RollbackRefusal{}
}
