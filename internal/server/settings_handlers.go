package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"

	"github.com/proshik/krill/internal/web/i18n"
	"github.com/proshik/krill/internal/web/templates"
)

// setLanguage stores the chosen UI language in the krill_lang cookie and
// redirects back. Read by the locale middleware on every request.
func (s *Server) setLanguage(w http.ResponseWriter, r *http.Request) {
	lang := r.FormValue("lang")
	if !i18n.Supported(lang) {
		lang = i18n.DefaultLocale
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "krill_lang",
		Value:    lang,
		Path:     "/",
		MaxAge:   365 * 24 * 3600,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, s.backURL(r), http.StatusSeeOther)
}

// settingsOverview renders the settings start page: a summary of every
// section the viewer may open. The instance's own sections are summarized for
// instance operators only, like the pages they lead to.
func (s *Server) settingsOverview(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	base := "/orgs/" + strconv.FormatInt(o.ID, 10)
	t := func(k string) string { return i18n.T(ctx, k) }
	var v templates.SettingsOverviewView
	if u, err := s.q.GetUserByID(ctx, auth.UserID(ctx)); err == nil {
		v.Email = u.Email
	}

	org := templates.SettingsSection{ID: "org", Title: t("settings.group_org")}
	if ms, err := s.q.ListMembers(ctx, o.ID); err == nil {
		roles := map[string]int{}
		for _, m := range ms {
			roles[m.Role]++
		}
		org.Rows = append(org.Rows, templates.SettingsRow{Title: t("nav.members"), Text: i18n.Tf(ctx, "members.n", len(ms)),
			Note: i18n.Tf(ctx, "settings.roles_summary", roles["owner"], roles["admin"], roles["member"]), Href: base + "/members"})
	}
	if regs, err := s.q.ListRegistriesByOrg(ctx, o.ID); err == nil {
		hosts := make([]string, 0, len(regs))
		for _, r := range regs {
			hosts = append(hosts, r.RegistryUrl)
		}
		org.Rows = append(org.Rows, templates.SettingsRow{Title: t("reg.link"), Text: i18n.Tf(ctx, "reg.n", len(regs)), Note: strings.Join(hosts, ", "), Href: base + "/registries"})
	}
	if gcs, err := s.q.ListGitCredentialsByOrg(ctx, o.ID); err == nil {
		hosts := make([]string, 0, len(gcs))
		for _, g := range gcs {
			hosts = append(hosts, g.Host)
		}
		org.Rows = append(org.Rows, templates.SettingsRow{Title: t("gitcred.link"), Text: i18n.Tf(ctx, "gitcred.n", len(gcs)), Note: strings.Join(hosts, ", "), Href: base + "/git-credentials"})
	}
	if dests, err := s.q.ListDestinationsByOrg(ctx, o.ID); err == nil {
		names := make([]string, 0, len(dests))
		for _, d := range dests {
			names = append(names, d.Name)
		}
		org.Rows = append(org.Rows, templates.SettingsRow{Title: t("dest.link"), Text: i18n.Tf(ctx, "dest.n", len(dests)), Note: strings.Join(names, ", "), Href: base + "/destinations"})
	}
	if isAdminRole(role) {
		row := templates.SettingsRow{Title: t("notif.link"), Text: "Telegram", Href: base + "/notifications", Kind: "idle", State: t("settings.off")}
		if ch, err := s.q.GetNotificationChannel(ctx, db.GetNotificationChannelParams{OrgID: o.ID, Type: telegramType}); err == nil && ch.Enabled {
			row.Kind, row.State = "ok", t("settings.on")
			if ch.DigestEnabled {
				row.Text = i18n.Tf(ctx, "settings.digest_at", digestTime(int(ch.DigestMinute)))
			}
		}
		org.Rows = append(org.Rows, row)
	}
	if toks, err := s.q.ListAPITokensByUserAndOrg(ctx, db.ListAPITokensByUserAndOrgParams{UserID: auth.UserID(ctx), OrgID: o.ID}); err == nil {
		org.Rows = append(org.Rows, templates.SettingsRow{Title: t("api_tokens.link"), Text: i18n.Tf(ctx, "api_tokens.n", len(toks)), Href: base + "/api-tokens"})
	}
	v.Org = append(v.Org, org)

	if auth.IsInstanceAdmin(ctx) {
		kr := templates.SettingsSection{ID: "krill", Title: t("settings.group_krill")}
		row := templates.SettingsRow{Title: t("panel.link"), Text: t("panel.state.off"), Href: base + "/panel-domain", Kind: "idle", State: t("panel.state.off")}
		if pg, err := s.q.GetPanelGateway(ctx); err == nil && pg.Host != "" {
			row.Text = pg.Host
			switch pg.State {
			case "active":
				row.Kind, row.State = "ok", t("panel.state.active")
			case "pending":
				row.Kind, row.State = "warn", t("panel.state.pending")
			}
		}
		kr.Rows = append(kr.Rows, row)
		kr.Rows = append(kr.Rows, templates.SettingsRow{Title: t("firewall.link"), Text: t("settings.firewall_note"), Href: base + "/firewall"})
		obs := templates.SettingsRow{Title: t("obs.link"), Href: base + "/observability", Kind: "idle", State: t("obs.state.off")}
		if s.obsWired() {
			if row, err := s.q.GetObservabilitySettings(ctx); err == nil && row.Enabled {
				obs.Kind, obs.State = "ok", t("obs.state.on")
				obs.Text = strings.Trim(trimURLScheme(row.MetricsUrl)+", "+trimURLScheme(row.LogsUrl), ", ")
			}
		}
		kr.Rows = append(kr.Rows, obs)
		sbk := templates.SettingsRow{Title: t("sbk.link"), Href: base + "/krill-backups", Kind: "idle", State: t("sbk.state.none")}
		if s.sbkWired() {
			if st, err := s.sbk.ctl.View(ctx); err == nil && st.Ready() {
				sbk.Text = st.Bucket
				if st.Enabled {
					sbk.Kind, sbk.State = "ok", t("sbk.state.active")
				} else {
					sbk.Kind, sbk.State = "warn", t("sbk.state.paused")
				}
			}
		}
		kr.Rows = append(kr.Rows, sbk)
		up := templates.SettingsRow{Title: t("update.link"), Href: base + "/updates"}
		if uv := s.updatesView(ctx); uv.Wired {
			up.Text = uv.Current
			if uv.Available {
				up.Kind, up.State = "warn", i18n.Tf(ctx, "update.available_short", uv.Latest)
			} else if uv.Latest != "" {
				up.Kind, up.State = "ok", t("update.up_to_date_short")
			}
		}
		kr.Rows = append(kr.Rows, up)
		v.Krill = append(v.Krill, kr)
	}
	render(w, r, http.StatusOK, templates.SettingsOverview(o, role, v))
}

func trimURLScheme(u string) string {
	u = strings.TrimPrefix(u, "https://")
	return strings.TrimPrefix(u, "http://")
}
