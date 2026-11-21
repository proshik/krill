package server_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/backup"
	"github.com/proshik/krill/internal/config"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/hostshare"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/testutil"
)

// readHookDB runs a hook once, right after the server has read the
// notification channel — the window in which another user's save can land
// between the handler's read and its write.
type readHookDB struct {
	*pgxpool.Pool
	mu   sync.Mutex
	hook func()
}

func (d *readHookDB) arm(f func()) {
	d.mu.Lock()
	d.hook = f
	d.mu.Unlock()
}

func (d *readHookDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	row := d.Pool.QueryRow(ctx, sql, args...)
	if !strings.Contains(sql, "-- name: GetNotificationChannel ") {
		return row
	}
	d.mu.Lock()
	hook := d.hook
	d.hook = nil
	d.mu.Unlock()
	if hook == nil {
		return row
	}
	return hookedRow{row: row, after: hook}
}

type hookedRow struct {
	row   pgx.Row
	after func()
}

func (r hookedRow) Scan(dest ...any) error {
	err := r.row.Scan(dest...)
	r.after()
	return err
}

// newServerReadHook is newServer whose queries go through a readHookDB.
func newServerReadHook(t *testing.T) (http.Handler, *db.Queries, *org.Service, *readHookDB, *pgxpool.Pool) {
	t.Helper()
	pool := testutil.NewTestDB(t)
	hooked := &readHookDB{Pool: pool}
	q := db.New(hooked)
	orgSvc := org.NewService(q)
	cfg := config.Config{BaseDomain: "127-0-0-1.sslip.io", Network: "krill-net", AllowPrivateEgress: true}
	hub := deploy.NewLogHub()
	dbSvc := dbservice.New(nil, dbservice.NewDBStore(q), hub, "krill-net")
	srv := server.New(cfg, auth.NewService(q), orgSvc, q, nil, nil, hub, dbSvc)
	srv.SetHostShare(hostshare.NewStore(pool))
	srv.SetBackups(backup.New(nil, backup.NewDBStore(q), true), func() {})
	return srv.Router(), q, orgSvc, hooked, pool
}

// An organization admin's save must not write back node-alert settings it
// read before an instance operator changed them: the operator's choice is
// the one that stays, and a chat change is judged against the current row.
func TestSaveNotificationsKeepsConcurrentOperatorChange(t *testing.T) {
	h, q, orgSvc, hooked, pool := newServerReadHook(t)
	ctx := context.Background()
	opID := mkUser(t, q, "race-op@k.local")
	if err := q.SetUserAdmin(ctx, db.SetUserAdminParams{ID: opID, IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	o, _ := orgSvc.CreateOrg(ctx, opID, "Org")
	adminID := mkUser(t, q, "race-admin@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: adminID, Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	admin := loginAs(t, q, "race-admin@k.local")
	op := loginAs(t, q, "race-op@k.local")
	path := "/orgs/" + i64(o.ID) + "/notifications"
	load := func() db.NotificationChannel {
		ch, err := q.GetNotificationChannel(ctx, db.GetNotificationChannelParams{OrgID: o.ID, Type: "telegram"})
		if err != nil {
			t.Fatal(err)
		}
		return ch
	}
	operatorSets := func(infra bool, disk, mem int) func() {
		return func() {
			if _, err := pool.Exec(ctx, `UPDATE notification_channels
				SET notify_infra = $2, disk_threshold_pct = $3, mem_threshold_pct = $4
				WHERE org_id = $1 AND type = 'telegram'`, o.ID, infra, disk, mem); err != nil {
				t.Errorf("operator update: %v", err)
			}
		}
	}

	// The operator opts the channel in.
	postForm(t, h, path, op, notifyForm(url.Values{"bot_token": {"tok"}}))
	if ch := load(); !ch.NotifyInfra || ch.DiskThresholdPct != 90 || ch.MemThresholdPct != 120 {
		t.Fatalf("operator could not opt in: %+v", ch)
	}

	// The org admin saves with the same chat while the operator revokes node
	// alerts and moves the thresholds: the revocation stays.
	hooked.arm(operatorSets(false, 70, 150))
	rec := postForm(t, h, path, admin, notifyForm(url.Values{"notify_deploy": {""}}))
	if rec.Code != http.StatusSeeOther || hasErrFlash(rec) {
		t.Fatalf("org admin save: %d %q", rec.Code, flashCookieValue(rec))
	}
	if ch := load(); ch.NotifyInfra || ch.DiskThresholdPct != 70 || ch.MemThresholdPct != 150 || ch.NotifyDeploy {
		t.Fatalf("org admin's save wrote back stale node-alert settings: %+v", ch)
	}
	if v, _ := url.QueryUnescape(flashCookieValue(rec)); strings.Contains(v, "Infrastructure alerts were turned off") {
		t.Errorf("flash claims a reset the save did not make: %q", v)
	}

	// The org admin points the channel at another chat while the operator
	// opts it back in: node alerts must not follow the new chat, and the
	// flash says they were turned off.
	hooked.arm(operatorSets(true, 70, 150))
	rec = postForm(t, h, path, admin, notifyForm(url.Values{"chat_id": {"777"}}))
	if rec.Code != http.StatusSeeOther || hasErrFlash(rec) {
		t.Fatalf("org admin chat change: %d %q", rec.Code, flashCookieValue(rec))
	}
	if ch := load(); ch.NotifyInfra || ch.ChatID != "777" {
		t.Fatalf("node alerts followed a chat the operator never chose: %+v", ch)
	}
	if v, _ := url.QueryUnescape(flashCookieValue(rec)); !strings.Contains(v, "Infrastructure alerts were turned off") {
		t.Errorf("flash does not report the reset: %q", v)
	}

	// With the chat unchanged a blank token keeps the bot the row holds now,
	// and an operator's opt-in made meanwhile stays.
	stored := load().BotToken
	hooked.arm(operatorSets(true, 70, 150))
	rec = postForm(t, h, path, admin, notifyForm(url.Values{"chat_id": {"777"}}))
	if rec.Code != http.StatusSeeOther || hasErrFlash(rec) {
		t.Fatalf("org admin save: %d %q", rec.Code, flashCookieValue(rec))
	}
	if ch := load(); !ch.NotifyInfra || ch.BotToken != stored {
		t.Fatalf("operator's opt-in or the stored bot was lost: %+v", ch)
	}
}
