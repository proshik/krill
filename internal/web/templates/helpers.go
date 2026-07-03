package templates

import (
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/proshik/krill/internal/database/gen"
)

// itoa formats an int64 for interpolation into URLs inside templates.
func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// placementHas reports whether a swarm node ID is in the app's CSV placement set.
func placementHas(csv, nodeID string) bool {
	for _, p := range strings.Split(csv, ",") {
		if strings.TrimSpace(p) == nodeID {
			return true
		}
	}
	return false
}

// nodeManagedName returns the Krill-managed name for a Swarm node ID, or "" if
// the node was not added through Krill (e.g. the manager itself).
func nodeManagedName(rows []db.ClusterNode, swarmID string) string {
	for _, r := range rows {
		if r.SwarmNodeID != "" && r.SwarmNodeID == swarmID {
			return r.Name
		}
	}
	return ""
}

// nodeDisplay returns the human-readable display label for a Swarm node ID,
// falling back to the raw hostname when no label is set.
func nodeDisplay(labels map[string]string, swarmID, hostname string) string {
	if l := labels[swarmID]; l != "" {
		return l
	}
	return hostname
}

// basicAuthUsernames extracts the usernames from a newline-separated htpasswd
// "user:hash" list. Only usernames are exposed to the UI; hashes are never shown.
func basicAuthUsernames(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if e := strings.TrimSpace(line); e != "" {
			out = append(out, strings.SplitN(e, ":", 2)[0])
		}
	}
	return out
}

// strv dereferences a *string into a value, returning "" when nil.
func strv(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ownerValue returns the volume owner string for an input value ("" when unset).
func ownerValue(v db.AppVolume) string {
	if v.Owner == nil {
		return ""
	}
	return *v.Owner
}

// i32v formats a *int32 as a decimal string, returning "" when nil.
func i32v(p *int32) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(int(*p))
}

// destName returns the Name of the destination with the given id, or "" if none.
func destName(dests []db.Destination, id int64) string {
	for _, d := range dests {
		if d.ID == id {
			return d.Name
		}
	}
	return ""
}

// tsStr formats a timestamptz as "2006-01-02 15:04", or "—" if not set.
func tsStr(t pgtype.Timestamptz) string {
	if !t.Valid {
		return "—"
	}
	return t.Time.Format("2006-01-02 15:04")
}

// humanSize renders a byte count as a short human-readable string.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + string("KMGTPE"[exp]) + "iB"
}
