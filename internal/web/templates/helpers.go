package templates

import (
	"context"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/proshik/krill/internal/api"
	"github.com/proshik/krill/internal/backup"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice/drivers"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/topology"
	"github.com/proshik/krill/internal/web/i18n"
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

// statusLabel prettifies computed status values for display. Badges render the
// raw status string (English by convention); the node-unavailable states get a
// friendlier label.
func statusLabel(s string) string {
	switch s {
	case "node_down":
		return "node down"
	case "node_removed":
		return "node removed"
	default:
		return s
	}
}

// dbiStatus returns the computed display status for an instance, falling back to
// its stored status when no computed entry exists (engine unavailable).
func dbiStatus(statuses map[int64]string, in db.DbInstance) string {
	if s, ok := statuses[in.ID]; ok {
		return s
	}
	return in.Status
}

// dbEngineIcon returns the display icon for a db_instances engine value.
func dbEngineIcon(engine string) string {
	switch engine {
	case "postgres":
		return "🐘"
	case "redis":
		return "🟥"
	case "dragonfly":
		return "🪰"
	case "valkey":
		return "🔑"
	case "minio":
		return "🪣"
	default:
		return "🗄"
	}
}

// firstEngineDefaultImage returns the default image of the first registered
// driver — used as the create-form version field's initial placeholder
// (matches the engine select's default option; kept in sync by
// krillToggleEngine on change).
func firstEngineDefaultImage() string {
	list := drivers.Registry.List()
	if len(list) == 0 {
		return ""
	}
	return list[0].DefaultImage()
}

// hostnameInNodes reports whether hostname matches a live swarm node.
func hostnameInNodes(nodes []docker.SwarmNode, hostname string) bool {
	for _, n := range nodes {
		if n.Hostname == hostname {
			return true
		}
	}
	return false
}

// orphanedPlacementNodes returns the swarm IDs in a placement_nodes CSV that are
// no longer present in the live node list.
func orphanedPlacementNodes(csv string, nodes []docker.SwarmNode) []string {
	live := map[string]bool{}
	for _, n := range nodes {
		live[n.ID] = true
	}
	var out []string
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" && !live[p] {
			out = append(out, p)
		}
	}
	return out
}

// scheduleLabel renders a stored cron schedule as a friendly preset name (or the
// raw cron for a custom schedule).
func scheduleLabel(ctx context.Context, schedule string) string {
	p := backup.SchedulePreset(schedule)
	if p == "custom" {
		return schedule
	}
	return i18n.T(ctx, "backup.sched."+p)
}

// tokenPrefixDisplay renders an API token's stored lookup prefix (8 chars,
// no marker) back into its recognizable form, e.g. "krill_pat_a1b2c3d4…". The
// full token is never stored, so this is deliberately NOT enough to
// reconstruct it — just enough for a human to tell tokens apart in the list.
func tokenPrefixDisplay(prefix string) string {
	return api.TokenPrefix + prefix + "…"
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

// onOff is the form value of a toggle's desired state: "1" on, "0" off.
func onOff(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func envBase(org db.Organization, project db.Project, env db.Environment) string {
	return "/orgs/" + itoa(org.ID) + "/projects/" + itoa(project.ID) + "/environments/" + itoa(env.ID)
}

func joinIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = itoa(id)
	}
	return strings.Join(parts, " ")
}

// systemSummary is a system card's count line: apps, running, failing.
func systemSummary(ctx context.Context, sy topology.SystemView) string {
	s := i18n.Tf(ctx, "system.summary", len(sy.Apps), sy.Running)
	if sy.Failing > 0 {
		s += " · " + i18n.Tf(ctx, "system.failing", sy.Failing)
	}
	return s
}

// collapsedResources is the one-line resource list a collapsed card shows.
func collapsedResources(ctx context.Context, sy topology.SystemView) string {
	parts := make([]string, 0, len(sy.Resources))
	for _, r := range sy.Resources {
		p := resourceKind(r) + " " + resourceLabel(r)
		if r.Shared {
			p += " ⇄ " + i18n.T(ctx, "system.shared")
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " · ")
}

func resourceKind(r topology.ResourceView) string {
	switch r.Kind {
	case "ldb":
		return "PG"
	case "bucket":
		return "S3"
	}
	switch r.Engine {
	case "redis":
		return "Redis"
	case "valkey":
		return "Valkey"
	case "dragonfly":
		return "DragonFly"
	case "minio":
		return "S3"
	}
	return r.Engine
}

func resourceLabel(r topology.ResourceView) string {
	if r.Kind == "instance" {
		return r.Label
	}
	return r.Label + " @ " + r.Server
}

func resourceIcon(r topology.ResourceView) string {
	if r.Kind == "bucket" {
		return "🪣"
	}
	return dbEngineIcon(r.Engine)
}

// systemCandidates are the environment's apps not in system sysID: the ones
// "Add app" can move into it.
func systemCandidates(view topology.EnvView, sysID int64) []topology.AppView {
	var out []topology.AppView
	for _, s := range view.Systems {
		if s.ID != sysID {
			out = append(out, s.Apps...)
		}
	}
	return append(out, view.Ungrouped.Apps...)
}

// resourceAria is a resource chip's accessible name: what it shows, its
// badges, then the variables it feeds (the chip's title carries only those).
func resourceAria(ctx context.Context, r topology.ResourceView) string {
	s := resourceKind(r) + " " + resourceLabel(r)
	if r.Shared {
		s += ", " + i18n.T(ctx, "system.shared")
	}
	if r.Detected {
		s += ", " + i18n.T(ctx, "system.detected")
	}
	if len(r.Vars) > 0 {
		s += ": " + strings.Join(r.Vars, ", ")
	}
	return s
}
