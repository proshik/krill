package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/auth"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/org"
)

// rpFixture builds an org→project→env→app chain with one exposed primary domain,
// owned by email, and returns the base app URL, the owner cookie, and the app /
// domain / org IDs.
func rpFixture(t *testing.T, h http.Handler, q *db.Queries, orgSvc *org.Service, email string) (base string, ownerCookie *http.Cookie, appID, domID, orgID int64) {
	t.Helper()
	ctx := context.Background()
	ownerID := mkUser(t, q, email)
	// Derive a globally-unique token from the (unique) email local part: the org
	// slug, applications.domain, and domains.host are all UNIQUE across the DB, so
	// two fixtures in one test must not reuse a literal name.
	sub := strings.ToLower(strings.NewReplacer("@", "-", ".", "-", "_", "-").Replace(strings.Split(email, "@")[0]))
	host := sub + ".rp.example.com"
	o, err := orgSvc.CreateOrg(ctx, ownerID, "Org-"+sub)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	p, _ := orgSvc.CreateProject(ctx, o.ID, "Proj", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	a, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: sub, Image: "nginx", Tag: "alpine",
		Domain: host, Port: 80,
		SourceType: "image", GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	d, err := q.CreateDomain(ctx, db.CreateDomainParams{
		ApplicationID: a.ID, Host: host, Tls: false, IsPrimary: true, Exposed: true, Paths: "",
	})
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}
	base = "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID) + "/apps/" + i64(a.ID)
	return base, loginAs(t, q, email), a.ID, d.ID, o.ID
}

// basicAuthHash returns the stored bcrypt hash for a username in a newline-separated
// "user:hash" list.
func basicAuthHash(s, user string) (string, bool) {
	for _, line := range strings.Split(s, "\n") {
		if e := strings.TrimSpace(line); strings.HasPrefix(e, user+":") {
			return strings.SplitN(e, ":", 2)[1], true
		}
	}
	return "", false
}

// TestAddDomainBasicAuthUser: a valid add stores "user:bcrypthash" (verifiable with
// the original password) and only the username is reconstructable for the UI.
func TestAddDomainBasicAuthUser(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, _, domID, _ := rpFixture(t, h, q, orgSvc, "rp-add@k.local")

	rec := postForm(t, h, base+"/domains/"+i64(domID)+"/basic-auth", cookie, url.Values{
		"username": {"alice"}, "password": {"s3cret"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("add basic-auth want 303, got %d body: %s", rec.Code, rec.Body.String())
	}
	d, err := q.GetDomain(ctx, domID)
	if err != nil {
		t.Fatalf("GetDomain: %v", err)
	}
	hash, ok := basicAuthHash(d.BasicAuthUsers, "alice")
	if !ok {
		t.Fatalf("alice not stored; basic_auth_users=%q", d.BasicAuthUsers)
	}
	if !auth.CheckPassword(hash, "s3cret") {
		t.Errorf("stored hash does not verify the original password")
	}
	if strings.Contains(d.BasicAuthUsers, "s3cret") {
		t.Errorf("plaintext password leaked into storage: %q", d.BasicAuthUsers)
	}
}

// TestAddDomainBasicAuthRejects: duplicate username, invalid username, and empty
// password are all rejected with an err flash and leave the list unchanged.
func TestAddDomainBasicAuthRejects(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, _, domID, _ := rpFixture(t, h, q, orgSvc, "rp-rej@k.local")
	authURL := base + "/domains/" + i64(domID) + "/basic-auth"

	// seed one user
	if rec := postForm(t, h, authURL, cookie, url.Values{"username": {"bob"}, "password": {"pw"}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("seed want 303, got %d", rec.Code)
	}

	cases := []struct {
		name string
		form url.Values
	}{
		{"duplicate", url.Values{"username": {"bob"}, "password": {"pw2"}}},
		{"invalid-username-colon", url.Values{"username": {"a:b"}, "password": {"pw"}}},
		{"invalid-username-space", url.Values{"username": {"a b"}, "password": {"pw"}}},
		{"empty-password", url.Values{"username": {"carol"}, "password": {""}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postForm(t, h, authURL, cookie, tc.form)
			if rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
				t.Fatalf("%s want 303+err flash, got %d flash=%q", tc.name, rec.Code, flashCookieValue(rec))
			}
		})
	}
	d, _ := q.GetDomain(ctx, domID)
	if got := basicAuthEntriesCount(d.BasicAuthUsers); got != 1 {
		t.Fatalf("expected exactly 1 user after rejected adds, got %d (%q)", got, d.BasicAuthUsers)
	}
}

func basicAuthEntriesCount(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// TestDeleteDomainBasicAuthUser: removing a username drops only that entry.
func TestDeleteDomainBasicAuthUser(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, _, domID, _ := rpFixture(t, h, q, orgSvc, "rp-del@k.local")
	authURL := base + "/domains/" + i64(domID) + "/basic-auth"

	for _, u := range []string{"alice", "bob"} {
		if rec := postForm(t, h, authURL, cookie, url.Values{"username": {u}, "password": {"pw"}}); rec.Code != http.StatusSeeOther {
			t.Fatalf("seed %s want 303, got %d", u, rec.Code)
		}
	}
	rec := postForm(t, h, authURL+"/delete", cookie, url.Values{"username": {"alice"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete want 303, got %d", rec.Code)
	}
	d, _ := q.GetDomain(ctx, domID)
	if _, ok := basicAuthHash(d.BasicAuthUsers, "alice"); ok {
		t.Errorf("alice still present after delete: %q", d.BasicAuthUsers)
	}
	if _, ok := basicAuthHash(d.BasicAuthUsers, "bob"); !ok {
		t.Errorf("bob was removed too: %q", d.BasicAuthUsers)
	}
}

// TestSetDomainAllowedIPs: valid CIDRs/IPs persist (bare IP normalized to /32),
// junk input is rejected and nothing is saved.
func TestSetDomainAllowedIPs(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, _, domID, _ := rpFixture(t, h, q, orgSvc, "rp-ips@k.local")
	ipURL := base + "/domains/" + i64(domID) + "/allowed-ips"

	rec := postForm(t, h, ipURL, cookie, url.Values{"ips": {"10.0.0.0/8\n1.2.3.4"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("set ips want 303, got %d", rec.Code)
	}
	d, _ := q.GetDomain(ctx, domID)
	if d.AllowedIps != "10.0.0.0/8\n1.2.3.4/32" {
		t.Errorf("allowed_ips want %q, got %q", "10.0.0.0/8\n1.2.3.4/32", d.AllowedIps)
	}

	// junk line -> rejected, previous value untouched
	rec = postForm(t, h, ipURL, cookie, url.Values{"ips": {"10.0.0.0/8\nnope"}})
	if rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("junk ips want 303+err flash, got %d flash=%q", rec.Code, flashCookieValue(rec))
	}
	d, _ = q.GetDomain(ctx, domID)
	if d.AllowedIps != "10.0.0.0/8\n1.2.3.4/32" {
		t.Errorf("allowed_ips changed on rejected input: %q", d.AllowedIps)
	}
}

// TestSetDomainAllowedIPsNormalize: an IPv4-mapped IPv6 literal must normalize to
// a single-host /32 (not collapse to the ::/32 block), and duplicates are folded.
func TestSetDomainAllowedIPsNormalize(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, _, domID, _ := rpFixture(t, h, q, orgSvc, "rp-ipnorm@k.local")

	rec := postForm(t, h, base+"/domains/"+i64(domID)+"/allowed-ips", cookie, url.Values{
		"ips": {"::ffff:1.2.3.4\n1.2.3.4\n1.2.3.4\n1.2.3.0/24"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303, got %d", rec.Code)
	}
	d, _ := q.GetDomain(ctx, domID)
	if d.AllowedIps != "1.2.3.4/32\n1.2.3.0/24" {
		t.Errorf("allowed_ips want %q, got %q", "1.2.3.4/32\n1.2.3.0/24", d.AllowedIps)
	}
}

// TestDomainProtectionCrossTenant: org-B owner cannot mutate org-A's domain
// protection by injecting org-A's domainID into the org-B URL (must 404).
func TestDomainProtectionCrossTenant(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	_, _, _, aDomID, _ := rpFixture(t, h, q, orgSvc, "rp-tenantA@k.local")
	baseB, cookieB, _, _, _ := rpFixture(t, h, q, orgSvc, "rp-tenantB@k.local")

	for _, suffix := range []string{
		"/domains/" + i64(aDomID) + "/basic-auth",
		"/domains/" + i64(aDomID) + "/basic-auth/delete",
		"/domains/" + i64(aDomID) + "/allowed-ips",
	} {
		req := httptest.NewRequest(http.MethodPost, baseB+suffix, strings.NewReader(""))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookieB)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("SECURITY FINDING: POST %s expected 404, got %d", baseB+suffix, rec.Code)
		}
	}
	if _, err := q.GetDomain(ctx, aDomID); err != nil {
		t.Fatalf("org-A domain mutated/deleted cross-tenant: %v", err)
	}
}

// TestMemberCannotMutateDomainProtection: a read-only member is blocked (403) by
// the RequireRole(admin) group before the handler runs.
func TestMemberCannotMutateDomainProtection(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, _, _, domID, orgID := rpFixture(t, h, q, orgSvc, "rp-gate-owner@k.local")

	memberID := mkUser(t, q, "rp-gate-member@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: orgID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatalf("create member: %v", err)
	}
	memberCookie := loginAs(t, q, "rp-gate-member@k.local")

	for _, suffix := range []string{
		"/domains/" + i64(domID) + "/basic-auth",
		"/domains/" + i64(domID) + "/allowed-ips",
	} {
		rec := postForm(t, h, base+suffix, memberCookie, url.Values{"username": {"x"}, "password": {"y"}, "ips": {"1.2.3.4"}})
		if rec.Code != http.StatusForbidden {
			t.Errorf("member POST %s want 403, got %d", suffix, rec.Code)
		}
	}
}
