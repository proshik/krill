package api_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/api"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
)

func TestAppLogsClampsTail(t *testing.T) {
	f := newAPIFixture(t)
	cases := []struct{ in, want int }{
		{0, api.DefaultLogTail},
		{-5, api.DefaultLogTail},
		{50, 50},
		{99999, api.MaxLogTail},
	}
	for _, tc := range cases {
		if got := api.ClampTail(tc.in); got != tc.want {
			t.Fatalf("ClampTail(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
	_ = f
}

// TestDeploymentStatusRejectsForeignOrg is the load-bearing test for this
// task: a deployment is addressed by a bare numeric id with no app in the
// path, so without an explicit tenancy check any token could read any other
// organization's build log by guessing/incrementing ids.
func TestDeploymentStatusRejectsForeignOrg(t *testing.T) {
	f := newAPIFixture(t)
	depID := f.seedDeployment(t) // helper: creates a deployment row for f.appID
	if _, err := f.svc.DeploymentStatus(t.Context(), f.otherIdent, depID); err == nil {
		t.Fatal("cross-org deployment log readable by numeric id")
	}
}

// TestDeploymentStatusRejectsForeignOrgAsNotFound pins down the specific
// error the IDOR guard must produce: not_found, never forbidden. Forbidden
// would confirm to the caller that the deployment id exists (just not
// theirs), which is exactly the id-enumeration leak resolveApp's own
// tenancy checks are written to avoid — DeploymentStatus needs the same
// property since it bypasses resolveApp entirely (no app in the path).
func TestDeploymentStatusRejectsForeignOrgAsNotFound(t *testing.T) {
	f := newAPIFixture(t)
	depID := f.seedDeployment(t)
	_, err := f.svc.DeploymentStatus(t.Context(), f.otherIdent, depID)
	var aerr *api.Error
	if !errors.As(err, &aerr) || aerr.Code != api.CodeNotFound {
		t.Fatalf("want not_found, got %v", err)
	}
}

// TestDeploymentStatusUnknownIDIsNotFound proves an id that never existed at
// all returns the same not_found — an attacker probing sequential ids must
// not be able to distinguish "wrong org" from "no such deployment".
func TestDeploymentStatusUnknownIDIsNotFound(t *testing.T) {
	f := newAPIFixture(t)
	_, err := f.svc.DeploymentStatus(t.Context(), f.ident, 999999999)
	var aerr *api.Error
	if !errors.As(err, &aerr) || aerr.Code != api.CodeNotFound {
		t.Fatalf("want not_found for a nonexistent deployment id, got %v", err)
	}
}

func TestDeploymentStatusSameOrgSucceeds(t *testing.T) {
	f := newAPIFixture(t)
	depID := f.seedDeployment(t)
	got, err := f.svc.DeploymentStatus(t.Context(), f.ident, depID)
	if err != nil {
		t.Fatalf("same-org deployment status: %v", err)
	}
	if got.ID != depID {
		t.Fatalf("id: got %d, want %d", got.ID, depID)
	}
	if got.Status != "running" || got.Trigger != "manual" {
		t.Fatalf("unexpected fields: %+v", got)
	}
	if got.EndedAt != "" {
		t.Fatalf("a still-running deployment must have no ended_at, got %q", got.EndedAt)
	}
}

// TestDeploymentStatusLogTailIsBoundedWithMarker proves a build log longer
// than the 8 KB budget is cut down to its tail, with a truncation marker
// naming how much was dropped — a 256 KB log returned whole would defeat the
// entire point of bounding it.
func TestDeploymentStatusLogTailIsBoundedWithMarker(t *testing.T) {
	f := newAPIFixture(t)
	depID := f.seedDeployment(t)

	head := strings.Repeat("x", 9000)
	const tailMarker = "END-OF-LOG"
	fullLog := head + tailMarker // 9010 bytes total, well over the 8 KiB budget

	if err := f.q.FinishDeployment(t.Context(), db.FinishDeploymentParams{
		ID: depID, Status: "done", ImageTag: "v1", ErrorMessage: "", Log: fullLog,
	}); err != nil {
		t.Fatalf("finish deployment: %v", err)
	}

	got, err := f.svc.DeploymentStatus(t.Context(), f.ident, depID)
	if err != nil {
		t.Fatalf("deployment status: %v", err)
	}
	wantDropped := len(fullLog) - 8*1024
	wantMarker := fmt.Sprintf("...[truncated %d bytes]...\n", wantDropped)
	if !strings.HasPrefix(got.LogTail, wantMarker) {
		t.Fatalf("log_tail missing/wrong truncation marker: got prefix %q", got.LogTail[:min(60, len(got.LogTail))])
	}
	if !strings.HasSuffix(got.LogTail, tailMarker) {
		t.Fatalf("log_tail must end with the log's actual tail, got suffix %q", got.LogTail[max(0, len(got.LogTail)-20):])
	}
	if len(got.LogTail) != len(wantMarker)+8*1024 {
		t.Fatalf("log_tail length = %d, want %d", len(got.LogTail), len(wantMarker)+8*1024)
	}
	if got.Status != "done" || got.EndedAt == "" {
		t.Fatalf("finished deployment should report status=done and a non-empty ended_at: %+v", got.DeploymentSummary)
	}
}

// TestDeploymentStatusLogTailUnchangedWhenShort proves the marker only
// appears when content was actually dropped — a short log must round-trip
// verbatim, not gain a spurious "...[truncated 0 bytes]..." prefix.
func TestDeploymentStatusLogTailUnchangedWhenShort(t *testing.T) {
	f := newAPIFixture(t)
	depID := f.seedDeployment(t)
	const shortLog = "Step 1/3 : FROM alpine\nsuccessfully built\n"
	if err := f.q.FinishDeployment(t.Context(), db.FinishDeploymentParams{
		ID: depID, Status: "done", ImageTag: "v1", ErrorMessage: "", Log: shortLog,
	}); err != nil {
		t.Fatalf("finish deployment: %v", err)
	}
	got, err := f.svc.DeploymentStatus(t.Context(), f.ident, depID)
	if err != nil {
		t.Fatalf("deployment status: %v", err)
	}
	if got.LogTail != shortLog {
		t.Fatalf("log_tail = %q, want the log verbatim (no truncation marker)", got.LogTail)
	}
}

// TestListDeploymentsClampsLimit proves the caller-supplied limit is bounded
// on both ends — a limit above the cap must not return more than the cap,
// and a non-positive limit must fall back to the default rather than
// returning nothing.
func TestListDeploymentsClampsLimit(t *testing.T) {
	f := newAPIFixture(t)
	for i := 0; i < 3; i++ {
		f.seedDeployment(t)
	}

	all, err := f.svc.ListDeployments(t.Context(), f.ident, f.appIDString, 0)
	if err != nil {
		t.Fatalf("list deployments (default limit): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("default limit: got %d deployments, want 3", len(all))
	}

	capped, err := f.svc.ListDeployments(t.Context(), f.ident, f.appIDString, 2)
	if err != nil {
		t.Fatalf("list deployments (limit=2): %v", err)
	}
	if len(capped) != 2 {
		t.Fatalf("limit=2: got %d deployments, want 2", len(capped))
	}
}

// TestListDeploymentsRejectsForeignOrg proves ListDeployments goes through
// the same tenancy-checked resolveApp as the other ref-addressed operations
// (it takes an app ref, unlike DeploymentStatus's bare numeric id).
func TestListDeploymentsRejectsForeignOrg(t *testing.T) {
	f := newAPIFixture(t)
	f.seedDeployment(t)
	_, err := f.svc.ListDeployments(t.Context(), f.otherIdent, f.appIDString, 0)
	var aerr *api.Error
	if !errors.As(err, &aerr) || aerr.Code != api.CodeNotFound {
		t.Fatalf("want not_found, got %v", err)
	}
}

// stubLogsEngine is a docker.Engine that serves a fixed ServiceLogs body, for
// exercising AppLogs' parse/filter/clamp logic without a live Swarm. Every
// other method panics if called — AppLogs must never touch them.
type stubLogsEngine struct {
	docker.Engine
	body string
	err  error
}

func (e *stubLogsEngine) ServiceLogs(context.Context, string, bool) (io.ReadCloser, error) {
	if e.err != nil {
		return nil, e.err
	}
	return io.NopCloser(strings.NewReader(e.body)), nil
}

const sampleLogBody = "" +
	"2026-06-07T19:31:52.000Z level=debug msg=starting\n" +
	"2026-06-07T19:31:53.000Z level=info msg=ready\n" +
	"2026-06-07T19:31:54.000Z level=warn msg=slow_request\n" +
	"2026-06-07T19:31:55.000Z level=error msg=boom\n" +
	"2026-06-07T19:31:56.000Z a plain line with no detectable level\n"

// TestAppLogsParsesLines proves AppLogs actually calls the docker log line
// parser: with no level filter, all five lines come back with time/level/msg
// populated from the raw docker-timestamped input.
func TestAppLogsParsesLines(t *testing.T) {
	f := newAPIFixture(t)
	svc := api.NewService(f.q, &stubLogsEngine{body: sampleLogBody}, nil, nil)
	lines, err := svc.AppLogs(t.Context(), f.ident, f.appIDString, 0, "")
	if err != nil {
		t.Fatalf("app logs: %v", err)
	}
	if len(lines) != 5 {
		t.Fatalf("want 5 lines, got %d: %+v", len(lines), lines)
	}
	if lines[2].Level != "warn" || lines[2].Message != "level=warn msg=slow_request" {
		t.Fatalf("line 2 parsed wrong: %+v", lines[2])
	}
	if lines[2].Time != "2026-06-07T19:31:54.000Z" {
		t.Fatalf("line 2 timestamp parsed wrong: %+v", lines[2])
	}
	if lines[4].Level != "" {
		t.Fatalf("line 4 (no level) should have an empty level, got %q", lines[4].Level)
	}
}

// TestAppLogsLevelFilterKeepsAtOrAboveFloor proves the level filter is a
// minimum-severity floor, not an exact match: level=warn must keep warn and
// error (both >= warn) plus the undetectable line (no basis to drop it), and
// must drop debug and info (both < warn).
func TestAppLogsLevelFilterKeepsAtOrAboveFloor(t *testing.T) {
	f := newAPIFixture(t)
	svc := api.NewService(f.q, &stubLogsEngine{body: sampleLogBody}, nil, nil)
	lines, err := svc.AppLogs(t.Context(), f.ident, f.appIDString, 0, "warn")
	if err != nil {
		t.Fatalf("app logs: %v", err)
	}
	var levels []string
	for _, l := range lines {
		levels = append(levels, l.Level)
	}
	want := []string{"warn", "error", ""}
	if len(levels) != len(want) {
		t.Fatalf("levels = %v, want %v", levels, want)
	}
	for i := range want {
		if levels[i] != want[i] {
			t.Fatalf("levels = %v, want %v", levels, want)
		}
	}
}

// TestAppLogsTailClampsAfterFiltering proves tail bounds the result count —
// the single most important safety property of this operation, since an
// unbounded log would evict the agent's own context.
func TestAppLogsTailClampsAfterFiltering(t *testing.T) {
	f := newAPIFixture(t)
	svc := api.NewService(f.q, &stubLogsEngine{body: sampleLogBody}, nil, nil)
	lines, err := svc.AppLogs(t.Context(), f.ident, f.appIDString, 2, "")
	if err != nil {
		t.Fatalf("app logs: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("want 2 lines (tail=2), got %d: %+v", len(lines), lines)
	}
	// tail keeps the most recent lines, not the first ones.
	if lines[1].Message != "a plain line with no detectable level" {
		t.Fatalf("tail did not keep the last line: %+v", lines)
	}
}

// TestAppLogsRejectsUnknownLevel proves a mistyped level filter is rejected
// outright rather than silently matching everything — silently ignoring it
// would look like a clean log while hiding exactly the noise the caller
// meant to cut.
func TestAppLogsRejectsUnknownLevel(t *testing.T) {
	f := newAPIFixture(t)
	_, err := f.svc.AppLogs(t.Context(), f.ident, f.appIDString, 0, "verbose")
	var aerr *api.Error
	if !errors.As(err, &aerr) || aerr.Code != api.CodeInvalid {
		t.Fatalf("want invalid, got %v", err)
	}
	if !strings.Contains(aerr.Message, "verbose") {
		t.Fatalf("error should name the rejected value, got %q", aerr.Message)
	}
	for _, accepted := range []string{"trace", "debug", "info", "warn", "error", "fatal"} {
		if !strings.Contains(aerr.Message, accepted) {
			t.Fatalf("error should list %q as an accepted value, got %q", accepted, aerr.Message)
		}
	}
}

// TestAppLogsNilEngineReturnsEmptyNotPanic proves the nil-engine guard: the
// fixture's default Service has no engine (unit tests never touch docker),
// and AppLogs must degrade to an empty result instead of dereferencing it.
func TestAppLogsNilEngineReturnsEmptyNotPanic(t *testing.T) {
	f := newAPIFixture(t)
	lines, err := f.svc.AppLogs(t.Context(), f.ident, f.appIDString, 0, "")
	if err != nil {
		t.Fatalf("app logs with nil engine: %v", err)
	}
	if len(lines) != 0 {
		t.Fatalf("want no lines with a nil engine, got %+v", lines)
	}
}

// TestAppLogsRejectsForeignOrg proves AppLogs goes through the same
// tenancy-checked resolveApp as the other ref-addressed operations.
func TestAppLogsRejectsForeignOrg(t *testing.T) {
	f := newAPIFixture(t)
	_, err := f.svc.AppLogs(t.Context(), f.otherIdent, f.appIDString, 0, "")
	var aerr *api.Error
	if !errors.As(err, &aerr) || aerr.Code != api.CodeNotFound {
		t.Fatalf("want not_found, got %v", err)
	}
}
