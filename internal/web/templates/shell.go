package templates

import (
	"context"
	"github.com/a-h/templ"

	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/web/i18n"
	"github.com/proshik/krill/internal/web/nav"
)

// Shell describes the frame every signed-in page is drawn in: the top bar
// (organization or environment variant), the current tab and the body class
// the design system keys its page layouts on.
type Shell struct {
	Title string
	Org   db.Organization
	Role  string
	// Tab is the current tab: an org tab key ("projects", "db-servers", "nodes",
	// "topology", "monitoring", "settings") or, with Env set, an environment tab
	// key ("canvas", "databases", "settings"). Empty derives the org tab from
	// the request path.
	Tab string
	// Env switches the bar to the environment variant.
	Env *EnvBar
	// Body is the body class: "page" (a document page), "is-canvas" (the
	// environment canvas), optionally with " has-sheet".
	Body string
}

// EnvBar is the environment variant of the bar: project crumb, the
// environment switcher and the environment tabs.
type EnvBar struct {
	Project db.Project
	Env     db.Environment
	Envs    []EnvPick
}

// EnvPick is one entry of the environment switcher.
type EnvPick struct {
	ID     int64
	Name   string
	Health string // ok | err | warn | idle
}

// OrgTab is one tab of the organization bar.
type OrgTab struct {
	Key, Label, Href string
}

func isAdmin(role string) bool { return role == "owner" || role == "admin" }

func orgURL(orgID int64) string { return "/orgs/" + itoa(orgID) }

func projectURL(orgID, projID int64) string {
	return orgURL(orgID) + "/projects/" + itoa(projID)
}

func envURL(orgID, projID, envID int64) string {
	return projectURL(orgID, projID) + "/environments/" + itoa(envID)
}

// orgTabs lists the organization tabs the viewer may open. Instance-wide pages
// (nodes, monitoring) are shown to instance operators only and topology to
// organization admins, matching the route gates.
func orgTabs(ctx context.Context, orgID int64, role string) []OrgTab {
	base := orgURL(orgID)
	tabs := []OrgTab{
		{"projects", i18n.T(ctx, "nav.projects"), base},
		{"db-servers", i18n.T(ctx, "nav.dbservers"), base + "/db-servers"},
	}
	if auth.IsInstanceAdmin(ctx) {
		tabs = append(tabs, OrgTab{"nodes", i18n.T(ctx, "nav.nodes"), base + "/nodes"})
	}
	if isAdmin(role) {
		tabs = append(tabs, OrgTab{"topology", i18n.T(ctx, "nav.topology"), base + "/topology"})
	}
	if auth.IsInstanceAdmin(ctx) {
		tabs = append(tabs, OrgTab{"monitoring", i18n.T(ctx, "nav.monitoring"), base + "/monitoring"})
	}
	tabs = append(tabs, OrgTab{"settings", i18n.T(ctx, "nav.settings"), base + "/settings"})
	return tabs
}

// currentOrgTab returns the org tab to mark current: the shell's own when set,
// otherwise the one the request path belongs to.
func currentOrgTab(ctx context.Context, s Shell) string {
	if s.Env == nil && s.Tab != "" {
		return s.Tab
	}
	return nav.OrgTab(ctx)
}

func envTabs(ctx context.Context, s Shell) []OrgTab {
	base := envURL(s.Org.ID, s.Env.Project.ID, s.Env.Env.ID)
	tabs := []OrgTab{
		{"canvas", i18n.T(ctx, "env.tab_canvas"), base},
		{"databases", i18n.T(ctx, "env.tab_databases"), base + "/databases"},
	}
	if isAdmin(s.Role) {
		tabs = append(tabs, OrgTab{"settings", i18n.T(ctx, "env.tab_settings"), base + "/settings"})
	}
	return tabs
}

func bodyClass(s Shell) string {
	if s.Body == "" {
		return "page"
	}
	return s.Body
}

// toastKind maps a flash kind onto a toast/status kind.
func toastKind(kind string) string {
	switch kind {
	case "ok", "err", "warn":
		return kind
	}
	return "idle"
}

func barTabsLabel(ctx context.Context, s Shell) string {
	if s.Env != nil {
		return i18n.T(ctx, "nav.env_tabs")
	}
	return i18n.T(ctx, "nav.org_tabs")
}

func envHealthOf(b *EnvBar, id int64) string {
	for _, e := range b.Envs {
		if e.ID == id {
			return e.Health
		}
	}
	return "idle"
}

// paletteVals passes the environment the palette was opened in, so the search
// can offer that environment's actions first.
func paletteVals(s Shell) string {
	if s.Env == nil {
		return "{}"
	}
	return `{"env":"` + itoa(s.Env.Env.ID) + `"}`
}

// SettingsGroup is one group of the settings sub-navigation.
type SettingsGroup struct {
	Title string
	Items []OrgTab
	You   bool // the group of the person's own account: ends with sign out
}

// settingsGroups lists the settings pages the viewer may open, grouped as the
// sub-navigation shows them. Notifications and the organization's write pages
// follow the route gates: admins only; the instance pages are for operators.
func settingsGroups(ctx context.Context, orgID int64, role string) []SettingsGroup {
	base := orgURL(orgID)
	org := []OrgTab{{"overview", i18n.T(ctx, "settings.overview"), base + "/settings"}}
	org = append(org, OrgTab{"members", i18n.T(ctx, "nav.members"), base + "/members"})
	org = append(org, OrgTab{"registries", i18n.T(ctx, "reg.link"), base + "/registries"})
	org = append(org, OrgTab{"git", i18n.T(ctx, "gitcred.link"), base + "/git-credentials"})
	org = append(org, OrgTab{"storage", i18n.T(ctx, "dest.link"), base + "/destinations"})
	if isAdmin(role) {
		org = append(org, OrgTab{"notifications", i18n.T(ctx, "notif.link"), base + "/notifications"})
	}
	org = append(org, OrgTab{"tokens", i18n.T(ctx, "api_tokens.link"), base + "/api-tokens"})
	groups := []SettingsGroup{
		{i18n.T(ctx, "settings.group_org"), org, false},
		{i18n.T(ctx, "settings.group_you"), []OrgTab{{"password", i18n.T(ctx, "settings.password"), base + "/account/password"}}, true},
	}
	if auth.IsInstanceAdmin(ctx) {
		groups = append(groups, SettingsGroup{i18n.T(ctx, "settings.group_krill"), []OrgTab{
			{"panel", i18n.T(ctx, "panel.link"), base + "/panel-domain"},
			{"firewall", i18n.T(ctx, "firewall.link"), base + "/firewall"},
			{"observability", i18n.T(ctx, "obs.link"), base + "/observability"},
			{"krill", i18n.T(ctx, "sbk.link"), base + "/krill-backups"},
			{"updates", i18n.T(ctx, "update.link"), base + "/updates"},
		}, false})
	}
	return groups
}

func envBaseURL(s Shell) string { return envURL(s.Org.ID, s.Env.Project.ID, s.Env.Env.ID) }

func envDBURL(s Shell) string { return envBaseURL(s) + "/databases" }

func dashBody(sheet bool) string {
	if sheet {
		return "page has-sheet"
	}
	return "page"
}

// ariaCurrent marks the current link or card: aria-current="page" (or
// "true") when on, no attribute at all otherwise.
func ariaCurrent(on bool, value string) templ.Attributes {
	if on {
		return templ.Attributes{"aria-current": value}
	}
	return templ.Attributes{}
}
