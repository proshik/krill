package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/oplock"
	"github.com/proshik/krill/internal/secret"
)

type rollbackHandlerEngine struct{ noopEngine }

func (rollbackHandlerEngine) ResolveDigest(context.Context, string, string) (string, error) {
	return "nginx@sha256:" + strings.Repeat("a", 64), nil
}
func (rollbackHandlerEngine) ValidateRelease(context.Context, docker.ServiceSpec, []string) error {
	return nil
}

func TestRollbackHandlerAuthorizationAndHistory(t *testing.T) {
	secret.Init("handler-release-key")
	defer secret.Init("")
	h, q, orgSvc, _, dep := buildDeployServer(t, nil, rollbackHandlerEngine{})
	base, cookie, appID := domainFixture(t, h, q, orgSvc, "rollback-handler.test")
	original := dep.Enqueue(appID, "manual")
	deadline := time.Now().Add(10 * time.Second)
	for {
		row, _ := q.GetDeployment(t.Context(), original)
		if row.Status == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("deployment did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	path := base + "/deployments/" + i64(original) + "/rollback"
	member := mkUser(t, q, "rollback-member@k.local")
	chain, _ := q.GetApplicationChain(t.Context(), appID)
	if _, err := q.CreateMember(t.Context(), db.CreateMemberParams{OrganizationID: chain.OrgID, UserID: member, Role: "member"}); err != nil {
		t.Fatal(err)
	}
	if rec := postForm(t, h, path, loginAs(t, q, "rollback-member@k.local"), url.Values{}); rec.Code != http.StatusForbidden {
		t.Fatalf("member got %d", rec.Code)
	}
	// A deployment must belong to the complete app chain, not only the organization.
	app, _ := q.GetApplication(t.Context(), appID)
	other, err := q.CreateApplication(t.Context(), db.CreateApplicationParams{EnvironmentID: app.EnvironmentID, Name: "other", Domain: "other-rollback.test", SourceType: "image", Image: "nginx", Tag: "latest", Port: 80})
	if err != nil {
		t.Fatal(err)
	}
	otherPath := strings.TrimSuffix(base, "/"+i64(appID)) + "/" + i64(other.ID) + "/deployments/" + i64(original) + "/rollback"
	if rec := postForm(t, h, otherPath, cookie, url.Values{}); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign app got %d", rec.Code)
	}
	foreignOwner := mkUser(t, q, "foreign-release-owner@k.local")
	foreignOrg, err := orgSvc.CreateOrg(t.Context(), foreignOwner, "Foreign release org")
	if err != nil {
		t.Fatal(err)
	}
	foreignPath := strings.Replace(path, "/orgs/"+i64(chain.OrgID)+"/", "/orgs/"+i64(foreignOrg.ID)+"/", 1)
	if rec := postForm(t, h, foreignPath, loginAs(t, q, "foreign-release-owner@k.local"), url.Values{}); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-organization rollback got %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, base+"?tab=deployments", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), path) {
		t.Fatal("eligible rollback action missing")
	}
	req = httptest.NewRequest(http.MethodGet, base+"?tab=deployments", nil)
	req.AddCookie(loginAs(t, q, "rollback-member@k.local"))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), path) {
		t.Fatal("member received rollback action")
	}
	rec = postForm(t, h, path, cookie, url.Values{})
	if rec.Code != http.StatusSeeOther || hasErrFlash(rec) {
		t.Fatalf("owner rollback refused: %d %s", rec.Code, flashText(rec))
	}
}

type routingRaceEngine struct {
	rollbackHandlerEngine
	mu                          sync.Mutex
	validationCalls             int
	blockValidation, blockApply bool
	entered, proceed            chan struct{}
	labels                      map[string]string
}

func (e *routingRaceEngine) ValidateRelease(ctx context.Context, _ docker.ServiceSpec, _ []string) error {
	e.mu.Lock()
	e.validationCalls++
	block := e.blockValidation && e.validationCalls == 2
	e.mu.Unlock()
	if block {
		close(e.entered)
		select {
		case <-e.proceed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func (e *routingRaceEngine) ServiceDeploy(ctx context.Context, s docker.ServiceSpec) error {
	e.mu.Lock()
	block := e.blockApply
	e.blockApply = false
	e.mu.Unlock()
	if block {
		close(e.entered)
		select {
		case <-e.proceed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	e.mu.Lock()
	e.labels = s.Labels
	e.mu.Unlock()
	return nil
}
func (e *routingRaceEngine) ServiceUpdateLabels(_ context.Context, _ string, labels map[string]string) error {
	e.mu.Lock()
	e.labels = labels
	e.mu.Unlock()
	return nil
}
func TestRollbackKeepsConcurrentDomainProtection(t *testing.T) {
	for _, stage := range []string{"validation", "apply"} {
		t.Run(stage, func(t *testing.T) {
			secret.Init("routing-release-key")
			defer secret.Init("")
			e := &routingRaceEngine{}
			h, q, orgSvc, _, dep := buildDeployServer(t, nil, e)
			base, cookie, appID := domainFixture(t, h, q, orgSvc, "routing-race.test")
			wait := func(id int64) {
				t.Helper()
				deadline := time.Now().Add(10 * time.Second)
				for {
					row, err := q.GetDeployment(t.Context(), id)
					if err != nil {
						t.Fatal(err)
					}
					if row.Status == "done" {
						return
					}
					if row.Status == "error" || time.Now().After(deadline) {
						t.Fatalf("deployment %d: %s %s", id, row.Status, row.Log)
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			source := dep.Enqueue(appID, "manual")
			wait(source)
			doms, err := q.ListDomainsByApplication(t.Context(), appID)
			if err != nil || len(doms) != 1 {
				t.Fatalf("domains: %v", err)
			}
			e.mu.Lock()
			e.entered = make(chan struct{})
			e.proceed = make(chan struct{})
			e.blockValidation = stage == "validation"
			e.blockApply = stage == "apply"
			e.mu.Unlock()
			rollback, err := dep.EnqueueRollback(t.Context(), appID, source)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-e.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("rollback did not reach barrier")
			}
			var release sync.Once
			defer release.Do(func() { close(e.proceed) })
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				done <- postForm(t, h, base+"/domains/"+i64(doms[0].ID)+"/allowed-ips", cookie, url.Values{"ips": {"10.0.0.0/8"}})
			}()
			if stage == "validation" {
				select {
				case rec := <-done:
					if rec.Code != 303 || hasErrFlash(rec) {
						t.Fatalf("protection update: %d", rec.Code)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("handler blocked during validation")
				}
				release.Do(func() { close(e.proceed) })
			} else {
				// The DB write happens before label sync; keep spec apply blocked until the
				// handler has saved its protection and is waiting to synchronize the service.
				deadline := time.Now().Add(5 * time.Second)
				for {
					dom, _ := q.GetDomain(t.Context(), doms[0].ID)
					if dom.AllowedIps != "" {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("protection not saved")
					}
					time.Sleep(time.Millisecond)
				}
				select {
				case <-done:
					t.Fatal("handler returned before in-flight spec apply finished")
				default:
				}
				release.Do(func() { close(e.proceed) })
				select {
				case rec := <-done:
					if rec.Code != 303 || hasErrFlash(rec) {
						t.Fatalf("protection update: %d", rec.Code)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("handler never completed")
				}
			}
			wait(rollback)
			e.mu.Lock()
			defer e.mu.Unlock()
			found := false
			for k, v := range e.labels {
				if strings.HasSuffix(k, ".ipallowlist.sourcerange") && v == "10.0.0.0/8" {
					found = true
				}
			}
			if !found {
				t.Fatalf("successful protection update lost: %v", e.labels)
			}
		})
	}
}

func TestCanceledProtectionSyncDoesNotReportSuccess(t *testing.T) {
	e := &labelEngine{}
	h, q, orgSvc := newServerWithLabelEngine(t, e)
	base, cookie, appID := domainFixture(t, h, q, orgSvc, "routing-cancel.test")
	doms, err := q.ListDomainsByApplication(t.Context(), appID)
	if err != nil || len(doms) != 1 {
		t.Fatal("missing domain")
	}
	lock := oplock.AppRouting(appID)
	if !oplock.TryAcquire(lock) {
		t.Fatal("routing lock busy")
	}
	defer oplock.Release(lock)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, base+"/domains/"+i64(doms[0].ID)+"/allowed-ips", strings.NewReader(url.Values{"ips": {"10.0.0.0/8"}}.Encode())).WithContext(ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(rec, req); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		dom, err := q.GetDomain(t.Context(), doms[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if dom.AllowedIps != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("protection not saved")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled handler remains blocked")
	}
	if !hasErrFlash(rec) {
		t.Fatal("canceled routing sync reported success")
	}
	if !oplock.Held(lock) {
		t.Fatal("canceled waiter released deploy's lock")
	}
}
