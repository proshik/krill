package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/proshik/krill/internal/auth"
	"github.com/proshik/krill/internal/config"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/deploy"
	"github.com/proshik/krill/internal/firewall"
	"github.com/proshik/krill/internal/org"
	"github.com/proshik/krill/internal/server"
	"github.com/proshik/krill/internal/testutil"
)

// hostFirewall simulates the nft/systemd state of the control-plane host, keyed
// off the commands internal/firewall sends.
type hostFirewall struct {
	mu        sync.Mutex
	locked    bool
	pending   bool
	confirms  int
	refused   int // Applies refused because a switch was already armed
	rulesets  []string
	persisted string // ruleset saved for boot ("" = none)
}

func (h *hostFirewall) Run(_ context.Context, stdin, cmd string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case strings.HasPrefix(cmd, "# krill-fw-probe"):
		out := "nft yes\n"
		if h.locked && len(h.rulesets) > 0 {
			out += "table yes\nlive " + firewall.RulesetID(h.rulesets[len(h.rulesets)-1]) + "\n"
		} else {
			out += "table no\n"
		}
		if h.pending {
			out += "pending yes\n"
		} else {
			out += "pending no\n"
		}
		if h.persisted != "" {
			out += "persisted yes\npersisted-id " + firewall.RulesetID(h.persisted) + "\nunit yes\n"
		} else {
			out += "unit no\n"
		}
		return out, nil
	case strings.HasPrefix(cmd, "# krill-fw-disarm"): // Confirm's disarm
		if !h.pending {
			return "expired\n", nil
		}
		h.pending = false
		h.confirms++
		return "disarmed\n", nil
	case strings.HasPrefix(cmd, "set -e\nf="): // Confirm's persist script
		if h.locked && len(h.rulesets) > 0 {
			h.persisted = h.rulesets[len(h.rulesets)-1]
			return "persisted\n", nil
		}
		h.persisted = ""
		return "removed\n", nil
	case strings.Contains(cmd, "systemd-run"): // Apply's arm script
		if h.pending {
			h.refused++
			return "pending 1700000000\n", nil
		}
		h.pending = true
		return "armed 1700000000\n", nil
	case strings.HasPrefix(cmd, "nft list table"): // GatewayOnly
		if h.locked && len(h.rulesets) > 0 {
			return h.rulesets[len(h.rulesets)-1], nil
		}
		return "", nil
	case strings.Contains(cmd, "systemctl is-active"):
		if h.pending {
			return "yes\n", nil
		}
		return "no\n", nil
	case strings.Contains(cmd, "command -v nft"):
		if h.locked {
			return "yes\n", nil
		}
		return "no\n", nil
	case cmd == "nft -f -":
		h.locked = true
		h.rulesets = append(h.rulesets, stdin)
	case strings.Contains(cmd, "nft delete table"): // Open
		h.locked, h.pending, h.persisted = false, false, ""
	case strings.Contains(cmd, "systemctl stop"): // Confirm
		h.pending = false
		h.confirms++
	}
	return "", nil
}

func newFirewallServer(t *testing.T, advertise string, fw *hostFirewall) (http.Handler, *db.Queries, *org.Service) {
	t.Helper()
	pool := testutil.NewTestDB(t)
	q := db.New(pool)
	orgSvc := org.NewService(q)
	cfg := config.Config{BaseDomain: "127-0-0-1.sslip.io", Network: "krill-net", ListenAddr: ":8080", AdvertiseAddr: advertise}
	hub := deploy.NewLogHub()
	srv := server.New(cfg, auth.NewService(q), orgSvc, q, nil, nil, hub, dbservice.New(nil, dbservice.NewDBStore(q), hub, "krill-net"))
	srv.SetControlPlaneFirewall(fw)
	return srv.Router(), q, orgSvc
}

// Without the manager's address the allowlist would trust the workers but not
// the manager; the lockdown must refuse before touching any host.
func TestLockdownRefusesWithoutAdvertiseAddr(t *testing.T) {
	fw := &hostFirewall{}
	h, q, orgSvc := newFirewallServer(t, "", fw)
	base, cookie, _ := nodesOrg(t, q, orgSvc, "fw-noadv@k.local", "OrgFWNoAdv")
	rec := postForm(t, h, base+"/firewall/lockdown", cookie, url.Values{})
	if rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("want 303+err flash, got %d %q", rec.Code, flashCookieValue(rec))
	}
	if fw.locked || fw.pending {
		t.Fatal("control plane must not be touched when the advertise address is unset")
	}
}

// The full control-plane protocol: apply with the UI and ingress ports kept
// open, leave the dead-man switch armed, confirm only on the operator's
// follow-up request, then open again.
func TestControlPlaneLockdownConfirmOpen(t *testing.T) {
	fw := &hostFirewall{}
	// A hostname advertise address must resolve, not be rejected.
	h, q, orgSvc := newFirewallServer(t, "localhost", fw)
	base, cookie, _ := nodesOrg(t, q, orgSvc, "fw-cp@k.local", "OrgFWCP")

	rec := postForm(t, h, base+"/firewall/lockdown", cookie, url.Values{})
	if rec.Code != http.StatusSeeOther || hasErrFlash(rec) {
		t.Fatalf("lockdown want 303 without err flash, got %d %q", rec.Code, flashCookieValue(rec))
	}
	if !fw.locked || !fw.pending || fw.confirms != 0 {
		t.Fatalf("control plane must be locked with the revert still armed: locked=%v pending=%v confirms=%d", fw.locked, fw.pending, fw.confirms)
	}
	if got := rec.Header().Get("Connection"); got != "close" {
		t.Fatalf("lockdown response must close its connection, got %q", got)
	}
	if len(fw.rulesets) != 1 || !strings.Contains(fw.rulesets[0], "tcp dport { 80, 443, 8080 } accept") {
		t.Fatalf("control-plane ruleset must keep ingress and UI ports open: %q", fw.rulesets)
	}

	req := httptest.NewRequest(http.MethodGet, base+"/firewall", nil)
	req.AddCookie(cookie)
	page := httptest.NewRecorder()
	h.ServeHTTP(page, req)
	if page.Code != http.StatusOK {
		t.Fatalf("GET /firewall want 200, got %d", page.Code)
	}
	if page.Header().Get("Connection") != "close" || !strings.Contains(page.Body.String(), "/firewall/confirm") {
		t.Fatal("a pending lockdown must render the confirm form over a closing connection")
	}

	rec = postForm(t, h, base+"/firewall/confirm", cookie, url.Values{})
	if rec.Code != http.StatusSeeOther || hasErrFlash(rec) || fw.pending || fw.confirms != 1 {
		t.Fatalf("confirm: code=%d flash=%q pending=%v confirms=%d", rec.Code, flashCookieValue(rec), fw.pending, fw.confirms)
	}
	// Only a confirmed ruleset is saved for boot, and it is the applied one.
	if fw.persisted != fw.rulesets[0] {
		t.Fatalf("confirm must persist the confirmed ruleset, persisted=%q", fw.persisted)
	}
	// The switch is gone: a second confirm has nothing to cancel.
	rec = postForm(t, h, base+"/firewall/confirm", cookie, url.Values{})
	if !hasErrFlash(rec) || fw.confirms != 1 {
		t.Fatalf("confirm without a pending revert must err, got %q confirms=%d", flashCookieValue(rec), fw.confirms)
	}

	rec = postForm(t, h, base+"/firewall/open", cookie, url.Values{})
	if rec.Code != http.StatusSeeOther || fw.locked || fw.persisted != "" {
		t.Fatalf("open must remove the control-plane table and its boot copy: code=%d locked=%v persisted=%v", rec.Code, fw.locked, fw.persisted != "")
	}
}

// A second lockdown while the first is unconfirmed must leave the host alone:
// applying on top would replace the snapshot the armed switch restores.
func TestControlPlaneLockdownRefusedWhileArmed(t *testing.T) {
	fw := &hostFirewall{}
	h, q, orgSvc := newFirewallServer(t, "localhost", fw)
	base, cookie, _ := nodesOrg(t, q, orgSvc, "fw-armed@k.local", "OrgFWArmed")

	if rec := postForm(t, h, base+"/firewall/lockdown", cookie, url.Values{}); hasErrFlash(rec) {
		t.Fatalf("first lockdown: %q", flashCookieValue(rec))
	}
	rec := postForm(t, h, base+"/firewall/lockdown", cookie, url.Values{})
	if !hasErrFlash(rec) || fw.refused != 1 || len(fw.rulesets) != 1 {
		t.Fatalf("second lockdown must be refused without applying: flash=%q refused=%d rulesets=%d", flashCookieValue(rec), fw.refused, len(fw.rulesets))
	}
	if msg, _ := url.QueryUnescape(flashCookieValue(rec)); !strings.Contains(msg, "awaiting confirmation") || strings.Contains(msg, "%!") {
		t.Fatalf("the refusal must say a change is awaiting confirmation, got %q", msg)
	}
}

// After a reboot that did not reload the table (or a flush by another tool),
// the page must say the lockdown is missing instead of showing the node open.
func TestFirewallPageShowsMissingTable(t *testing.T) {
	fw := &hostFirewall{}
	h, q, orgSvc := newFirewallServer(t, "localhost", fw)
	base, cookie, _ := nodesOrg(t, q, orgSvc, "fw-missing@k.local", "OrgFWMissing")
	postForm(t, h, base+"/firewall/lockdown", cookie, url.Values{})
	postForm(t, h, base+"/firewall/confirm", cookie, url.Values{})
	if fw.persisted == "" {
		t.Fatal("setup: confirm must persist")
	}
	fw.mu.Lock()
	fw.locked = false // the table is gone; the boot copy is still there
	fw.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, base+"/firewall", nil)
	req.AddCookie(cookie)
	page := httptest.NewRecorder()
	h.ServeHTTP(page, req)
	body := page.Body.String()
	if page.Code != http.StatusOK || !strings.Contains(body, "no ruleset is loaded now") {
		t.Fatalf("missing table not reported: %d\n%s", page.Code, body)
	}
}
