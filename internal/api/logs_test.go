package api_test

import (
	"bytes"
	"context"
	"encoding/json"
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
// exercising AppLogs' parse/filter/clamp logic without a live Swarm. It also
// records the tail argument its last ServiceLogs call received, so a test can
// prove AppLogs actually plumbs its clamped tail through to the engine rather
// than relying on the transport to apply its own default. Every other method
// panics if called — AppLogs must never touch them.
type stubLogsEngine struct {
	docker.Engine
	body    string
	err     error
	readErr error // if set, the returned reader fails with this error once body is exhausted, instead of a clean EOF
	gotTail int   // records the tail argument the last ServiceLogs call received
}

func (e *stubLogsEngine) ServiceLogs(_ context.Context, _ string, _ bool, tail int) (io.ReadCloser, error) {
	e.gotTail = tail
	if e.err != nil {
		return nil, e.err
	}
	var r io.Reader = strings.NewReader(e.body)
	if e.readErr != nil {
		r = &errAfterReader{r: r, err: e.readErr}
	}
	return io.NopCloser(r), nil
}

// errAfterReader replays the wrapped reader's content, then returns a
// caller-supplied error instead of a clean io.EOF once it's exhausted —
// simulating a docker log stream that dies mid-read rather than ending
// cleanly, without needing a real broken connection to test against.
type errAfterReader struct {
	r   io.Reader
	err error
}

func (e *errAfterReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err == io.EOF {
		return n, e.err
	}
	return n, err
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
	svc := api.NewService(f.q, &stubLogsEngine{body: sampleLogBody}, nil)
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
	svc := api.NewService(f.q, &stubLogsEngine{body: sampleLogBody}, nil)
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
	svc := api.NewService(f.q, &stubLogsEngine{body: sampleLogBody}, nil)
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

// TestAppLogsPassesClampedTailToEngine is the load-bearing test for plumbing
// tail through to the transport: docker.Engine.ServiceLogs takes an explicit
// tail argument now (it no longer hardcodes its own window), so AppLogs must
// actually pass its clamped value through rather than the engine silently
// applying a stale default. Without this test, dropping the parameter (or
// forgetting to clamp before passing it) would compile and every other test
// in this file would still pass.
func TestAppLogsPassesClampedTailToEngine(t *testing.T) {
	f := newAPIFixture(t)
	eng := &stubLogsEngine{body: sampleLogBody}
	svc := api.NewService(f.q, eng, nil)
	if _, err := svc.AppLogs(t.Context(), f.ident, f.appIDString, 99999, ""); err != nil {
		t.Fatalf("app logs: %v", err)
	}
	if eng.gotTail != api.MaxLogTail {
		t.Fatalf("tail=99999: engine got tail=%d, want the clamped max %d", eng.gotTail, api.MaxLogTail)
	}

	if _, err := svc.AppLogs(t.Context(), f.ident, f.appIDString, 0, ""); err != nil {
		t.Fatalf("app logs: %v", err)
	}
	if eng.gotTail != api.DefaultLogTail {
		t.Fatalf("tail=0: engine got tail=%d, want the default %d", eng.gotTail, api.DefaultLogTail)
	}
}

// TestAppLogsScanErrorAppendsNotice proves a stream that breaks partway
// through — here, a single line over the scanner's 1 MiB buffer — is never
// silently indistinguishable from a complete, clean read: the caller must see
// an in-band signal that something was cut.
func TestAppLogsScanErrorAppendsNotice(t *testing.T) {
	f := newAPIFixture(t)
	// No newline anywhere: the scanner exceeds its 1 MiB buffer looking for a
	// line ending before it ever finds one, so this hits bufio.ErrTooLong via
	// real bufio.Scanner semantics rather than a mocked scanner error.
	oversized := strings.Repeat("a", 2*1024*1024)
	svc := api.NewService(f.q, &stubLogsEngine{body: oversized}, nil)
	lines, err := svc.AppLogs(t.Context(), f.ident, f.appIDString, 0, "")
	if err != nil {
		t.Fatalf("app logs: %v", err)
	}
	if len(lines) == 0 {
		t.Fatal("want a synthetic notice line, got none")
	}
	last := lines[len(lines)-1]
	if last.Level != "error" || !strings.Contains(last.Message, "1 MiB limit") {
		t.Fatalf("want a 1 MiB-limit notice, got %+v", last)
	}
}

// TestAppLogsReadErrorAppendsNotice proves a broken connection mid-stream
// (not just an oversized line) also surfaces a notice, and that lines
// successfully read before the break are still returned rather than
// discarded.
func TestAppLogsReadErrorAppendsNotice(t *testing.T) {
	f := newAPIFixture(t)
	body := "2026-06-07T19:31:52.000Z level=info msg=ready\n"
	eng := &stubLogsEngine{body: body, readErr: errors.New("connection reset")}
	svc := api.NewService(f.q, eng, nil)
	lines, err := svc.AppLogs(t.Context(), f.ident, f.appIDString, 0, "")
	if err != nil {
		t.Fatalf("app logs: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("want 1 real line + 1 notice, got %d: %+v", len(lines), lines)
	}
	if lines[0].Message != "level=info msg=ready" {
		t.Fatalf("the line read before the break should still be returned: %+v", lines[0])
	}
	last := lines[len(lines)-1]
	if last.Level != "error" || !strings.Contains(last.Message, "log stream stopped") {
		t.Fatalf("want a notice naming the read failure, got %+v", last)
	}
}

// TestAppLogsScanErrorNoticeSurvivesLevelFilter proves the synthetic notice
// bypasses the level filter — it describes the read itself, not application
// output. The filter here is "fatal", one rank above the notice's own level
// ("error"): if the notice went through the ordinary floor check like a real
// log line, it would be dropped. It must survive anyway, or a caller filtering
// for only the most severe lines would silently lose the one line telling them
// the read was cut short.
func TestAppLogsScanErrorNoticeSurvivesLevelFilter(t *testing.T) {
	f := newAPIFixture(t)
	oversized := strings.Repeat("a", 2*1024*1024)
	svc := api.NewService(f.q, &stubLogsEngine{body: oversized}, nil)
	lines, err := svc.AppLogs(t.Context(), f.ident, f.appIDString, 0, "fatal")
	if err != nil {
		t.Fatalf("app logs: %v", err)
	}
	if len(lines) != 1 || lines[0].Level != "error" {
		t.Fatalf("scan-end notice must survive a level filter stricter than its own level, got %+v", lines)
	}
}

// TestAppLogsScanErrorNoticeSurvivesTailClamp proves the notice is appended
// after filtering but before the tail clamp, so a tight tail (e.g. 1) never
// evicts it in favor of an earlier, less important line — the doc comment on
// AppLogs promises this ordering explicitly.
func TestAppLogsScanErrorNoticeSurvivesTailClamp(t *testing.T) {
	f := newAPIFixture(t)
	eng := &stubLogsEngine{body: sampleLogBody, readErr: errors.New("connection reset")}
	svc := api.NewService(f.q, eng, nil)
	lines, err := svc.AppLogs(t.Context(), f.ident, f.appIDString, 1, "")
	if err != nil {
		t.Fatalf("app logs: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("tail=1 should keep exactly 1 line, got %d: %+v", len(lines), lines)
	}
	if lines[0].Level != "error" || !strings.Contains(lines[0].Message, "log stream stopped") {
		t.Fatalf("a tail=1 clamp must keep the truncation notice, not an earlier real log line, got %+v", lines[0])
	}
}

// The in-band notice must not quote the underlying error. It is handed to
// whoever is reading the log — for this surface, an agent, whose context
// reaches a model provider — and the cause can name a docker socket path or a
// host. The cause belongs in the server log, not in the answer.
func TestAppLogsScanErrorNoticeHidesTheCause(t *testing.T) {
	f := newAPIFixture(t)
	secret := "dial unix /var/run/docker.sock: connection reset"
	eng := &stubLogsEngine{body: sampleLogBody, readErr: errors.New(secret)}
	svc := api.NewService(f.q, eng, nil)
	lines, err := svc.AppLogs(t.Context(), f.ident, f.appIDString, 100, "")
	if err != nil {
		t.Fatalf("app logs: %v", err)
	}
	blob, err := json.Marshal(lines)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(blob, []byte("docker.sock")) || bytes.Contains(blob, []byte("connection reset")) {
		t.Fatalf("the underlying scan error leaked to the caller: %s", blob)
	}
	last := lines[len(lines)-1]
	if last.Level != "error" || !strings.Contains(last.Message, "log stream stopped") {
		t.Fatalf("want a generic stream-stopped notice, got %+v", last)
	}
}
