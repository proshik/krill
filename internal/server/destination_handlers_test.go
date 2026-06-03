package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/backup"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/testutil"
)

// createDestinationForm POSTs a destination create form and returns the recorder.
func createDestinationForm(t *testing.T, h http.Handler, cookie *http.Cookie, orgID int64, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	target := "/orgs/" + i64(orgID) + "/destinations"
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCreateDestinationSucceeds(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	cookie := loginAs(t, q, "owner@k.local")

	minio := testutil.NewMinio(t)
	dst := backup.Destination{
		Endpoint:  minio.Endpoint,
		Bucket:    "test",
		Region:    minio.Region,
		AccessKey: minio.AccessKey,
		SecretKey: minio.SecretKey,
	}
	if err := backup.CreateBucket(ctx, dst); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	form := url.Values{
		"name":       {"primary"},
		"endpoint":   {minio.Endpoint},
		"bucket":     {"test"},
		"region":     {minio.Region},
		"access_key": {minio.AccessKey},
		"secret_key": {minio.SecretKey},
	}
	rec := createDestinationForm(t, h, cookie, o.ID, form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create destination want 303, got %d (%s)", rec.Code, rec.Body.String())
	}

	dests, err := q.ListDestinationsByOrg(ctx, o.ID)
	if err != nil {
		t.Fatalf("list destinations: %v", err)
	}
	if len(dests) != 1 || dests[0].Name != "primary" || dests[0].Bucket != "test" {
		t.Fatalf("expected 1 destination 'primary', got %+v", dests)
	}
}

func TestCreateDestinationDuplicateName400(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	cookie := loginAs(t, q, "owner@k.local")

	minio := testutil.NewMinio(t)
	dst := backup.Destination{
		Endpoint:  minio.Endpoint,
		Bucket:    "test",
		Region:    minio.Region,
		AccessKey: minio.AccessKey,
		SecretKey: minio.SecretKey,
	}
	if err := backup.CreateBucket(ctx, dst); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	form := url.Values{
		"name":       {"primary"},
		"endpoint":   {minio.Endpoint},
		"bucket":     {"test"},
		"region":     {minio.Region},
		"access_key": {minio.AccessKey},
		"secret_key": {minio.SecretKey},
	}
	if rec := createDestinationForm(t, h, cookie, o.ID, form); rec.Code != http.StatusSeeOther {
		t.Fatalf("first create want 303, got %d (%s)", rec.Code, rec.Body.String())
	}

	// Same name again → 400.
	rec := createDestinationForm(t, h, cookie, o.ID, form)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate name want 400, got %d", rec.Code)
	}

	dests, _ := q.ListDestinationsByOrg(ctx, o.ID)
	if len(dests) != 1 {
		t.Fatalf("expected 1 destination after duplicate, got %d", len(dests))
	}
}

func TestMemberCannotCreateDestination(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	memberID := mkUser(t, q, "member@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	cookie := loginAs(t, q, "member@k.local")

	form := url.Values{
		"name":       {"primary"},
		"bucket":     {"test"},
		"region":     {"us-east-1"},
		"access_key": {"k"},
		"secret_key": {"s"},
	}
	rec := createDestinationForm(t, h, cookie, o.ID, form)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member create destination want 403, got %d", rec.Code)
	}

	dests, _ := q.ListDestinationsByOrg(ctx, o.ID)
	if len(dests) != 0 {
		t.Fatalf("expected no destinations, got %d", len(dests))
	}
}

func TestDeleteUnreferencedDestination(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	ownerID := mkUser(t, q, "owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, ownerID, "Org")
	cookie := loginAs(t, q, "owner@k.local")

	d, err := q.CreateDestination(ctx, db.CreateDestinationParams{
		OrganizationID: o.ID,
		Name:           "primary",
		Endpoint:       "http://minio:9000",
		Bucket:         "test",
		Region:         "us-east-1",
		AccessKey:      "k",
		SecretKey:      "s",
	})
	if err != nil {
		t.Fatalf("create destination: %v", err)
	}

	target := "/orgs/" + i64(o.ID) + "/destinations/" + i64(d.ID) + "/delete"
	req := httptest.NewRequest(http.MethodPost, target, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete destination want 303, got %d (%s)", rec.Code, rec.Body.String())
	}

	if _, err := q.GetDestination(ctx, d.ID); err == nil {
		t.Fatalf("destination should be deleted")
	}
}

func TestDeleteDestinationCrossTenant404(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()

	ownerAID := mkUser(t, q, "ownera@k.local")
	orgA, _ := orgSvc.CreateOrg(ctx, ownerAID, "OrgA")
	dA, err := q.CreateDestination(ctx, db.CreateDestinationParams{
		OrganizationID: orgA.ID, Name: "a", Endpoint: "http://m:9000",
		Bucket: "b", Region: "us-east-1", AccessKey: "k", SecretKey: "s",
	})
	if err != nil {
		t.Fatalf("create destination A: %v", err)
	}

	ownerBID := mkUser(t, q, "ownerb@k.local")
	orgB, _ := orgSvc.CreateOrg(ctx, ownerBID, "OrgB")
	cookieB := loginAs(t, q, "ownerb@k.local")

	// Org-B owner tries to delete org-A's destination via org-B's path.
	target := "/orgs/" + i64(orgB.ID) + "/destinations/" + i64(dA.ID) + "/delete"
	req := httptest.NewRequest(http.MethodPost, target, nil)
	req.AddCookie(cookieB)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant delete want 404, got %d", rec.Code)
	}
	if _, err := q.GetDestination(ctx, dA.ID); err != nil {
		t.Fatalf("org-A destination must still exist: %v", err)
	}
}
