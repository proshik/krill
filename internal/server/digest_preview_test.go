package server_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/backup"
	"github.com/proshik/krill/internal/config"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/hostshare"
	"github.com/proshik/krill/internal/metrics"
	"github.com/proshik/krill/internal/notify"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/testutil"
)

// stateBackupsFake is the digest's view of a Krill state backup that is set
// up and ran recently.
type stateBackupsFake struct{}

func (stateBackupsFake) StateBackupStatus(context.Context, time.Time) (notify.StateStatus, error) {
	return notify.StateStatus{Configured: true, Since: time.Now().Add(-48 * time.Hour), LastSuccess: time.Now().Add(-time.Hour)}, nil
}
func (stateBackupsFake) PruneStateRuns(context.Context, time.Time) error { return nil }

func newServerWithDigest(t *testing.T) (http.Handler, *db.Queries, *org.Service) {
	t.Helper()
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	orgSvc := org.NewService(q)
	cfg := config.Config{BaseDomain: "127-0-0-1.sslip.io", Network: "krill-net"}
	hub := deploy.NewLogHub()
	srv := server.New(cfg, auth.NewService(q), orgSvc, q, nil, nil, hub, dbservice.New(nil, dbservice.NewDBStore(q), hub, "krill-net"))
	srv.SetHostShare(hostshare.NewStore(pool))
	srv.SetBackups(backup.New(nil, backup.NewDBStore(q), true), func() {})
	store := notify.NewDBStore(q)
	svc := notify.New(store)
	srv.SetNotify(svc)
	d := notify.NewDigest(svc, store, metrics.NewDBStore(q), nil)
	d.SetStateBackups(stateBackupsFake{})
	srv.SetDigest(d)
	return srv.Router(), q, orgSvc
}

// The digest preview shows node data and Krill's state backup only to an
// instance operator: an organization admin looking at the same
// infrastructure channel gets the apps' part without them.
func TestDigestPreviewInstanceAdminBoundary(t *testing.T) {
	h, q, orgSvc := newServerWithDigest(t)
	ctx := context.Background()
	operator := mkUser(t, q, "preview-operator@k.local")
	if err := q.SetUserAdmin(ctx, db.SetUserAdminParams{ID: operator, IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	o, _ := orgSvc.CreateOrg(ctx, operator, "Preview")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "shop", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	mkApp(t, q, e.ID, "storefront-api", "preview-api.example.com")
	admin := mkUser(t, q, "preview-admin@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: admin, Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	if err := q.UpsertNodeCapacity(ctx, db.UpsertNodeCapacityParams{Node: "secret-node-7", Ncpu: 4, MemTotalBytes: 8 << 30}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.UpsertNotificationChannel(ctx, db.UpsertNotificationChannelParams{
		OrgID: o.ID, Type: "telegram", BotToken: "000000:local-test", ChatID: "1",
		NotifyDeploy: true, NotifyInfra: true, DiskThresholdPct: 85, MemThresholdPct: 100,
		DigestEnabled: true, DigestMinute: 660, DigestTz: "UTC", Lang: "en",
	}); err != nil {
		t.Fatal(err)
	}
	path := "/orgs/" + i64(o.ID) + "/notifications"

	body := getWithCookie(t, h, path, loginAs(t, q, "preview-operator@k.local")).Body.String()
	if !strings.Contains(body, "tg-bubble") || !strings.Contains(body, "storefront-api") {
		t.Fatalf("no digest preview for the operator:\n%s", excerpt(body, `class="tg"`))
	}
	if !strings.Contains(body, "secret-node-7") || !strings.Contains(body, "Krill state") {
		t.Fatalf("the operator's preview lacks the node part:\n%s", excerpt(body, "tg-bubble"))
	}

	body = getWithCookie(t, h, path, loginAs(t, q, "preview-admin@k.local")).Body.String()
	if !strings.Contains(body, "tg-bubble") || !strings.Contains(body, "storefront-api") {
		t.Fatalf("no digest preview for the organization admin:\n%s", excerpt(body, `class="tg"`))
	}
	if strings.Contains(body, "secret-node-7") || strings.Contains(body, "Krill state") {
		t.Fatal("SECURITY: an organization admin's preview shows node data or Krill's state backup")
	}
}

// The notifications page's digest preview names a node's cores in the right
// plural form for one core and for several, in the channel's language: msk's
// one-core nodes read "от мощности всех ядер (1)".
func TestDigestPreviewCoreCounts(t *testing.T) {
	h, q, orgSvc := newServerWithDigest(t)
	ctx := context.Background()
	operator := mkUser(t, q, "cores-operator@k.local")
	if err := q.SetUserAdmin(ctx, db.SetUserAdminParams{ID: operator, IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	o, _ := orgSvc.CreateOrg(ctx, operator, "Cores")
	for node, ncpu := range map[string]int32{"node-one": 1, "node-four": 4} {
		if err := q.UpsertNodeCapacity(ctx, db.UpsertNodeCapacityParams{Node: node, Ncpu: ncpu, MemTotalBytes: 8 << 30}); err != nil {
			t.Fatal(err)
		}
		if err := q.InsertMetricSample(ctx, db.InsertMetricSampleParams{Node: node, Component: "krill-1", CpuPct: 40, MemBytes: 1 << 20}); err != nil {
			t.Fatal(err)
		}
	}
	path := "/orgs/" + i64(o.ID) + "/notifications"
	cookie := loginAs(t, q, "cores-operator@k.local")
	for lang, want := range map[string][]string{
		"ru": {"от мощности 1 ядра", "от мощности всех 4 ядер"},
		"en": {"of 1 core", "of all 4 cores"},
	} {
		if _, err := q.UpsertNotificationChannel(ctx, db.UpsertNotificationChannelParams{
			OrgID: o.ID, Type: "telegram", BotToken: "000000:local-test", ChatID: "1",
			NotifyDeploy: true, NotifyInfra: true, DiskThresholdPct: 85, MemThresholdPct: 100,
			DigestEnabled: true, DigestMinute: 660, DigestTz: "UTC", Lang: lang,
		}); err != nil {
			t.Fatal(err)
		}
		body := getWithCookie(t, h, path, cookie).Body.String()
		preview := excerpt(body, "tg-bubble")
		for _, w := range want {
			if !strings.Contains(body, w) {
				t.Errorf("%s preview lacks %q:\n%s", lang, w, preview)
			}
		}
		for _, bad := range []string{"всех ядер (", "all cores ("} {
			if strings.Contains(body, bad) {
				t.Errorf("%s preview still shows %q", lang, bad)
			}
		}
	}
}
