package templates

import (
	"context"
	"path"
	"slices"

	"github.com/a-h/templ"
	"strings"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/web/i18n"
)

// primaryPublicURL is the address the sheet links to: a domain that is
// exposed and serves the app (not a redirect), the primary one when it
// qualifies, with that domain's own scheme. An internal primary is never
// linked: Traefik does not route it.
func primaryPublicURL(domains []db.Domain, exposed bool) string {
	if !exposed {
		return ""
	}
	var pick *db.Domain
	for i, d := range domains {
		if !d.Exposed || d.RedirectToPrimary {
			continue
		}
		if d.IsPrimary {
			pick = &domains[i]
			break
		}
		if pick == nil {
			pick = &domains[i]
		}
	}
	if pick == nil {
		return ""
	}
	scheme := "http://"
	if pick.Tls {
		scheme = "https://"
	}
	return scheme + pick.Host + firstPath(pick.Paths)
}

func firstPath(csv string) string {
	if i := strings.IndexAny(csv, ",\n"); i >= 0 {
		csv = csv[:i]
	}
	return strings.TrimSpace(csv)
}

func trimScheme(u string) string {
	u = strings.TrimPrefix(u, "https://")
	return strings.TrimPrefix(u, "http://")
}

func taskKind(state string) string {
	switch state {
	case "running":
		return "ok"
	case "failed", "rejected":
		return "err"
	case "starting", "preparing", "assigned", "pending", "accepted", "ready", "new", "allocated":
		return "warn"
	}
	return "idle"
}

func taskLabel(ctx context.Context, state string) string {
	switch taskKind(state) {
	case "ok":
		return i18n.T(ctx, "task.running")
	case "err":
		return i18n.T(ctx, "task.failed")
	case "warn":
		return i18n.T(ctx, "task.starting")
	}
	return i18n.T(ctx, "task.stopped")
}

func placementText(ctx context.Context, c AppCtx) string {
	switch c.App.PlacementMode {
	case "pin":
		return i18n.T(ctx, "placement.text_pin")
	case "global":
		return i18n.T(ctx, "placement.text_global")
	}
	return i18n.T(ctx, "placement.text_any")
}

// splitDeployments picks what the deployments tab calls out: a deployment
// still in progress (shown apart: until it completes, the release before it
// keeps serving), the newest deployment when it failed while the app is in
// error, the deployment that is serving (or the newest one), and the history
// after it.
func splitDeployments(appStatus string, deps []db.ListDeploymentSummariesByApplicationRow) (running, failed, active *db.ListDeploymentSummariesByApplicationRow, rest []db.ListDeploymentSummariesByApplicationRow) {
	if len(deps) > 0 && deps[0].Status == "running" {
		running = &deps[0]
		deps = deps[1:]
	}
	i := 0
	if len(deps) > 0 && deps[0].Status == "error" && appStatus == "error" {
		failed = &deps[0]
		i = 1
	}
	for j := i; j < len(deps); j++ {
		if deps[j].Status != "error" {
			active = &deps[j]
			rest = append(rest, deps[i:j]...)
			rest = append(rest, deps[j+1:]...)
			return running, failed, active, rest
		}
	}
	if failed == nil && len(deps) > 0 {
		active = &deps[0]
		rest = deps[1:]
	} else if failed != nil {
		rest = deps[1:]
	}
	return running, failed, active, rest
}

func deployWhen(ctx context.Context, d db.ListDeploymentSummariesByApplicationRow) string {
	t := d.StartedAt
	if d.FinishedAt.Valid {
		t = d.FinishedAt.Time
	}
	return i18n.Tf(ctx, "deploy.when_by", ago(ctx, t), triggerLabel(ctx, d.Trigger))
}

func triggerLabel(ctx context.Context, trigger string) string {
	switch trigger {
	case "manual", "webhook", "schedule", "api", "system":
		return i18n.T(ctx, "deploy.trigger_"+trigger)
	}
	return trigger
}

func deployTagLabel(ctx context.Context, status string) string {
	switch status {
	case "done":
		return i18n.T(ctx, "deploy.done")
	case "running":
		return i18n.T(ctx, "deploy.running")
	case "error":
		return i18n.T(ctx, "deploy.failed")
	}
	return status
}

// historyTagLabel labels a release in the history: a successful one there
// has been replaced by a newer release, so it is "removed", not "active".
func historyTagLabel(ctx context.Context, status string) string {
	if status == "done" {
		return i18n.T(ctx, "deploy.removed")
	}
	return deployTagLabel(ctx, status)
}

func deployState(ctx context.Context, status string) (string, string) {
	switch status {
	case "done":
		return "ok", i18n.T(ctx, "deploy.done")
	case "running":
		return "warn", i18n.T(ctx, "deploy.running")
	case "error":
		return "err", i18n.T(ctx, "deploy.failed")
	}
	return "idle", status
}

// logID turns a log WebSocket path into an id-safe suffix.
func logID(path string) string {
	return strings.NewReplacer("/", "-", "?", "-", "=", "-").Replace(strings.Trim(path, "/"))
}

func tlsButton(ctx context.Context, on bool) string {
	if on {
		return i18n.T(ctx, "domains.tls_turn_off")
	}
	return i18n.T(ctx, "domains.tls_turn_on")
}

// engineLabel is an engine's name as people say it.
func engineLabel(engine string) string {
	switch engine {
	case "postgres":
		return "Postgres"
	case "redis":
		return "Redis"
	case "dragonfly":
		return "DragonFly"
	case "valkey":
		return "Valkey"
	case "minio":
		return "S3"
	}
	return engine
}

// archiveName is the file name of a stored backup: what tells two archives
// of the same minute and size apart.
func archiveName(key string) string {
	return path.Base(key)
}

// shortImageRef shortens a digest-pinned image reference for a card:
// "repo@sha256:<64 hex>" becomes "repo@sha256:<12 hex>…" (the full reference
// goes into the title). A 64-character hash otherwise breaks mid-way onto a
// second line. Anything else is returned unchanged.
func shortImageRef(ref string) string {
	const marker = "@sha256:"
	i := strings.Index(ref, marker)
	if i < 0 || len(ref)-(i+len(marker)) <= 12 {
		return ref
	}
	return ref[:i+len(marker)+12] + "…"
}

// nodeName is the name the canvas and the nodes page give a node:
// "control-plane" for the manager, a label, or the hostname.
func (c AppCtx) nodeName(id, hostname string) string {
	if n := c.NodeNames[id]; n != "" {
		return n
	}
	return nodeDisplay(c.NodeLabels, id, hostname)
}

// nodeNameByHost is nodeName for a record that carries only the hostname
// (a task from the history).
func (c AppCtx) nodeNameByHost(hostname string) string {
	for _, n := range c.Nodes {
		if n.Hostname == hostname {
			return c.nodeName(n.ID, n.Hostname)
		}
	}
	return hostname
}

// connHintKey says how apps get a server's connection: a Redis-like server's
// details are what a link injects; a Postgres link injects its logical
// database's own user, never the superuser; on an S3 server a direct link's
// access_key/secret_key are these root keys, while a bucket link injects the
// bucket's own key.
func connHintKey(engine string) string {
	switch engine {
	case "postgres":
		return "dbi.conn_hint_pg"
	case "minio":
		return "dbi.conn_hint_s3"
	}
	return "dbi.conn_hint"
}

// PendingView is what an app's saved settings would change if deployed now
// (deploy.Pending): the changed sections (deploy.Sections names, in display
// order) and whether "apply settings" can deploy them on the running image.
type PendingView struct {
	Changed  []string
	CanApply bool
}

// Any reports whether some section changed.
func (p PendingView) Any() bool { return len(p.Changed) > 0 }

// Has reports whether any of sections changed.
func (p PendingView) Has(sections ...string) bool {
	for _, c := range p.Changed {
		if slices.Contains(sections, c) {
			return true
		}
	}
	return false
}

// tabSections are the settings sections a sheet tab edits.
func tabSections(tab string) []string {
	switch tab {
	case "variables":
		return []string{"env"}
	case "metrics":
		return []string{"metrics"}
	case "settings":
		return []string{"source", "runtime", "predeploy", "volumes", "ports", "placement"}
	}
	return nil
}

// pendingNames names changed sections for a sentence ("variables, volumes").
func pendingNames(ctx context.Context, sections []string) string {
	names := make([]string, len(sections))
	for i, s := range sections {
		names[i] = i18n.T(ctx, "pending.section."+s)
	}
	return strings.Join(names, ", ")
}

// appElemID is the DOM id of an app's card on the environment canvas.
func appElemID(appID int64) string { return "s-app-" + itoa(appID) }

// swapOOB marks an element of a poll response for an out-of-band swap. The
// value must be explicit: htmx reads a bare hx-swap-oob as no swap at all.
func swapOOB(oob bool) templ.Attributes {
	if oob {
		return templ.Attributes{"hx-swap-oob": "true"}
	}
	return templ.Attributes{}
}
