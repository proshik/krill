package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/notify"
	"github.com/proshik/krill/internal/secret"
	"github.com/proshik/krill/internal/web/i18n"
	"github.com/proshik/krill/internal/web/templates"
)

const telegramType = "telegram"

// Defaults of a channel that does not exist yet (match migration 000061).
const (
	defaultDiskThreshold = 85
	defaultMemThreshold  = 100
	defaultDigestMinute  = 11 * 60
)

// listNotifications renders the org's Telegram notification settings.
func (s *Server) listNotifications(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	v := templates.NotifyView{
		NotifyDeploy: true, NotifyBackup: true, NotifyHealth: true,
		Lang: i18n.Current(r.Context()), DigestTime: digestTime(defaultDigestMinute), DigestTZ: notify.DefaultDigestTZ,
		DiskThresholdPct: defaultDiskThreshold, MemThresholdPct: defaultMemThreshold,
	}
	ch, err := s.q.GetNotificationChannel(r.Context(), db.GetNotificationChannelParams{OrgID: o.ID, Type: telegramType})
	if err == nil {
		v = templates.NotifyView{
			Enabled:             ch.Enabled,
			HasToken:            ch.BotToken != "",
			ChatID:              ch.ChatID,
			NotifyDeploy:        ch.NotifyDeploy,
			NotifyBackup:        ch.NotifyBackup,
			NotifyHealth:        ch.NotifyHealth,
			NotifyDeploySuccess: ch.NotifyDeploySuccess,
			Lang:                ch.Lang,
			DigestEnabled:       ch.DigestEnabled,
			DigestTime:          digestTime(int(ch.DigestMinute)),
			DigestTZ:            ch.DigestTz,
			NotifyInfra:         ch.NotifyInfra,
			DiskThresholdPct:    int(ch.DiskThresholdPct),
			MemThresholdPct:     int(ch.MemThresholdPct),
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		logFrom(r).Error("listNotifications: load channel failed", "err", err, "org_id", o.ID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	v.CanInfra = auth.IsInstanceAdmin(r.Context())
	if s.notify != nil {
		via, perr := s.notify.TelegramProxy()
		switch {
		case perr != nil:
			v.ProxyErrKey = proxyErrKey(perr)
		case via != "":
			v.ProxySet = true
			if v.CanInfra { // the proxy is part of the machine: like the Nodes page, operators only
				v.ProxyVia = via
			}
		}
	}
	if err == nil {
		s.digestPreview(r, ch, &v)
	}
	render(w, r, http.StatusOK, templates.Notifications(o, role, v))
}

// digestPreview builds the organization's digest as it would go out now,
// through the same Digest.Build the scheduler uses, without sending it. Node
// data is for instance operators only, so for anyone else the preview leaves
// the infrastructure part out even when the channel has it.
func (s *Server) digestPreview(r *http.Request, row db.NotificationChannel, v *templates.NotifyView) {
	if s.digest == nil {
		return
	}
	ch, err := notify.ChannelFromRow(row)
	if err != nil {
		v.PreviewErr = true
		return
	}
	if !auth.IsInstanceAdmin(r.Context()) {
		ch.NotifyInfra = false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	parts, err := s.digest.Build(ctx, ch, time.Now())
	if err != nil {
		logFrom(r).Warn("listNotifications: digest preview", "err", err)
		v.PreviewErr = true
		return
	}
	v.Preview = templates.ParseDigest(parts)
}

// digestTime renders minutes after midnight as "HH:MM".
func digestTime(minute int) string {
	return fmt.Sprintf("%02d:%02d", minute/60, minute%60)
}

// parseDigestTime parses "HH:MM" (what <input type=time> submits) into
// minutes after midnight.
func parseDigestTime(v string) (int, bool) {
	t, err := time.Parse("15:04", strings.TrimSpace(v))
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}

// formValue returns a form field and whether the form sent it at all.
func formValue(r *http.Request, name string) (string, bool) {
	if err := r.ParseForm(); err != nil {
		return "", false
	}
	vs, ok := r.PostForm[name]
	if !ok || len(vs) == 0 {
		return "", false
	}
	return vs[0], true
}

// parseThreshold parses a whole percentage within [lo, hi].
func parseThreshold(v string, lo, hi int) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < lo || n > hi {
		return 0, false
	}
	return n, true
}

// saveNotifications upserts the channel. A blank token keeps the stored one.
//
// The infrastructure settings (node alerts and the digest's node section,
// plus their thresholds) are only taken from an instance operator: the nodes
// are shared by every organization, and org-scoped admin is self-grantable.
// Anyone else keeps the stored values, and a change of bot token or chat
// turns node alerts off, so an organization admin can't redirect them to a
// chat the operator never chose.
func (s *Server) saveNotifications(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	existing, err := s.q.GetNotificationChannel(r.Context(), db.GetNotificationChannelParams{OrgID: o.ID, Type: telegramType})
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		logFrom(r).Error("saveNotifications: load channel failed", "err", err, "org_id", o.ID)
		s.flashErr(w, r, err.Error())
		return
	}
	// A field the form did not send (a page loaded before an upgrade) keeps
	// the stored value; one sent with a bad value is refused.
	lang, minute, tz := i18n.Current(r.Context()), defaultDigestMinute, notify.DefaultDigestTZ
	if found {
		lang, minute, tz = existing.Lang, int(existing.DigestMinute), existing.DigestTz
	}
	if v, sent := formValue(r, "lang"); sent {
		if !i18n.Supported(v) {
			s.flashErrT(w, r, "flash.err.notif_lang")
			return
		}
		lang = v
	}
	if v, sent := formValue(r, "digest_time"); sent {
		if minute, ok = parseDigestTime(v); !ok {
			s.flashErrT(w, r, "flash.err.notif_time")
			return
		}
	}
	if v, sent := formValue(r, "digest_tz"); sent {
		if tz = strings.TrimSpace(v); !notify.ValidTZ(tz) {
			s.flashErrT(w, r, "flash.err.notif_tz")
			return
		}
	}

	token := r.FormValue("bot_token")
	chatID := r.FormValue("chat_id")
	enabled := r.FormValue("enabled") == "on"
	notifyDeploy := r.FormValue("notify_deploy") == "on"
	notifyBackup := r.FormValue("notify_backup") == "on"
	notifyHealth := r.FormValue("notify_health") == "on"
	notifyDeploySuccess := r.FormValue("notify_deploy_success") == "on"
	digestEnabled := r.FormValue("digest_enabled") == "on"

	var infra, infraReset bool
	if auth.IsInstanceAdmin(r.Context()) {
		disk, mem := defaultDiskThreshold, defaultMemThreshold
		if found {
			disk, mem = int(existing.DiskThresholdPct), int(existing.MemThresholdPct)
		}
		if v, sent := formValue(r, "disk_threshold"); sent {
			if disk, ok = parseThreshold(v, 50, 99); !ok {
				s.flashErrT(w, r, "flash.err.notif_disk_threshold")
				return
			}
		}
		if v, sent := formValue(r, "mem_threshold"); sent {
			if mem, ok = parseThreshold(v, 50, 200); !ok {
				s.flashErrT(w, r, "flash.err.notif_mem_threshold")
				return
			}
		}
		infra = r.FormValue("notify_infra") == "on"
		encToken := existing.BotToken
		if token != "" {
			encToken = secret.Enc(token)
		}
		if _, err := s.q.UpsertNotificationChannel(r.Context(), db.UpsertNotificationChannelParams{
			OrgID:               o.ID,
			Type:                telegramType,
			Enabled:             enabled,
			BotToken:            encToken,
			ChatID:              chatID,
			NotifyDeploy:        notifyDeploy,
			NotifyBackup:        notifyBackup,
			NotifyHealth:        notifyHealth,
			NotifyDeploySuccess: notifyDeploySuccess,
			NotifyInfra:         infra,
			DiskThresholdPct:    int16(disk),
			MemThresholdPct:     int16(mem),
			DigestEnabled:       digestEnabled,
			DigestMinute:        int16(minute),
			DigestTz:            tz,
			Lang:                lang,
		}); err != nil {
			logFrom(r).Error("saveNotifications: upsert failed", "err", err, "org_id", o.ID)
			s.flashErr(w, r, err.Error())
			return
		}
	} else {
		// The node-alert settings are decided by the statement against the
		// row as it is now: an operator's change made after the read above
		// must not be written back. A blank token keeps the stored bot.
		encToken := ""
		if token != "" {
			encToken = secret.Enc(token)
		}
		saved, err := s.q.UpsertNotificationChannelKeepInfra(r.Context(), db.UpsertNotificationChannelKeepInfraParams{
			OrgID:               o.ID,
			Type:                telegramType,
			Enabled:             enabled,
			BotToken:            encToken,
			ChatID:              chatID,
			NotifyDeploy:        notifyDeploy,
			NotifyBackup:        notifyBackup,
			NotifyHealth:        notifyHealth,
			NotifyDeploySuccess: notifyDeploySuccess,
			DigestEnabled:       digestEnabled,
			DigestMinute:        int16(minute),
			DigestTz:            tz,
			Lang:                lang,
		})
		if err != nil {
			logFrom(r).Error("saveNotifications: upsert failed", "err", err, "org_id", o.ID)
			s.flashErr(w, r, err.Error())
			return
		}
		infra, infraReset = saved.NotifyInfra, saved.WasInfra && !saved.NotifyInfra
	}
	logFrom(r).Info("notifications saved", "org_id", o.ID, "infra", infra, "infra_reset", infraReset)
	msg := i18n.T(r.Context(), "notif.saved")
	if infraReset {
		msg += ". " + i18n.T(r.Context(), "notif.infra_reset")
	}
	s.setFlash(w, r, "ok", msg)
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/notifications", http.StatusSeeOther)
}

// sendDigestNow sends the organization's digest right away, whatever its
// schedule — a preview of what the channel will get.
func (s *Server) sendDigestNow(w http.ResponseWriter, r *http.Request) {
	o, _, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	row, err := s.q.GetNotificationChannel(r.Context(), db.GetNotificationChannelParams{OrgID: o.ID, Type: telegramType})
	if err != nil || row.BotToken == "" || row.ChatID == "" {
		s.flashErr(w, r, i18n.T(r.Context(), "notif.not_configured"))
		return
	}
	if s.digest == nil {
		s.flashErr(w, r, i18n.T(r.Context(), "notif.digest_fail"))
		return
	}
	ch, err := notify.ChannelFromRow(row)
	if err != nil {
		logFrom(r).Error("sendDigestNow: bot token undecryptable", "err", err, "org_id", o.ID)
		s.flashErrT(w, r, "flash.err.secret_undecryptable")
		return
	}
	if !s.claimTestNotification(o.ID, time.Now()) {
		s.flashErrT(w, r, "flash.err.notif_test_cooldown")
		return
	}
	if err := s.digest.SendNow(r.Context(), ch); err != nil {
		logFrom(r).Info("sendDigestNow: send failed", "org_id", o.ID, "err", err) // the token is redacted from send errors
		s.flashErr(w, r, i18n.T(r.Context(), "notif.digest_fail"))
		return
	}
	s.setFlash(w, r, "ok", i18n.T(r.Context(), "notif.digest_sent"))
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
	botToken, derr := secret.Dec(ch.BotToken)
	if derr != nil {
		logFrom(r).Error("testNotification: bot token undecryptable", "err", derr, "org_id", o.ID)
		s.flashErrT(w, r, "flash.err.secret_undecryptable")
		return
	}
	if !s.claimTestNotification(o.ID, time.Now()) {
		s.flashErrT(w, r, "flash.err.notif_test_cooldown")
		return
	}
	if err := s.notify.SendTest(r.Context(), botToken, ch.ChatID, "Krill: test message"); err != nil {
		logFrom(r).Info("testNotification: send failed", "org_id", o.ID, "err", err) // the token is redacted from send errors
		s.flashErr(w, r, i18n.T(r.Context(), "notif.test_fail"))
		return
	}
	s.setFlash(w, r, "ok", i18n.T(r.Context(), "notif.test_ok"))
	http.Redirect(w, r, "/orgs/"+strconv.FormatInt(o.ID, 10)+"/notifications", http.StatusSeeOther)
}

// testNotificationCooldown is how long an organization waits between test
// messages: long enough to swallow a double click, short enough not to be
// noticed otherwise.
const testNotificationCooldown = 10 * time.Second

// claimTestNotification reports whether orgID may send a test message now,
// recording it when so. Claimed before sending, so two overlapping requests
// can't both pass.
func (s *Server) claimTestNotification(orgID int64, now time.Time) bool {
	s.testNotifyMu.Lock()
	defer s.testNotifyMu.Unlock()
	if last, ok := s.testNotifyAt[orgID]; ok && now.Sub(last) < testNotificationCooldown {
		return false
	}
	if s.testNotifyAt == nil {
		s.testNotifyAt = map[int64]time.Time{}
	}
	s.testNotifyAt[orgID] = now
	return true
}

// proxyErrKey is the i18n key of why the Telegram proxy setting was rejected.
func proxyErrKey(err error) string {
	switch {
	case errors.Is(err, notify.ErrProxyScheme):
		return "notif.proxy_err_scheme"
	case errors.Is(err, notify.ErrProxyHost):
		return "notif.proxy_err_host"
	case errors.Is(err, notify.ErrProxyPath):
		return "notif.proxy_err_path"
	}
	return "notif.proxy_err_url"
}
