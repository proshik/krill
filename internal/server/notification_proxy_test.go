package server_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/backup"
	"github.com/proshik/krill/internal/config"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/hostshare"
	"github.com/proshik/krill/internal/notify"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/testutil"
)

// newServerWithTelegramProxy is newServer with a notify service whose
// Telegram proxy setting is raw (as KRILL_TELEGRAM_PROXY at startup).
func newServerWithTelegramProxy(t *testing.T, raw string) (http.Handler, *db.Queries, *org.Service) {
	t.Helper()
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	orgSvc := org.NewService(q)
	cfg := config.Config{BaseDomain: "127-0-0-1.sslip.io", Network: "krill-net"}
	hub := deploy.NewLogHub()
	srv := server.New(cfg, auth.NewService(q), orgSvc, q, nil, nil, hub, dbservice.New(nil, dbservice.NewDBStore(q), hub, "krill-net"))
	srv.SetHostShare(hostshare.NewStore(pool))
	srv.SetBackups(backup.New(nil, backup.NewDBStore(q), true), func() {})
	svc := notify.New(notify.NewDBStore(q))
	svc.SetTelegramProxy(raw) // a bad value is reported on the page, not here
	srv.SetNotify(svc)
	return srv.Router(), q, orgSvc
}

// The notifications page tells whether Telegram goes through a proxy: the
// address (never its credentials) to an instance operator, the fact alone to
// an organization admin, and a rejected setting as an error to both.
func TestNotificationsPageShowsTelegramProxy(t *testing.T) {
	for _, c := range []struct {
		name, raw     string
		operator, adm []string // must appear for the operator / the org admin
	}{
		{"unset", "", nil, nil},
		{"set", "socks5h://krill:s3cret-pw@127.0.0.1:1080",
			[]string{"Telegram messages are sent via proxy socks5h://127.0.0.1:1080"},
			[]string{"sent via a proxy set by the installation"}},
		{"invalid", "krill:s3cret-pw@127.0.0.1:1080",
			[]string{"The Telegram proxy is set incorrectly: unsupported or missing scheme", "Messages are not sent"},
			[]string{"The Telegram proxy is set incorrectly: unsupported or missing scheme"}},
		{"no host", "socks5h://",
			[]string{"set incorrectly: no proxy host"}, []string{"set incorrectly: no proxy host"}},
		{"a path", "socks5h://127.0.0.1:1080/x",
			[]string{"set incorrectly: a proxy URL has no path"}, []string{"set incorrectly: a proxy URL has no path"}},
		{"not a URL", "socks5h://krill:s3cret-pw@127.0.0.1:port",
			[]string{"set incorrectly: not a valid URL"}, []string{"set incorrectly: not a valid URL"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, q, orgSvc := newServerWithTelegramProxy(t, c.raw)
			ctx := context.Background()
			opID := mkUser(t, q, "proxy-op@k.local")
			if err := q.SetUserAdmin(ctx, db.SetUserAdminParams{ID: opID, IsAdmin: true}); err != nil {
				t.Fatal(err)
			}
			o, _ := orgSvc.CreateOrg(ctx, opID, "Org")
			adminID := mkUser(t, q, "proxy-admin@k.local")
			if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: adminID, Role: "admin"}); err != nil {
				t.Fatal(err)
			}
			path := "/orgs/" + i64(o.ID) + "/notifications"
			for who, want := range map[string][]string{"proxy-op@k.local": c.operator, "proxy-admin@k.local": c.adm} {
				rec := getPage(t, h, path, loginAs(t, q, who), nil)
				if rec.Code != http.StatusOK {
					t.Fatalf("%s: %d", who, rec.Code)
				}
				body := rec.Body.String()
				if len(want) == 0 && strings.Contains(body, `id="notif-proxy"`) {
					t.Errorf("%s: a proxy line without a proxy", who)
				}
				for _, w := range want {
					if !strings.Contains(body, w) {
						t.Errorf("%s: page lacks %q", who, w)
					}
				}
				for _, leak := range []string{"s3cret", "krill:"} {
					if strings.Contains(body, leak) {
						t.Errorf("%s: page shows %q", who, leak)
					}
				}
				if who == "proxy-admin@k.local" && strings.Contains(body, "127.0.0.1:1080") {
					t.Errorf("an organization admin sees the proxy address")
				}
				if who == "proxy-op@k.local" && strings.Contains(body, "set by the installation") {
					t.Errorf("the operator gets the address-less sentence")
				}
			}
		})
	}
}
