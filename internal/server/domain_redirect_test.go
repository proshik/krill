package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
)

// redirectFixture: app with a TLS primary (old.rd.example.com) and a second
// domain (new.rd.example.com); labels land in eng.
func redirectFixture(t *testing.T, email string) (h http.Handler, q *db.Queries, eng *labelEngine, base string, cookie *http.Cookie, appID, primaryID, otherID int64) {
	t.Helper()
	eng = &labelEngine{}
	h, q, orgSvc := newServerWithLabelEngine(t, eng)
	base, cookie, appID, primaryID, _ = rpFixture(t, h, q, orgSvc, email)
	d, err := q.CreateDomain(context.Background(), db.CreateDomainParams{ApplicationID: appID, Host: "second-" + i64(appID) + ".rd.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	return h, q, eng, base, cookie, appID, primaryID, d.ID
}

func TestDomainRedirectRefusedOnPrimary(t *testing.T) {
	h, q, _, base, cookie, _, primaryID, _ := redirectFixture(t, "rd-primary@k.local")
	rec := postForm(t, h, base+"/domains/"+i64(primaryID)+"/redirect", cookie, url.Values{"redirect": {"on"}})
	if !hasErrFlash(rec) || !strings.Contains(flashText(rec), "primary domain cannot redirect") {
		t.Fatalf("want a primary refusal, got %q", flashText(rec))
	}
	if d, _ := q.GetDomain(context.Background(), primaryID); d.RedirectToPrimary {
		t.Fatal("primary marked as redirecting")
	}
}

func TestDomainRedirectLabelsFollowThePrimary(t *testing.T) {
	h, q, eng, base, cookie, appID, primaryID, otherID := redirectFixture(t, "rd-labels@k.local")
	ctx := context.Background()
	primary, _ := q.GetDomain(ctx, primaryID)
	other, _ := q.GetDomain(ctx, otherID)
	redir, err := q.CreateDomain(ctx, db.CreateDomainParams{ApplicationID: appID, Host: "third-" + i64(appID) + ".rd.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	// Rows are listed primary first, then by creation: redir is always d2.
	svc := "krill-" + i64(appID)
	replacement := func() string {
		return eng.labelsOf(svc)["traefik.http.middlewares."+svc+"-d2-toprimary.redirectregex.replacement"]
	}

	rec := postForm(t, h, base+"/domains/"+i64(redir.ID)+"/redirect", cookie, url.Values{"redirect": {"on"}})
	if hasErrFlash(rec) {
		t.Fatalf("redirect refused: %q", flashText(rec))
	}
	if got := replacement(); got != "http://"+primary.Host+"${1}" {
		t.Fatalf("replacement = %q (labels %v)", got, eng.labelsOf(svc))
	}
	if got := eng.labelsOf(svc)["traefik.http.routers."+svc+"-d2.rule"]; got != "Host(`"+redir.Host+"`)" {
		t.Fatalf("redirect rule = %q", got)
	}

	// Make primary alone re-applies the labels: the existing redirect follows.
	rec = postForm(t, h, base+"/domains/"+i64(otherID)+"/primary", cookie, nil)
	if hasErrFlash(rec) {
		t.Fatalf("make primary refused: %q", flashText(rec))
	}
	if got := replacement(); got != "http://"+other.Host+"${1}" {
		t.Fatalf("after primary change replacement = %q", got)
	}
	app, _ := q.GetApplication(ctx, appID)
	if app.Domain != other.Host {
		t.Fatalf("applications.domain = %s", app.Domain)
	}

	// A redirecting domain cannot become primary, nor be deleted as one.
	rec = postForm(t, h, base+"/domains/"+i64(redir.ID)+"/primary", cookie, nil)
	if !hasErrFlash(rec) {
		t.Fatal("a redirecting domain became primary")
	}
	// The former primary is an ordinary domain now and can redirect too.
	rec = postForm(t, h, base+"/domains/"+i64(primaryID)+"/redirect", cookie, url.Values{"redirect": {"on"}})
	if hasErrFlash(rec) {
		t.Fatalf("redirect of the former primary refused: %q", flashText(rec))
	}
	rec = postForm(t, h, base+"/domains/"+i64(otherID)+"/delete", cookie, nil)
	if !hasErrFlash(rec) {
		t.Fatal("the new primary was deleted")
	}

	page := getWithCookie(t, h, base+"?tab=domains", cookie).Body.String()
	if !strings.Contains(page, "Redirects to "+other.Host) || !strings.Contains(page, "Exposure, paths and access protection do not apply") {
		t.Fatal("domains tab does not show the redirect")
	}
}

func TestDomainRedirectCrossTenantAndMember(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	baseA, _, appA, _, orgA := rpFixture(t, h, q, orgSvc, "rd-tenant-a@k.local")
	baseB, cookieB, _, _, _ := rpFixture(t, h, q, orgSvc, "rd-tenant-b@k.local")
	extra, err := q.CreateDomain(ctx, db.CreateDomainParams{ApplicationID: appA, Host: "extra.rd.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"/redirect", "/primary"} {
		target := baseB + "/domains/" + i64(extra.ID) + suffix
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader("redirect=on"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookieB)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("SECURITY FINDING: POST %s expected 404, got %d", target, rec.Code)
		}
	}
	memberID := mkUser(t, q, "rd-member@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: orgA, UserID: memberID, Role: "member"}); err != nil {
		t.Fatal(err)
	}
	member := loginAs(t, q, "rd-member@k.local")
	for _, suffix := range []string{"/redirect", "/primary"} {
		if rec := postForm(t, h, baseA+"/domains/"+i64(extra.ID)+suffix, member, url.Values{"redirect": {"on"}}); rec.Code != http.StatusForbidden {
			t.Errorf("member POST %s want 403, got %d", suffix, rec.Code)
		}
	}
	d, _ := q.GetDomain(ctx, extra.ID)
	if d.RedirectToPrimary || d.IsPrimary {
		t.Fatalf("domain mutated: %+v", d)
	}
}
