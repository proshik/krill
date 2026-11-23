package server

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/web/i18n"
	"github.com/proshik/krill/internal/web/templates"
)

// paletteMax caps each group of the palette's results.
const paletteMax = 8

// likeEscaper makes a search term literal inside an ILIKE pattern.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// orgSearch answers the command palette: the organization's projects,
// environments, apps, databases, buckets, servers and the pages the viewer
// may open, filtered by ?q=. With ?env= (an environment of this
// organization) that environment's actions come first. Everything is read
// through the organization's own index, so nothing of another tenant can
// appear; instance-wide pages are offered to instance operators only.
func (s *Server) orgSearch(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	match := func(parts ...string) bool {
		if q == "" {
			return true
		}
		for _, p := range parts {
			if strings.Contains(strings.ToLower(p), q) {
				return true
			}
		}
		return false
	}
	ix, err := s.loadOrgIndex(ctx, o.ID)
	if err != nil {
		logFrom(r).Error("orgSearch: org index", "err", err, "org_id", o.ID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	base := "/orgs/" + strconv.FormatInt(o.ID, 10)
	admin := isAdminRole(role)
	var groups []templates.PaletteGroup
	add := func(g templates.PaletteGroup) {
		if len(g.Items) > paletteMax {
			g.Items = g.Items[:paletteMax]
		}
		if len(g.Items) > 0 {
			groups = append(groups, g)
		}
	}
	// The apps to show are picked first, capped, and only they are looked up
	// in Swarm for their live status (nothing is, when none is shown).
	byName := func(ids []int64) {
		sort.SliceStable(ids, func(i, j int) bool { return ix.appByID[ids[i]].Name < ix.appByID[ids[j]].Name })
	}
	capped := func(ids []int64) []int64 {
		if len(ids) > paletteMax {
			return ids[:paletteMax]
		}
		return ids
	}
	var envID int64 = -1
	var envApps, orgApps []int64
	if id, err := strconv.ParseInt(r.URL.Query().Get("env"), 10, 64); err == nil {
		if _, ok := ix.envs[id]; ok {
			envID = id
			for _, a := range ix.apps {
				if a.EnvironmentID == id && match(a.Name) {
					envApps = append(envApps, a.ID)
				}
			}
			byName(envApps)
			envApps = capped(envApps)
		}
	}
	if q != "" {
		for _, a := range ix.apps {
			if match(a.Name, ix.envLabel(a.EnvironmentID)) {
				orgApps = append(orgApps, a.ID)
			}
		}
		byName(orgApps)
		orgApps = capped(orgApps)
	}
	var shown []indexApp
	seen := map[int64]bool{}
	for _, id := range append(append([]int64{}, envApps...), orgApps...) {
		if !seen[id] {
			seen[id] = true
			shown = append(shown, ix.appByID[id])
		}
	}
	s.liveIndexStatuses(ctx, shown)
	live := map[int64]indexApp{}
	for _, a := range shown {
		live[a.ID] = a
	}
	appItem := func(appID int64) templates.PaletteItem {
		a := live[appID]
		it := templates.PaletteItem{Label: a.Name, Sub: ix.envLabel(a.EnvironmentID), Href: ix.appURL(a), Glyph: "i-app"}
		if kind, text := templates.AppState(ctx, a.Status); kind == "err" || kind == "warn" {
			it.StateKind, it.StateText = kind, text
		}
		return it
	}

	// The environment the palette was opened in.
	if envID >= 0 {
		g := templates.PaletteGroup{Title: ix.envLabel(envID)}
		envBase := ix.envURL(envID)
		if admin {
			for _, it := range []templates.PaletteItem{
				{Label: i18n.T(ctx, "project.new_app"), Href: envBase + "?new=app", Glyph: "i-plus"},
				{Label: i18n.T(ctx, "db.new"), Href: envBase + "/databases?new=database", Glyph: "i-plus"},
				{Label: i18n.T(ctx, "bucket.new"), Href: envBase + "/databases?new=bucket", Glyph: "i-plus"},
			} {
				if match(it.Label) {
					g.Items = append(g.Items, it)
				}
			}
		}
		for _, id := range envApps {
			g.Items = append(g.Items, appItem(id))
		}
		add(g)
	}

	// Apps of the whole organization.
	if len(orgApps) > 0 {
		g := templates.PaletteGroup{Title: i18n.T(ctx, "palette.apps")}
		for _, id := range orgApps {
			g.Items = append(g.Items, appItem(id))
		}
		add(g)
	}

	// Databases, buckets and servers: one organization-wide query each,
	// matched and capped in SQL, and a group each, so many matching databases
	// cannot push a bucket or a server out of the results.
	if q != "" {
		g := templates.PaletteGroup{Title: i18n.T(ctx, "palette.databases")}
		pattern := "%" + likeEscaper.Replace(q) + "%"
		ldbs, err := s.q.SearchLogicalDatabasesByOrg(ctx, db.SearchLogicalDatabasesByOrgParams{OrganizationID: o.ID, Pattern: pattern, MaxRows: paletteMax})
		if err != nil {
			logFrom(r).Error("orgSearch: databases", "err", err, "org_id", o.ID)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		for _, l := range ldbs {
			if _, ok := ix.envs[l.EnvironmentID]; ok {
				g.Items = append(g.Items, templates.PaletteItem{Label: l.Name, Sub: ix.envLabel(l.EnvironmentID), Href: ix.envURL(l.EnvironmentID) + "/databases/" + strconv.FormatInt(l.ID, 10), Glyph: "i-pg"})
			}
		}
		bs, err := s.q.SearchS3BucketsByOrg(ctx, db.SearchS3BucketsByOrgParams{OrganizationID: o.ID, Pattern: pattern, MaxRows: paletteMax})
		if err != nil {
			logFrom(r).Error("orgSearch: buckets", "err", err, "org_id", o.ID)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		add(g)
		g = templates.PaletteGroup{Title: i18n.T(ctx, "palette.buckets")}
		for _, b := range bs {
			if _, ok := ix.envs[b.EnvironmentID]; ok {
				g.Items = append(g.Items, templates.PaletteItem{Label: b.Name, Sub: ix.envLabel(b.EnvironmentID), Href: ix.envURL(b.EnvironmentID) + "/buckets/" + strconv.FormatInt(b.ID, 10), Glyph: "i-bucket"})
			}
		}
		add(g)
		g = templates.PaletteGroup{Title: i18n.T(ctx, "nav.dbservers")}
		insts, err := s.q.ListDBInstancesByOrg(ctx, o.ID)
		if err != nil {
			logFrom(r).Error("orgSearch: database servers", "err", err, "org_id", o.ID)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		for _, in := range insts {
			if match(in.Name, in.Engine) {
				g.Items = append(g.Items, templates.PaletteItem{Label: in.Name, Sub: i18n.Tf(ctx, "canvas.server", engineLabel(in.Engine)), Href: base + "/db-servers/" + strconv.FormatInt(in.ID, 10), Glyph: engineGlyph(in.Engine)})
			}
		}
		add(g)
	}

	// Projects and environments.
	{
		g := templates.PaletteGroup{Title: i18n.T(ctx, "nav.projects")}
		envs := make([]int64, 0, len(ix.envs))
		for id := range ix.envs {
			envs = append(envs, id)
		}
		sort.Slice(envs, func(i, j int) bool { return ix.envLabel(envs[i]) < ix.envLabel(envs[j]) })
		for _, id := range envs {
			if match(ix.envLabel(id)) {
				g.Items = append(g.Items, templates.PaletteItem{Label: ix.envLabel(id), Href: ix.envURL(id)})
			}
		}
		add(g)
	}

	// Pages.
	g := templates.PaletteGroup{Title: i18n.T(ctx, "palette.go_to")}
	for _, t := range paletteTabs(ctx, base, role) {
		if match(t.Label) {
			g.Items = append(g.Items, templates.PaletteItem{Label: t.Label, Href: t.Href})
		}
	}
	for _, sg := range paletteSettings(ctx, base, role) {
		if match(sg.Label, i18n.T(ctx, "nav.settings")) {
			g.Items = append(g.Items, templates.PaletteItem{Label: sg.Label, Sub: i18n.T(ctx, "nav.settings"), Href: sg.Href})
		}
	}
	if match(i18n.T(ctx, "nav.logout")) {
		g.Items = append(g.Items, templates.PaletteItem{Label: i18n.T(ctx, "nav.logout"), Logout: true})
	}
	if len(g.Items) > 0 {
		groups = append(groups, g) // not capped: the pages are the palette's map
	}
	render(w, r, http.StatusOK, templates.PaletteResults(groups))
}

type paletteLink struct{ Label, Href string }

// paletteTabs are the organization's tabs, with the bar's gates.
func paletteTabs(ctx context.Context, base, role string) []paletteLink {
	out := []paletteLink{
		{i18n.T(ctx, "nav.projects"), base},
		{i18n.T(ctx, "nav.dbservers"), base + "/db-servers"},
	}
	if auth.IsInstanceAdmin(ctx) {
		out = append(out, paletteLink{i18n.T(ctx, "nav.nodes"), base + "/nodes"})
	}
	if isAdminRole(role) {
		out = append(out, paletteLink{i18n.T(ctx, "nav.topology"), base + "/topology"})
	}
	if auth.IsInstanceAdmin(ctx) {
		out = append(out, paletteLink{i18n.T(ctx, "nav.monitoring"), base + "/monitoring"})
	}
	return append(out, paletteLink{i18n.T(ctx, "nav.settings"), base + "/settings"})
}

// paletteSettings are the settings pages, with the sub-navigation's gates.
func paletteSettings(ctx context.Context, base, role string) []paletteLink {
	out := []paletteLink{
		{i18n.T(ctx, "nav.members"), base + "/members"},
		{i18n.T(ctx, "reg.link"), base + "/registries"},
		{i18n.T(ctx, "gitcred.link"), base + "/git-credentials"},
		{i18n.T(ctx, "dest.link"), base + "/destinations"},
		{i18n.T(ctx, "api_tokens.link"), base + "/api-tokens"},
		{i18n.T(ctx, "settings.password"), base + "/account/password"},
	}
	if isAdminRole(role) {
		out = append(out, paletteLink{i18n.T(ctx, "notif.link"), base + "/notifications"})
	}
	if auth.IsInstanceAdmin(ctx) {
		out = append(out,
			paletteLink{i18n.T(ctx, "panel.link"), base + "/panel-domain"},
			paletteLink{i18n.T(ctx, "firewall.link"), base + "/firewall"},
			paletteLink{i18n.T(ctx, "obs.link"), base + "/observability"},
			paletteLink{i18n.T(ctx, "sbk.link"), base + "/krill-backups"},
			paletteLink{i18n.T(ctx, "update.link"), base + "/updates"},
		)
	}
	return out
}
