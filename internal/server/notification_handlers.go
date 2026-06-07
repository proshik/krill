package server

import (
	"net/http"
	"strconv"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/web/i18n"
	"github.com/proshik/krill/internal/web/templates"
)

const telegramType = "telegram"

// listNotifications renders the org's Telegram notification settings.
func (s *Server) listNotifications(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	v := templates.NotifyView{NotifyDeploy: true, NotifyBackup: true, NotifyHealth: true}
	ch, err := s.q.GetNotificationChannel(r.Context(), db.GetNotificationChannelParams{OrgID: o.ID, Type: telegramType})
	if err == nil {
		v = templates.NotifyView{
			Enabled:      ch.Enabled,
			HasToken:     ch.BotToken != "",
			ChatID:       ch.ChatID,
			NotifyDeploy: ch.NotifyDeploy,
			NotifyBackup: ch.NotifyBackup,
			NotifyHealth: ch.NotifyHealth,
		}
	}
	render(w, r, http.StatusOK, templates.Notifications(o, role, v))
}

// saveNotifications upserts the channel. A blank token keeps the stored one.
func (s *Server) saveNotifications(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	token := r.FormValue("bot_token")
	stored := ""
	if existing, err := s.q.GetNotificationChannel(r.Context(), db.GetNotificationChannelParams{OrgID: o.ID, Type: telegramType}); err == nil {
		stored = existing.BotToken
	}
	encToken := stored
	if token != "" {
		encToken = secret.Enc(token)
	}
	if _, err := s.q.UpsertNotificationChannel(r.Context(), db.UpsertNotificationChannelParams{
		OrgID:        o.ID,
		Type:         telegramType,
		Enabled:      r.FormValue("enabled") == "on",
		BotToken:     encToken,
		ChatID:       r.FormValue("chat_id"),
		NotifyDeploy: r.FormValue("notify_deploy") == "on",
		NotifyBackup: r.FormValue("notify_backup") == "on",
		NotifyHealth: r.FormValue("notify_health") == "on",
	}); err != nil {
		logFrom(r).Error("saveNotifications: upsert failed", "err", err, "org_id", o.ID)
		s.flashErr(w, r, err.Error())
		return
	}
	logFrom(r).Info("notifications saved", "org_id", o.ID)
	s.setFlash(w, "ok", i18n.T(r.Context(), "notif.saved"))
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/notifications", http.StatusSeeOther)
}

// testNotification sends a test message synchronously.
func (s *Server) testNotification(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	ch, err := s.q.GetNotificationChannel(r.Context(), db.GetNotificationChannelParams{OrgID: o.ID, Type: telegramType})
	if err != nil || ch.BotToken == "" || ch.ChatID == "" {
		s.flashErr(w, r, i18n.T(r.Context(), "notif.not_configured"))
		return
	}
	if s.notify == nil {
		s.flashErr(w, r, i18n.T(r.Context(), "notif.test_fail"))
		return
	}
	if err := s.notify.SendTest(r.Context(), secret.Dec(ch.BotToken), ch.ChatID, "Krill: test message"); err != nil {
		logFrom(r).Info("testNotification: send failed", "org_id", o.ID) // never log the token
		s.flashErr(w, r, i18n.T(r.Context(), "notif.test_fail"))
		return
	}
	s.setFlash(w, "ok", i18n.T(r.Context(), "notif.test_ok"))
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/notifications", http.StatusSeeOther)
}
