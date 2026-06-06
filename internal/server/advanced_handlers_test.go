package server_test

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestSaveAdvancedInvalidMemoryFlash verifies that an invalid memory limit is
// rejected with an err flash + 303 and leaves the app row unchanged.
func TestSaveAdvancedInvalidMemoryFlash(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, appID := domainFixture(t, h, q, orgSvc, "adv-mem.example.com")

	rec := postForm(t, h, base+"/advanced", cookie, url.Values{
		"memory_limit":      {"abc"},
		"replicas":          {"1"},
		"restart_condition": {"any"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("invalid memory want 303, got %d body: %s", rec.Code, rec.Body.String())
	}
	if !hasErrFlash(rec) {
		t.Fatalf("invalid memory want err flash, got %q", flashCookieValue(rec))
	}

	a, err := q.GetApplication(ctx, appID)
	if err != nil {
		t.Fatalf("GetApplication: %v", err)
	}
	if a.MemoryLimit != nil {
		t.Errorf("MemoryLimit want nil (unchanged), got %q", *a.MemoryLimit)
	}
}

// TestSaveAdvancedValid verifies that a valid form persists all advanced fields.
func TestSaveAdvancedValid(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, appID := domainFixture(t, h, q, orgSvc, "adv-ok.example.com")

	rec := postForm(t, h, base+"/advanced", cookie, url.Values{
		"command":              {"start-dev"},
		"replicas":             {"2"},
		"memory_limit":         {"256m"},
		"cpu_limit":            {"0.5"},
		"restart_condition":    {"on-failure"},
		"restart_max_attempts": {"3"},
		"healthcheck_cmd":      {"curl -f http://localhost/ || exit 1"},
		"healthcheck_interval": {"30s"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("valid advanced want 303, got %d body: %s", rec.Code, rec.Body.String())
	}

	a, err := q.GetApplication(ctx, appID)
	if err != nil {
		t.Fatalf("GetApplication: %v", err)
	}
	if a.Command == nil || *a.Command != "start-dev" {
		t.Errorf("Command want %q, got %v", "start-dev", a.Command)
	}
	if a.MemoryLimit == nil || *a.MemoryLimit != "256m" {
		t.Errorf("MemoryLimit want %q, got %v", "256m", a.MemoryLimit)
	}
	if a.CpuLimit == nil || *a.CpuLimit != "0.5" {
		t.Errorf("CpuLimit want %q, got %v", "0.5", a.CpuLimit)
	}
	if a.Replicas != 2 {
		t.Errorf("Replicas want 2, got %d", a.Replicas)
	}
	if a.RestartCondition != "on-failure" {
		t.Errorf("RestartCondition want %q, got %q", "on-failure", a.RestartCondition)
	}
	if a.RestartMaxAttempts != 3 {
		t.Errorf("RestartMaxAttempts want 3, got %d", a.RestartMaxAttempts)
	}
	if a.HealthcheckCmd == nil || !strings.Contains(*a.HealthcheckCmd, "curl") {
		t.Errorf("HealthcheckCmd want to contain %q, got %v", "curl", a.HealthcheckCmd)
	}
	if a.HealthcheckInterval == nil || *a.HealthcheckInterval != "30s" {
		t.Errorf("HealthcheckInterval want %q, got %v", "30s", a.HealthcheckInterval)
	}
}
