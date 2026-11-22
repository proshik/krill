package templates

import (
	"context"
	"strconv"
	"strings"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/topology"
	"github.com/proshik/krill/internal/web/i18n"
)

// CanvasCard is one card on the environment canvas: an app or a resource.
type CanvasCard struct {
	Key       string // canvas key ("app:12", "ldb:3", "bucket:4", "inst:5")
	ElemID    string // DOM id the wires address ("s-app-12")
	Glyph     string // sprite id
	Name      string
	Sub       string
	SubLink   bool // Sub is a public address
	StateID   string
	StateKind string
	StateText string
	Nodes     []string
	Href      string
	To        []string // ElemIDs of the resources the app uses
	Detected  []string // the subset of To only found in env values
}

// CanvasNode is one cluster node in the canvas node filter.
type CanvasNode struct {
	Name  string
	Count int
	Warn  string // pressure label for instance operators, "" when fine
}

// CanvasFailing is the note over the canvas about an app whose deploy failed.
type CanvasFailing struct {
	Name string
	Href string
	When time.Time
}

// CanvasView is everything the canvas draws.
type CanvasView struct {
	Title     string
	Plane     topology.Canvas
	Cards     map[string]CanvasCard
	Nodes     []CanvasNode
	Failing   *CanvasFailing
	StatusURL string
	Current   string // key of the card whose sheet is open
	Ghost     bool   // a new app is being created
	Empty     bool   // no apps and no resources
	NewAppURL string // where "new app" goes, "" for members
	// A freshly cloned environment asks for a review before its first deploy.
	Cloned, ClonePending bool
	SettingsURL          string
}

func canvasStyle(x, y int) string {
	return "--x:" + strconv.Itoa(x) + "px;--y:" + strconv.Itoa(y) + "px"
}

func groupStyle(g topology.CanvasGroup) string {
	return "--x:" + strconv.Itoa(g.X) + "px;--y:" + strconv.Itoa(g.Y) + "px;--w:" + strconv.Itoa(g.W) + "px;--h:" + strconv.Itoa(g.H) + "px"
}

func planeStyle(c topology.Canvas) string {
	return "--plane-w:" + strconv.Itoa(c.W) + "px;--plane-h:" + strconv.Itoa(c.H) + "px"
}

// ghostY places the new app's ghost card under the loose group's last card.
func ghostY(g topology.CanvasGroup) int {
	y := 64
	for _, it := range g.Items {
		if it.X < 200 && it.Y+114 > y {
			y = it.Y + 114
		}
	}
	return y
}

func ghostGroupStyle(g topology.CanvasGroup) string {
	h := g.H
	if gy := ghostY(g) + 114 + 8; gy > h {
		h = gy
	}
	return "--x:" + strconv.Itoa(g.X) + "px;--y:" + strconv.Itoa(g.Y) + "px;--w:" + strconv.Itoa(g.W) + "px;--h:" + strconv.Itoa(h) + "px"
}

func looseGroup(c topology.Canvas) (topology.CanvasGroup, bool) {
	for _, g := range c.Groups {
		if g.Loose {
			return g, true
		}
	}
	return topology.CanvasGroup{}, false
}

// ghostOnlyStyle is the loose group drawn just to hold the ghost card when
// the environment has nothing loose.
func ghostOnlyStyle(c topology.Canvas) string {
	x := 0
	if len(c.Groups) > 0 {
		x = 760
	}
	return "--x:" + strconv.Itoa(x) + "px;--y:0px;--w:336px;--h:186px"
}

func nodeCountLabel(n CanvasNode) string { return strconv.Itoa(n.Count) }

func joinSpace(s []string) string { return strings.Join(s, " ") }

// ago is a short relative time: "just now", "12 min ago", "3 h ago", "2 d ago".
func ago(ctx context.Context, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return i18n.T(ctx, "time.just_now")
	case d < time.Hour:
		return i18n.Tf(ctx, "time.min_ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return i18n.Tf(ctx, "time.h_ago", int(d/time.Hour))
	default:
		return i18n.Tf(ctx, "time.d_ago", int(d/(24*time.Hour)))
	}
}

// AppState maps a stored or derived app status to a signal kind and label.
func AppState(ctx context.Context, status string) (string, string) {
	switch status {
	case "running":
		return "ok", i18n.T(ctx, "state.online")
	case "deploying":
		return "warn", i18n.T(ctx, "state.deploying")
	case "error":
		return "err", i18n.T(ctx, "state.deploy_failed")
	default:
		return "idle", i18n.T(ctx, "state.stopped")
	}
}

// ServerState maps a database server's status to a signal kind and label.
func ServerState(ctx context.Context, status string) (string, string) {
	switch status {
	case "running":
		return "ok", i18n.T(ctx, "state.online")
	case "deploying", "migrating":
		return "warn", i18n.T(ctx, "state."+status)
	case "error":
		return "err", i18n.T(ctx, "state.error")
	default:
		return "idle", i18n.T(ctx, "state.stopped")
	}
}

func joinComma(s []string) string { return strings.Join(s, ", ") }

// EnvResourceRow is one database, bucket or linked server on the
// environment's databases page.
type EnvResourceRow struct {
	Key        string
	Glyph      string
	Name       string
	KindLabel  string
	Href       string // its own page, "" for a whole server
	StateKind  string
	StateText  string
	Server     string
	ServerHref string
	Node       string
	Detail     string // e.g. "database shop, user shop"
	Users      []UsedBy
	BackupKind string // "" when the kind has no backups
	BackupText string
	BackupHref string
}

// UsedBy is an app that uses a resource, with the variables it gets.
type UsedBy struct {
	Name string
	Href string
	Note string
}

// EnvDatabasesView is the environment's databases page.
type EnvDatabasesView struct {
	Rows    []EnvResourceRow
	PgInsts []db.DbInstance
	S3Insts []db.DbInstance
	Sheet   string // "", "database", "bucket"
}

// EnvSettingsView is the environment's settings page.
type EnvSettingsView struct {
	View       topology.EnvView
	Envs       []db.Environment
	RequestKey string // clone request identity, "" when cloning is unavailable
	Cloned     bool
	Pending    bool
}
