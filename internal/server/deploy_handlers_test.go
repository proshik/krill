package server_test

import (
	"context"
	"net/http"
	"net/url"
	"testing"
)

// TestDeployAppEnqueues: POST /deploy redirects 303 and creates a deployment row.
func TestDeployAppEnqueues(t *testing.T) {
	h, q, orgSvc := newDeployServer(t)
	base, cookie, appID := domainFixture(t, h, q, orgSvc, "deploy1.example.com")

	rec := postForm(t, h, base+"/deploy", cookie, url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("deploy want 303, got %d: %s", rec.Code, rec.Body.String())
	}
	deps, err := q.ListDeploymentsByApplication(context.Background(), appID)
	if err != nil {
		t.Fatalf("list deployments: %v", err)
	}
	if len(deps) == 0 {
		t.Fatal("expected a deployment row after deploy")
	}
}

// TestRebuildAppEnqueues: POST /rebuild redirects 303 and creates a deployment.
func TestRebuildAppEnqueues(t *testing.T) {
	h, q, orgSvc := newDeployServer(t)
	base, cookie, appID := domainFixture(t, h, q, orgSvc, "deploy2.example.com")

	rec := postForm(t, h, base+"/rebuild", cookie, url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("rebuild want 303, got %d: %s", rec.Code, rec.Body.String())
	}
	deps, err := q.ListDeploymentsByApplication(context.Background(), appID)
	if err != nil {
		t.Fatalf("list deployments: %v", err)
	}
	if len(deps) == 0 {
		t.Fatal("expected a deployment row after rebuild")
	}
}

// TestReloadAppRedirects: POST /reload redirects 303 (restart, no new deployment).
func TestReloadAppRedirects(t *testing.T) {
	h, q, orgSvc := newDeployServer(t)
	base, cookie, _ := domainFixture(t, h, q, orgSvc, "deploy3.example.com")

	rec := postForm(t, h, base+"/reload", cookie, url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("reload want 303, got %d: %s", rec.Code, rec.Body.String())
	}
}
