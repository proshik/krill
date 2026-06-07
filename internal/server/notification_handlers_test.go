package server_test

import (
	"context"
	"net/http"
	"net/url"
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
		"enabled":  {"on"},
		"bot_token": {""},
		"chat_id":  {"66"},
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
