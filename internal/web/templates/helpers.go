package templates

import (
	"context"
	"strconv"
	"strings"

	"github.com/proshik/krill/internal/api"
	"github.com/proshik/krill/internal/backup"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
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

// limitText is an effective limit with where it comes from.
func limitText(ctx context.Context, value, source string) string {
	switch source {
	case "explicit":
		return value
	case "default":
		return i18n.Tf(ctx, "runtime.limit_default", value)
	}
	return i18n.T(ctx, "runtime.limit_none")
}

// limitPlaceholder says what a blank limit field means: the instance default
// when one is configured, otherwise no limit.
func limitPlaceholder(ctx context.Context, example, def string) string {
	if def != "" {
		return i18n.Tf(ctx, "runtime.placeholder_default", example, def)
	}
	return i18n.Tf(ctx, "runtime.placeholder_none", example)
}

// taskDetail is a task's own message, error and exit code in one line.
func taskDetail(t docker.TaskRecord) string {
	parts := []string{}
	if t.Err != "" {
		parts = append(parts, t.Err)
	} else if t.Message != "" {
		parts = append(parts, t.Message)
	}
	if t.ExitCode != 0 {
		parts = append(parts, "exit "+strconv.Itoa(t.ExitCode))
	}
	return strings.Join(parts, " · ")
}

// taskHint explains the task states that are easy to misread.
func taskHint(ctx context.Context, t docker.TaskRecord) string {
	switch {
	case t.ExitCode == 137:
		return i18n.T(ctx, "runtime.hint_137")
	case t.State == "starting":
		return i18n.T(ctx, "runtime.hint_starting")
	}
	return ""
}

// connWarnings are the connection-budget situations worth a closer look.
func connWarnings(ctx context.Context, c dbservice.ConnStats) []string {
	var out []string
	if c.Max > 0 && c.Total*100 >= c.Max*80 {
		out = append(out, i18n.Tf(ctx, "conn.warn_budget", c.Total, c.Max))
	}
	if c.IdleInTx > 0 && c.OldestIdleTxS >= 60 {
		out = append(out, i18n.Tf(ctx, "conn.warn_idle_tx", c.IdleInTx, c.OldestIdleTxS))
	}
	if c.AdvisoryLocks > 0 {
		out = append(out, i18n.Tf(ctx, "conn.warn_advisory", c.AdvisoryLocks))
	}
	return out
}

// humanBytes is a byte count in binary units.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + string("KMGTPE"[exp]) + "iB"
}

// restoreName suggests a name for the bucket a snapshot is restored into.
func restoreName(bucket string) string {
	name := bucket + "-restored"
	if len(name) > 63 {
		name = name[len(name)-63:]
	}
	return strings.TrimLeft(name, "-")
}
