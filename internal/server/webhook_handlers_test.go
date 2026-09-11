package server_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/org"
)

func ghSign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// seedAutoDeploy seeds an app with a known secret + auto_deploy=true.
func seedAutoDeploy(t *testing.T, q *db.Queries, appID int64, secret string) {
	t.Helper()
	ctx := context.Background()
	if err := q.SetApplicationWebhookSecret(ctx, db.SetApplicationWebhookSecretParams{ID: appID, WebhookSecret: secret}); err != nil {
		t.Fatalf("set secret: %v", err)
	}
	if err := q.SetApplicationAutoDeploy(ctx, db.SetApplicationAutoDeployParams{ID: appID, AutoDeploy: true}); err != nil {
		t.Fatalf("enable: %v", err)
	}
}

func postWebhook(t *testing.T, h http.Handler, path, event string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	if event != "" {
		req.Header.Set("X-GitHub-Event", event)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func imageAppFixture(t *testing.T, q *db.Queries, orgSvc *org.Service) int64 {
	t.Helper()
	ctx := context.Background()
	email := "wh-img@k.local"
	o, err := orgSvc.CreateOrg(ctx, mkUser(t, q, email), "Org-"+email)
	if err != nil {
		t.Fatalf("create org for image app: %v", err)
	}
	p, err := orgSvc.CreateProject(ctx, o.ID, "P", "")
	if err != nil {
		t.Fatalf("create project for image app: %v", err)
	}
	e, err := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	if err != nil {
		t.Fatalf("create env for image app: %v", err)
	}
	a, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "img", Image: "nginx", Tag: "alpine",
		Domain: "wh-img.example.com", Port: 80, SourceType: "image",
		GitUrl: "", GitBranch: "", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create image app: %v", err)
	}
	return a.ID
}

func dockerfileAppFixture(t *testing.T, q *db.Queries, orgSvc *org.Service, branch string) int64 {
	t.Helper()
	ctx := context.Background()
	email := "wh-df-" + branch + "@k.local"
	o, err := orgSvc.CreateOrg(ctx, mkUser(t, q, email), "Org-"+email)
	if err != nil {
		t.Fatalf("create org for dockerfile app: %v", err)
	}
	p, err := orgSvc.CreateProject(ctx, o.ID, "P", "")
	if err != nil {
		t.Fatalf("create project for dockerfile app: %v", err)
	}
	e, err := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	if err != nil {
		t.Fatalf("create env for dockerfile app: %v", err)
	}
	a, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "df", Image: "", Tag: "",
		Domain: "wh-df-" + branch + ".example.com", Port: 80, SourceType: "dockerfile",
		GitUrl: "https://github.com/x/y", GitBranch: branch, DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatalf("create dockerfile app: %v", err)
	}
	return a.ID
}

func TestGithubWebhook(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	// dockerfile app on branch main
	appID := dockerfileAppFixture(t, q, orgSvc, "main")
	const sec = "topsecret"
	seedAutoDeploy(t, q, appID, sec)
	path := "/webhooks/github/" + i64(appID)
	body := []byte(`{"ref":"refs/heads/main","deleted":false}`)

	// valid signature + matching branch → 202 + deployment row trigger=webhook
	rec := postWebhook(t, h, path, "push", body, map[string]string{"X-Hub-Signature-256": ghSign(sec, body)})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("push: want 202, got %d (%s)", rec.Code, rec.Body.String())
	}
	deps, _ := q.ListDeploymentsByApplication(context.Background(), appID)
	if len(deps) != 1 || deps[0].Trigger != "webhook" {
		t.Fatalf("want 1 webhook deployment, got %+v", deps)
	}

	// bad signature → 404 (indistinguishable from unknown/disabled app), no new deploy
	rec = postWebhook(t, h, path, "push", body, map[string]string{"X-Hub-Signature-256": "sha256=bad"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bad sig: want 404, got %d", rec.Code)
	}

	// non-matching branch → 200, no new deploy
	other := []byte(`{"ref":"refs/heads/dev"}`)
	rec = postWebhook(t, h, path, "push", other, map[string]string{"X-Hub-Signature-256": ghSign(sec, other)})
	if rec.Code != http.StatusOK {
		t.Fatalf("other branch: want 200, got %d", rec.Code)
	}

	// ping → 200
	rec = postWebhook(t, h, path, "ping", body, map[string]string{"X-Hub-Signature-256": ghSign(sec, body)})
	if rec.Code != http.StatusOK {
		t.Fatalf("ping: want 200, got %d", rec.Code)
	}

	deps, _ = q.ListDeploymentsByApplication(context.Background(), appID)
	if len(deps) != 1 {
		t.Fatalf("only the first push should have deployed, got %d", len(deps))
	}
}

// TestGithubWebhookEmptyBranchSkipsTagPush guards against a regression where a
// blank configured git_branch (e.g. cleared via the edit form) would match a
// tag push's empty Branch() and wrongly enqueue a deploy: "" == "" if the
// skip condition only compared ev.Branch() != a.GitBranch.
func TestGithubWebhookEmptyBranchSkipsTagPush(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	appID := dockerfileAppFixture(t, q, orgSvc, "")
	const sec = "topsecret"
	seedAutoDeploy(t, q, appID, sec)
	path := "/webhooks/github/" + i64(appID)
	// A tag push: Branch() returns "" for any non-refs/heads/* ref.
	body := []byte(`{"ref":"refs/tags/v1.0.0","deleted":false}`)

	rec := postWebhook(t, h, path, "push", body, map[string]string{"X-Hub-Signature-256": ghSign(sec, body)})
	if rec.Code != http.StatusOK {
		t.Fatalf("empty branch + tag push: want 200 (skip), got %d (%s)", rec.Code, rec.Body.String())
	}
	deps, _ := q.ListDeploymentsByApplication(context.Background(), appID)
	if len(deps) != 0 {
		t.Fatalf("empty branch + tag push must not enqueue a deploy, got %d deployments", len(deps))
	}
}

func TestGithubWebhookDisabledAndWrongSource(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	// disabled app
	off := dockerfileAppFixture(t, q, orgSvc, "main")
	body := []byte(`{"ref":"refs/heads/main"}`)
	rec := postWebhook(t, h, "/webhooks/github/"+i64(off), "push", body,
		map[string]string{"X-Hub-Signature-256": ghSign("x", body)})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("disabled: want 404, got %d", rec.Code)
	}
	// image app on the github endpoint → 404
	img := imageAppFixture(t, q, orgSvc)
	seedAutoDeploy(t, q, img, "s")
	rec = postWebhook(t, h, "/webhooks/github/"+i64(img), "push", body,
		map[string]string{"X-Hub-Signature-256": ghSign("s", body)})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("image on github endpoint: want 404, got %d", rec.Code)
	}
}

func TestDeployHook(t *testing.T) {
	h, q, orgSvc, _ := newDeployServer(t)
	appID := imageAppFixture(t, q, orgSvc)
	const sec = "tok123"
	seedAutoDeploy(t, q, appID, sec)
	path := "/webhooks/deploy/" + i64(appID)
	bearer := func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

	// valid token → 202 + deployment
	rec := postWebhook(t, h, path, "", nil, bearer(sec))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("deploy hook: want 202, got %d (%s)", rec.Code, rec.Body.String())
	}
	deps, _ := q.ListDeploymentsByApplication(context.Background(), appID)
	if len(deps) != 1 || deps[0].Trigger != "webhook" {
		t.Fatalf("want 1 webhook deployment, got %+v", deps)
	}

	// bad token → 404 (indistinguishable from unknown/disabled app)
	rec = postWebhook(t, h, path, "", nil, bearer("nope"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bad token: want 404, got %d", rec.Code)
	}

	// the correct secret in ?token= is ignored → 404: query strings land in
	// proxy logs and browser history, so the header is the only accepted form
	rec = postWebhook(t, h, path+"?token="+sec, "", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("?token= must not authenticate: want 404, got %d", rec.Code)
	}
	if deps, _ := q.ListDeploymentsByApplication(context.Background(), appID); len(deps) != 1 {
		t.Fatalf("?token= must not enqueue a deployment, got %d deployments", len(deps))
	}

	// ?tag=v2 updates the app tag
	rec = postWebhook(t, h, path+"?tag=v2", "", nil, bearer(sec))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("tag update: want 202, got %d", rec.Code)
	}
	a, _ := q.GetApplication(context.Background(), appID)
	if a.Tag != "v2" {
		t.Fatalf("tag = %q, want v2", a.Tag)
	}

	// dockerfile app on the deploy endpoint → 404
	df := dockerfileAppFixture(t, q, orgSvc, "main")
	seedAutoDeploy(t, q, df, "x")
	rec = postWebhook(t, h, "/webhooks/deploy/"+i64(df), "", nil, bearer("x"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("dockerfile on deploy endpoint: want 404, got %d", rec.Code)
	}
}
