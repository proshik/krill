package server_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/org"
)

// apiTokenTestOwnerEmail is the fixed identity loginAsOwner mints, so
// userIDFrom can look it back up without threading an id through every test.
const apiTokenTestOwnerEmail = "token-owner@k.local"

// loginAsOwner creates a fresh user, an org they own (satisfying
// RequireRole(RoleAdmin) for token creation), and returns a login cookie for
// that user plus the new org's id. h is accepted (not exercised directly
// here — login goes through the auth service, mirroring loginAs in
// app_rbac_test.go) to match the call shape callers expect from a
// form-login-based helper.
func loginAsOwner(t *testing.T, h http.Handler, q *db.Queries, orgSvc *org.Service) (*http.Cookie, int64) {
	t.Helper()
	uid := mkUser(t, q, apiTokenTestOwnerEmail)
	o, err := orgSvc.CreateOrg(context.Background(), uid, "Token Org")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	return loginAs(t, q, apiTokenTestOwnerEmail), o.ID
}

// userIDFrom looks up the user loginAsOwner created.
func userIDFrom(t *testing.T, q *db.Queries) int64 {
	t.Helper()
	u, err := q.GetUserByEmail(context.Background(), apiTokenTestOwnerEmail)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	return u.ID
}

// TestCreateTokenShowsPlaintextOnceThenNever pins the one property this
// feature exists to protect: the plaintext token is handed back exactly once
// (via the one-shot flash cookie), a later page load never contains it again,
// and the stored row holds only its hash — never the plaintext.
func TestCreateTokenShowsPlaintextOnceThenNever(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	cookie, orgID := loginAsOwner(t, h, q, orgSvc)

	form := url.Values{"name": {"laptop"}, "level": {"write"}, "expires": {"never"}}
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/orgs/%d/api-tokens", orgID), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create want 303, got %d", rec.Code)
	}

	// The plaintext travels in a one-shot flash cookie, then the page renders it.
	var flash *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if strings.Contains(c.Name, "flash") && strings.Contains(c.Value, "krill_pat_") {
			flash = c
		}
	}
	if flash == nil {
		t.Fatal("plaintext token was not handed back on creation")
	}

	listReq := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/orgs/%d/api-tokens", orgID), nil)
	listReq.AddCookie(cookie)
	listRec := httptest.NewRecorder()
	h.ServeHTTP(listRec, listReq)
	if strings.Contains(listRec.Body.String(), flash.Value) {
		t.Fatal("plaintext token is still rendered on a later page load")
	}

	rows, err := q.ListAPITokensByUserAndOrg(t.Context(), db.ListAPITokensByUserAndOrgParams{UserID: userIDFrom(t, q), OrgID: orgID})
	if err != nil || len(rows) != 1 {
		t.Fatalf("want exactly one stored token, got %d (err=%v)", len(rows), err)
	}
	if strings.Contains(rows[0].TokenHash, "krill_pat_") {
		t.Fatal("plaintext token was stored instead of its hash")
	}
}

// TestCreateTokenRequiresAdmin locks in that minting a token is admin-gated: a
// plain member (not owner/admin) must be blocked with 403 before the handler
// ever runs, so no token is created.
func TestCreateTokenRequiresAdmin(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()

	ownerID := mkUser(t, q, "tok-owner@k.local")
	o, err := orgSvc.CreateOrg(ctx, ownerID, "Tok Org")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	memberID := mkUser(t, q, "tok-member@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatalf("add member: %v", err)
	}

	memberCookie := loginAs(t, q, "tok-member@k.local")
	form := url.Values{"name": {"laptop"}, "level": {"write"}, "expires": {"never"}}
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/orgs/%d/api-tokens", o.ID), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(memberCookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member create want 403, got %d", rec.Code)
	}

	rows, err := q.ListAPITokensByUserAndOrg(ctx, db.ListAPITokensByUserAndOrgParams{UserID: memberID, OrgID: o.ID})
	if err != nil {
		t.Fatalf("list tokens: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("member's blocked create must not have persisted a token, got %d", len(rows))
	}
}

// TestCreateTokenInvalidLevelRejected exercises the level validation path: a
// tampered/unknown level value must be rejected (no token minted), rather
// than silently coerced to read or write.
func TestCreateTokenInvalidLevelRejected(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	cookie, orgID := loginAsOwner(t, h, q, orgSvc)

	form := url.Values{"name": {"laptop"}, "level": {"superuser"}, "expires": {"never"}}
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/orgs/%d/api-tokens", orgID), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("want 303 (PRG back to the form), got %d", rec.Code)
	}
	if !hasErrFlash(rec) {
		t.Fatal("want an error flash for an invalid level")
	}

	rows, err := q.ListAPITokensByUserAndOrg(t.Context(), db.ListAPITokensByUserAndOrgParams{UserID: userIDFrom(t, q), OrgID: orgID})
	if err != nil || len(rows) != 0 {
		t.Fatalf("invalid level must not create a token, got %d (err=%v)", len(rows), err)
	}
}

// TestRevokeTokenRequiresOwnership locks in the IDOR guard: user B must not
// be able to revoke user A's token by guessing/enumerating its id, even
// though both are members of the same org and revocation itself needs no
// admin role.
func TestRevokeTokenRequiresOwnership(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()

	ownerCookie, orgID := loginAsOwner(t, h, q, orgSvc)
	ownerID := userIDFrom(t, q)

	// Owner mints a token.
	form := url.Values{"name": {"laptop"}, "level": {"read"}, "expires": {"never"}}
	createReq := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/orgs/%d/api-tokens", orgID), strings.NewReader(form.Encode()))
	createReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	createReq.AddCookie(ownerCookie)
	createRec := httptest.NewRecorder()
	h.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusSeeOther {
		t.Fatalf("create want 303, got %d", createRec.Code)
	}
	rows, err := q.ListAPITokensByUserAndOrg(ctx, db.ListAPITokensByUserAndOrgParams{UserID: ownerID, OrgID: orgID})
	if err != nil || len(rows) != 1 {
		t.Fatalf("want exactly one token, got %d (err=%v)", len(rows), err)
	}
	tokenID := rows[0].ID

	// A second org member tries to revoke the owner's token.
	outsiderID := mkUser(t, q, "tok-outsider@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: orgID, UserID: outsiderID, Role: "member"}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	outsiderCookie := loginAs(t, q, "tok-outsider@k.local")
	delReq := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/orgs/%d/api-tokens/%d/delete", orgID, tokenID), nil)
	delReq.AddCookie(outsiderCookie)
	delRec := httptest.NewRecorder()
	h.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusNotFound {
		t.Fatalf("outsider delete want 404, got %d", delRec.Code)
	}

	rows, err = q.ListAPITokensByUserAndOrg(ctx, db.ListAPITokensByUserAndOrgParams{UserID: ownerID, OrgID: orgID})
	if err != nil || len(rows) != 1 {
		t.Fatalf("owner's token must survive an outsider's delete attempt, got %d (err=%v)", len(rows), err)
	}
}

// TestRevokeOwnTokenNeedsNoAdmin proves a plain member can revoke a token
// they hold themselves without needing an admin role — the query already
// scopes deletion by user_id, so no extra role gate is required.
func TestRevokeOwnTokenNeedsNoAdmin(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()

	ownerID := mkUser(t, q, "tok-owner2@k.local")
	o, err := orgSvc.CreateOrg(ctx, ownerID, "Tok Org 2")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	memberID := mkUser(t, q, "tok-member2@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	// Seed a token directly for the member (bypassing the admin-gated create
	// route, matching how such a token could exist: minted while the member was
	// still an admin, or issued by an admin acting on their behalf pre-Task-10).
	issueAPIToken(t, q, memberID, o.ID, "read")

	rows, err := q.ListAPITokensByUserAndOrg(ctx, db.ListAPITokensByUserAndOrgParams{UserID: memberID, OrgID: o.ID})
	if err != nil || len(rows) != 1 {
		t.Fatalf("seed: want exactly one token, got %d (err=%v)", len(rows), err)
	}
	tokenID := rows[0].ID

	memberCookie := loginAs(t, q, "tok-member2@k.local")
	delReq := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/orgs/%d/api-tokens/%d/delete", o.ID, tokenID), nil)
	delReq.AddCookie(memberCookie)
	delRec := httptest.NewRecorder()
	h.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusSeeOther {
		t.Fatalf("member revoking their own token want 303, got %d", delRec.Code)
	}

	rows, err = q.ListAPITokensByUserAndOrg(ctx, db.ListAPITokensByUserAndOrgParams{UserID: memberID, OrgID: o.ID})
	if err != nil || len(rows) != 0 {
		t.Fatalf("token must be gone after revoke, got %d (err=%v)", len(rows), err)
	}
}

// TestListAPITokensScopedToOrg pins that the list page obeys the org in its
// own URL: a token is genuinely org-bound (Identity.OrgID comes straight from
// the stored org_id and decides what the token can act on), so a token minted
// under one org must never appear on a different org's /api-tokens page, even
// for the very same user. One user, two orgs they own, one token minted in
// each — each org's page must show exactly its own token and nothing else.
func TestListAPITokensScopedToOrg(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	ctx := context.Background()

	uid := mkUser(t, q, "multi-org@k.local")
	orgA, err := orgSvc.CreateOrg(ctx, uid, "Org A")
	if err != nil {
		t.Fatalf("create org A: %v", err)
	}
	orgB, err := orgSvc.CreateOrg(ctx, uid, "Org B")
	if err != nil {
		t.Fatalf("create org B: %v", err)
	}
	cookie := loginAs(t, q, "multi-org@k.local")

	mint := func(orgID int64, name string) {
		t.Helper()
		form := url.Values{"name": {name}, "level": {"read"}, "expires": {"never"}}
		req := httptest.NewRequest(http.MethodPost,
			fmt.Sprintf("/orgs/%d/api-tokens", orgID), strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("mint %q in org %d: want 303, got %d", name, orgID, rec.Code)
		}
	}
	mint(orgA.ID, "token-in-a")
	mint(orgB.ID, "token-in-b")

	list := func(orgID int64) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/orgs/%d/api-tokens", orgID), nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("list org %d: want 200, got %d", orgID, rec.Code)
		}
		return rec.Body.String()
	}

	bodyA := list(orgA.ID)
	if !strings.Contains(bodyA, "token-in-a") {
		t.Error("org A's page must show its own token")
	}
	if strings.Contains(bodyA, "token-in-b") {
		t.Error("org A's page must not show org B's token")
	}

	bodyB := list(orgB.ID)
	if !strings.Contains(bodyB, "token-in-b") {
		t.Error("org B's page must show its own token")
	}
	if strings.Contains(bodyB, "token-in-a") {
		t.Error("org B's page must not show org A's token")
	}
}

// A double-submitted form used to mint two tokens and show only one of them.
func TestCreateTokenRefusesADuplicateName(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	cookie, orgID := loginAsOwner(t, h, q, orgSvc)
	form := url.Values{"name": {"ci"}, "level": {"read"}, "expires": {"never"}}
	for i := 0; i < 2; i++ {
		rec := postForm(t, h, fmt.Sprintf("/orgs/%d/api-tokens", orgID), cookie, form)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("create #%d: want 303, got %d", i+1, rec.Code)
		}
		if i == 1 && !hasErrFlash(rec) {
			t.Fatal("a second token with the same name must be refused")
		}
	}
	toks, err := q.ListAPITokensByUserAndOrg(context.Background(), db.ListAPITokensByUserAndOrgParams{UserID: userIDFrom(t, q), OrgID: orgID})
	if err != nil || len(toks) != 1 {
		t.Fatalf("want one token, got %d (%v)", len(toks), err)
	}
}
