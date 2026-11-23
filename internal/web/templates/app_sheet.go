package templates

import (
	"context"
	"path"
	"strings"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/web/i18n"
)

// primaryPublicURL is the address the sheet links to: the primary domain
// when the app is exposed.
func primaryPublicURL(domains []db.Domain, exposed bool, primary string) string {
	if !exposed {
		return ""
	}
	for _, d := range domains {
		if d.IsPrimary && d.Exposed && !d.RedirectToPrimary {
			scheme := "http://"
			if d.Tls {
				scheme = "https://"
			}
			return scheme + d.Host + firstPath(d.Paths)
		}
	}
	if primary != "" {
		return "http://" + primary
	}
	return ""
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

// splitDeployments picks what the deployments tab calls out: the newest
// deployment when it failed while the app is in error, the deployment that
// is serving (or the newest one), and the history after it.
func splitDeployments(appStatus string, deps []db.ListDeploymentSummariesByApplicationRow) (failed, active *db.ListDeploymentSummariesByApplicationRow, rest []db.ListDeploymentSummariesByApplicationRow) {
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
			return failed, active, rest
		}
	}
	if failed == nil && len(deps) > 0 {
		active = &deps[0]
		rest = deps[1:]
	} else if failed != nil {
		rest = deps[1:]
	}
	return failed, active, rest
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
