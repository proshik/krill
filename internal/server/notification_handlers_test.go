package server_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
)

func TestSaveNotificationsUpsertsAndKeepsToken(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "notif-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	cookie := loginAs(t, q, "notif-owner@k.local")

	rec := postForm(t, h, "/orgs/"+i64(o.ID)+"/notifications", cookie, url.Values{
		"enabled":       {"on"},
		"bot_token":     {"tok-123"},
		"chat_id":       {"55"},
		"notify_deploy": {"on"},
		"notify_backup": {"on"},
		"notify_health": {"on"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save want 303, got %d: %s", rec.Code, rec.Body.String())
	}
	ch, err := q.GetNotificationChannel(ctx, db.GetNotificationChannelParams{OrgID: o.ID, Type: "telegram"})
	if err != nil {
		t.Fatalf("get channel: %v", err)
	}
	if ch.ChatID != "55" || ch.BotToken == "" {
		t.Fatalf("channel = %+v", ch)
	}
	firstToken := ch.BotToken

	// Second save with blank token — chat_id changes but token must be preserved.
	rec = postForm(t, h, "/orgs/"+i64(o.ID)+"/notifications", cookie, url.Values{
		"enabled":   {"on"},
		"bot_token": {""},
		"chat_id":   {"66"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save2 want 303, got %d", rec.Code)
	}
	ch, _ = q.GetNotificationChannel(ctx, db.GetNotificationChannelParams{OrgID: o.ID, Type: "telegram"})
	if ch.ChatID != "66" {
		t.Fatalf("chat_id not updated: %q", ch.ChatID)
	}
	if ch.BotToken != firstToken {
		t.Fatalf("blank token must not overwrite stored token")
	}
}

func TestNotificationsMemberForbidden(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "no-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	memberID := mkUser(t, q, "no-member@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	cookie := loginAs(t, q, "no-member@k.local")

	rec := postForm(t, h, "/orgs/"+i64(o.ID)+"/notifications", cookie, url.Values{"chat_id": {"1"}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member save want 403, got %d", rec.Code)
	}
}

// TestNotificationsCrossTenantForbidden verifies an admin of org B cannot read
// or write org A's notification channel (RequireOrgMember returns 404 for a
// non-member, so the org-A config is never reachable or mutated).
func TestNotificationsCrossTenantForbidden(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()

	userA := mkUser(t, q, "nt-a@k.local")
	orgA, _ := orgSvc.CreateOrg(ctx, userA, "OrgA")
	if _, err := q.UpsertNotificationChannel(ctx, db.UpsertNotificationChannelParams{
		OrgID: orgA.ID, Type: "telegram", Enabled: true, BotToken: "secret-a", ChatID: "1",
		NotifyDeploy: true, NotifyBackup: true, NotifyHealth: true,
		DiskThresholdPct: 85, MemThresholdPct: 100, DigestMinute: 660, DigestTz: "Europe/Moscow", Lang: "en",
	}); err != nil {
		t.Fatalf("seed org-A channel: %v", err)
	}

	userB := mkUser(t, q, "nt-b@k.local")
	if _, err := orgSvc.CreateOrg(ctx, userB, "OrgB"); err != nil {
		t.Fatalf("create org-B: %v", err)
	}
	cookieB := loginAs(t, q, "nt-b@k.local")

	rec := postForm(t, h, "/orgs/"+i64(orgA.ID)+"/notifications", cookieB, url.Values{"chat_id": {"999"}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("SECURITY FINDING: cross-tenant POST expected 404, got %d", rec.Code)
	}

	ch, err := q.GetNotificationChannel(ctx, db.GetNotificationChannelParams{OrgID: orgA.ID, Type: "telegram"})
	if err != nil || ch.ChatID != "1" {
		t.Fatalf("SECURITY FINDING: org-A channel mutated by cross-tenant request: %+v err=%v", ch, err)
	}
}

func notifyForm(extra url.Values) url.Values {
	v := url.Values{
		"enabled": {"on"}, "chat_id": {"55"}, "lang": {"ru"},
		"notify_deploy": {"on"}, "notify_deploy_success": {"on"},
		"digest_enabled": {"on"}, "digest_time": {"09:30"}, "digest_tz": {"Europe/Amsterdam"},
		"notify_infra": {"on"}, "disk_threshold": {"90"}, "mem_threshold": {"120"},
	}
	for k, vs := range extra {
		v[k] = vs
	}
	return v
}

func TestSaveNotificationsNewSettings(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	uid := mkUser(t, q, "notif-op@k.local")
	if err := q.SetUserAdmin(ctx, db.SetUserAdminParams{ID: uid, IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	o, _ := orgSvc.CreateOrg(ctx, uid, "Org")
	cookie := loginAs(t, q, "notif-op@k.local")

	rec := postForm(t, h, "/orgs/"+i64(o.ID)+"/notifications", cookie, notifyForm(url.Values{"bot_token": {"tok"}}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	ch, err := q.GetNotificationChannel(ctx, db.GetNotificationChannelParams{OrgID: o.ID, Type: "telegram"})
	if err != nil {
		t.Fatal(err)
	}
	if !ch.NotifyDeploySuccess || !ch.DigestEnabled || ch.DigestMinute != 570 || ch.DigestTz != "Europe/Amsterdam" ||
		ch.Lang != "ru" || !ch.NotifyInfra || ch.DiskThresholdPct != 90 || ch.MemThresholdPct != 120 {
		t.Fatalf("channel = %+v", ch)
	}

	// The page shows them back.
	page := getPage(t, h, "/orgs/"+i64(o.ID)+"/notifications", cookie, nil)
	for _, want := range []string{`value="09:30"`, `value="Europe/Amsterdam"`, `value="90"`, `value="120"`, `name="notify_infra"`} {
		if !strings.Contains(page.Body.String(), want) {
			t.Errorf("page lacks %s", want)
		}
	}

	for name, bad := range map[string]url.Values{
		"disk too high":    {"disk_threshold": {"100"}},
		"mem not a number": {"mem_threshold": {"lots"}},
		"bad time":         {"digest_time": {"25:00"}},
		"bad zone":         {"digest_tz": {"Mars/Olympus"}},
		"bad lang":         {"lang": {"de"}},
	} {
		rec := postForm(t, h, "/orgs/"+i64(o.ID)+"/notifications", cookie, notifyForm(bad))
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("%s: %d", name, rec.Code)
		}
		after, _ := q.GetNotificationChannel(ctx, db.GetNotificationChannelParams{OrgID: o.ID, Type: "telegram"})
		if after.UpdatedAt != ch.UpdatedAt {
			t.Errorf("%s: an invalid form was saved: %+v", name, after)
		}
	}
}

// An organization admin who is not the instance operator cannot turn node
// alerts on or move their thresholds, and changing the bot or chat of a
// channel the operator opted in turns them off.
func TestSaveNotificationsInfraIsOperatorOnly(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	opID := mkUser(t, q, "infra-op@k.local")
	if err := q.SetUserAdmin(ctx, db.SetUserAdminParams{ID: opID, IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	o, _ := orgSvc.CreateOrg(ctx, opID, "Org")
	adminID := mkUser(t, q, "infra-admin@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: adminID, Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	admin := loginAs(t, q, "infra-admin@k.local")
	op := loginAs(t, q, "infra-op@k.local")
	path := "/orgs/" + i64(o.ID) + "/notifications"
	load := func() db.NotificationChannel {
		ch, err := q.GetNotificationChannel(ctx, db.GetNotificationChannelParams{OrgID: o.ID, Type: "telegram"})
		if err != nil {
			t.Fatal(err)
		}
		return ch
	}

	// An org admin's attempt is ignored.
	postForm(t, h, path, admin, notifyForm(url.Values{"bot_token": {"tok"}}))
	if ch := load(); ch.NotifyInfra || ch.DiskThresholdPct != 85 || ch.MemThresholdPct != 100 {
		t.Fatalf("org admin set infrastructure settings: %+v", ch)
	}
	if page := getPage(t, h, path, admin, nil); strings.Contains(page.Body.String(), `name="notify_infra"`) {
		t.Error("the infrastructure checkbox is editable for an org admin")
	}

	// The operator opts the channel in.
	postForm(t, h, path, op, notifyForm(nil))
	if ch := load(); !ch.NotifyInfra || ch.DiskThresholdPct != 90 {
		t.Fatalf("operator could not set infrastructure settings: %+v", ch)
	}

	// The org admin edits other settings with the same chat: kept.
	postForm(t, h, path, admin, notifyForm(url.Values{"disk_threshold": {"50"}, "notify_infra": {""}}))
	if ch := load(); !ch.NotifyInfra || ch.DiskThresholdPct != 90 {
		t.Fatalf("org admin's save changed infrastructure settings: %+v", ch)
	}
	// ...and points the channel at another chat: node alerts go off.
	postForm(t, h, path, admin, notifyForm(url.Values{"chat_id": {"777"}}))
	if ch := load(); ch.NotifyInfra || ch.ChatID != "777" {
		t.Fatalf("node alerts followed a chat the operator never chose: %+v", ch)
	}
	// Same for a new bot token.
	postForm(t, h, path, op, notifyForm(url.Values{"chat_id": {"777"}}))
	postForm(t, h, path, admin, notifyForm(url.Values{"chat_id": {"777"}, "bot_token": {"other-bot"}}))
	if ch := load(); ch.NotifyInfra {
		t.Fatalf("node alerts followed a new bot: %+v", ch)
	}
}

// A form without the new fields (a page loaded before the upgrade) keeps the
// stored values instead of failing or resetting them.
func TestSaveNotificationsOldFormKeepsNewSettings(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	uid := mkUser(t, q, "notif-old@k.local")
	o, _ := orgSvc.CreateOrg(ctx, uid, "Org")
	cookie := loginAs(t, q, "notif-old@k.local")
	path := "/orgs/" + i64(o.ID) + "/notifications"
	postForm(t, h, path, cookie, notifyForm(url.Values{"bot_token": {"tok"}}))
	rec := postForm(t, h, path, cookie, url.Values{"enabled": {"on"}, "chat_id": {"55"}, "notify_deploy": {"on"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("old form: %d", rec.Code)
	}
	ch, _ := q.GetNotificationChannel(ctx, db.GetNotificationChannelParams{OrgID: o.ID, Type: "telegram"})
	if ch.Lang != "ru" || ch.DigestMinute != 570 || ch.DigestTz != "Europe/Amsterdam" {
		t.Fatalf("old form reset the new settings: %+v", ch)
	}
}

func TestSendDigestNowNeedsAChannel(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	uid := mkUser(t, q, "digest-now@k.local")
	o, _ := orgSvc.CreateOrg(ctx, uid, "Org")
	cookie := loginAs(t, q, "digest-now@k.local")
	rec := postForm(t, h, "/orgs/"+i64(o.ID)+"/notifications/digest", cookie, url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("digest without a channel: %d", rec.Code)
	}
	memberID := mkUser(t, q, "digest-member@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatal(err)
	}
	if rec := postForm(t, h, "/orgs/"+i64(o.ID)+"/notifications/digest", loginAs(t, q, "digest-member@k.local"), url.Values{}); rec.Code != http.StatusForbidden {
		t.Fatalf("member digest-now want 403, got %d", rec.Code)
	}
}
