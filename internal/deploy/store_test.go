package deploy

import (
	"testing"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
)

func sp(s string) *string { return &s }
func ip(v int32) *int32   { return &v }

func TestBuildHealthcheckNil(t *testing.T) {
	// No command at all => healthcheck disabled.
	if hc := buildHealthcheck(db.Application{}); hc != nil {
		t.Fatalf("buildHealthcheck(empty) = %+v, want nil", hc)
	}
	// Whitespace-only command also counts as empty.
	if hc := buildHealthcheck(db.Application{HealthcheckCmd: sp("   ")}); hc != nil {
		t.Fatalf("buildHealthcheck(blank cmd) = %+v, want nil", hc)
	}
}

func TestBuildHealthcheckFull(t *testing.T) {
	a := db.Application{
		HealthcheckCmd:         sp("curl -f http://localhost/ || exit 1"),
		HealthcheckInterval:    sp("30s"),
		HealthcheckTimeout:     sp("5s"),
		HealthcheckStartPeriod: sp("10s"),
		HealthcheckRetries:     ip(3),
	}
	hc := buildHealthcheck(a)
	if hc == nil {
		t.Fatal("buildHealthcheck returned nil for configured healthcheck")
	}
	wantTest := []string{"CMD-SHELL", "curl -f http://localhost/ || exit 1"}
	if len(hc.Test) != 2 || hc.Test[0] != wantTest[0] || hc.Test[1] != wantTest[1] {
		t.Errorf("Test = %v, want %v", hc.Test, wantTest)
	}
	if hc.Interval != 30*time.Second {
		t.Errorf("Interval = %v, want 30s", hc.Interval)
	}
	if hc.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want 5s", hc.Timeout)
	}
	if hc.StartPeriod != 10*time.Second {
		t.Errorf("StartPeriod = %v, want 10s", hc.StartPeriod)
	}
	if hc.Retries != 3 {
		t.Errorf("Retries = %d, want 3", hc.Retries)
	}
}

func TestBuildHealthcheckInvalidDurations(t *testing.T) {
	// Command set but durations invalid/empty/retries nil: zero values remain.
	a := db.Application{
		HealthcheckCmd:         sp("true"),
		HealthcheckInterval:    sp("not-a-duration"),
		HealthcheckTimeout:     sp(""),
		HealthcheckStartPeriod: nil,
		HealthcheckRetries:     nil,
	}
	hc := buildHealthcheck(a)
	if hc == nil {
		t.Fatal("buildHealthcheck returned nil for configured healthcheck")
	}
	if hc.Interval != 0 {
		t.Errorf("Interval = %v, want 0 (invalid duration)", hc.Interval)
	}
	if hc.Timeout != 0 {
		t.Errorf("Timeout = %v, want 0 (empty duration)", hc.Timeout)
	}
	if hc.StartPeriod != 0 {
		t.Errorf("StartPeriod = %v, want 0 (nil)", hc.StartPeriod)
	}
	if hc.Retries != 0 {
		t.Errorf("Retries = %d, want 0 (nil)", hc.Retries)
	}
}

func TestStrDeref(t *testing.T) {
	if got := strDeref(nil); got != "" {
		t.Errorf("strDeref(nil) = %q, want \"\"", got)
	}
	if got := strDeref(sp("x")); got != "x" {
		t.Errorf("strDeref(\"x\") = %q, want \"x\"", got)
	}
}
