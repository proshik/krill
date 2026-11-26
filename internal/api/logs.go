package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/proshik/krill/internal/logparse"
)

// DefaultLogTail and MaxLogTail bound how many runtime log lines one AppLogs
// call may return. Unbounded logs would fill the agent's context window and
// evict the task it was called for.
const (
	DefaultLogTail = 200
	MaxLogTail     = 1000
)

// defaultDeployListLimit and maxDeployListLimit bound ListDeployments the same
// way: a caller with no opinion about how much history it wants gets a short,
// useful window; a caller asking for everything is still capped, matching the
// 50-row LIMIT already baked into ListDeploymentSummariesByApplication's SQL.
const (
	defaultDeployListLimit = 20
	maxDeployListLimit     = 50
)

// buildLogTailBytes bounds DeploymentStatus's embedded build log the same way
// AppLogs bounds runtime lines: a build log can reach ~256 KB, and returning
// it whole would blow an agent's context budget for a single status check.
const buildLogTailBytes = 8 * 1024

// ClampTail bounds how many log lines one call may return. Unbounded logs would
// fill the agent's context window and evict the task it was called for.
func ClampTail(n int) int {
	if n <= 0 {
		return DefaultLogTail
	}
	if n > MaxLogTail {
		return MaxLogTail
	}
	return n
}

// clampDeployListLimit is ClampTail's counterpart for ListDeployments' limit
// argument: same shape, different bounds.
func clampDeployListLimit(n int) int {
	if n <= 0 {
		return defaultDeployListLimit
	}
	if n > maxDeployListLimit {
		return maxDeployListLimit
	}
	return n
}

// DeploymentSummary is the list-view shape of one deployment: everything
// except the log, which can reach ~256 KB — ListDeployments intentionally
// queries around it (ListDeploymentSummariesByApplication). Use
// DeploymentStatus for the single-deployment detail/log view.
type DeploymentSummary struct {
	ID        int64  `json:"id"`
	Status    string `json:"status"`  // running | done | error
	Trigger   string `json:"trigger"` // manual | webhook | schedule | api | system
	ImageTag  string `json:"image_tag,omitempty"`
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at,omitempty"`
}

// DeploymentDetail is one deployment's full status plus its bounded build log.
type DeploymentDetail struct {
	DeploymentSummary
	LogTail string `json:"log_tail"` // last ~8 KB, with a truncation marker when it was cut
	// Settling: the deploy applied its spec and Krill is watching Swarm
	// converge in the background. Still running, not done: keep waiting.
	Settling bool `json:"settling,omitempty"`
}

// Deployment wait bounds, in seconds, and how often a wait looks again.
const (
	maxDeployWait  = 120
	deployWaitPoll = 500 * time.Millisecond
)

// LogLine is one parsed runtime log line, in the agent-facing API's own shape
// (deliberately distinct from logparse.LogLine — internal/server's WebSocket
// streaming code keeps using that one directly).
type LogLine struct {
	Time    string `json:"t"`
	Level   string `json:"lvl"`
	Message string `json:"msg"`
}

// formatOptionalTime renders a nullable timestamp as RFC3339, or "" when
// unset (a deployment still running has no finished_at).
func formatOptionalTime(t pgtype.Timestamptz) string {
	if !t.Valid {
		return ""
	}
	return t.Time.Format(time.RFC3339)
}

// ListDeployments returns an application's deployment history, most recent
// first, bounded to at most maxDeployListLimit rows regardless of what the
// caller asks for. It deliberately queries ListDeploymentSummariesByApplication
// rather than the log-carrying variant — a deploy log can reach ~256 KB, and a
// history listing has no business paying for that on every row.
func (s *Service) ListDeployments(ctx context.Context, id Identity, ref string, limit int) ([]DeploymentSummary, error) {
	app, err := s.resolveApp(ctx, id, ref)
	if err != nil {
		return nil, err
	}
	rows, err := s.q.ListDeploymentSummariesByApplication(ctx, app.ID)
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	n := clampDeployListLimit(limit)
	if len(rows) > n {
		rows = rows[:n] // already ORDER BY started_at DESC — this keeps the most recent n
	}
	out := make([]DeploymentSummary, 0, len(rows))
	for _, r := range rows {
		out = append(out, DeploymentSummary{
			ID:        r.ID,
			Status:    r.Status,
			Trigger:   r.Trigger,
			ImageTag:  r.ImageTag,
			StartedAt: r.StartedAt.Format(time.RFC3339),
			EndedAt:   formatOptionalTime(r.FinishedAt),
		})
	}
	return out, nil
}

// tailBytes returns the last max bytes of log, prefixed with a truncation
// marker naming how many bytes were dropped when log is longer than max.
func tailBytes(log string, max int) string {
	if len(log) <= max {
		return log
	}
	dropped := len(log) - max
	return fmt.Sprintf("...[truncated %d bytes]...\n", dropped) + log[len(log)-max:]
}

// DeploymentStatus reports one deployment's status and its bounded build log.
//
// A deployment is addressed by a bare numeric id, with no application in the
// path, so it needs its own tenancy check distinct from resolveApp's: without
// verifying that the deployment's application chain resolves into the
// caller's org, any token could read any other organization's build logs
// simply by guessing (or incrementing) ids — and those logs routinely contain
// source paths, dependency URLs, and sometimes secrets echoed by a build
// step. A mismatch reports not_found, never forbidden, for the same reason
// resolveApp does: telling the caller a deployment id exists elsewhere turns
// id enumeration into recon.
func (s *Service) DeploymentStatus(ctx context.Context, id Identity, deployID int64) (DeploymentDetail, error) {
	return s.DeploymentStatusWait(ctx, id, deployID, 0)
}

// DeploymentStatusWait is DeploymentStatus that, given wait seconds (at most
// maxDeployWait; more is clamped), first waits while the deployment is still
// running: it answers once the deployment ends, the time is up, the caller
// goes away or Krill starts shutting down — whichever comes first — with the
// state at that moment. The tenancy check comes first, so a foreign id is
// refused at once.
func (s *Service) DeploymentStatusWait(ctx context.Context, id Identity, deployID int64, wait int) (DeploymentDetail, error) {
	notFound := NotFound(fmt.Sprintf("no deployment %d in this organization", deployID))
	if wait < 0 {
		return DeploymentDetail{}, Invalid("wait must not be negative")
	}
	if wait > maxDeployWait {
		wait = maxDeployWait
	}
	if wait > 0 {
		// A waiting call ends on Krill's shutdown from its first read on.
		var cancel context.CancelFunc
		ctx, cancel = s.withStopping(ctx)
		defer cancel()
	}

	dep, err := s.q.GetDeployment(ctx, deployID)
	if err != nil {
		return DeploymentDetail{}, notFound
	}
	chain, err := s.q.GetApplicationChain(ctx, dep.ApplicationID)
	if err != nil || chain.OrgID != id.OrgID {
		return DeploymentDetail{}, notFound
	}

	if wait > 0 && dep.Status == "running" {
		// Polls run to the deadline; the reads after them get a short
		// fixed grace past it.
		deadline := time.Now().Add(time.Duration(wait) * time.Second)
		st, err := s.waitDeployment(ctx, deployID, time.Until(deadline), deadline.Add(waitReadGrace))
		if err != nil {
			return DeploymentDetail{}, err
		}
		if st != "running" {
			// Read the final row (its log, its end time) within the grace;
			// failing that, answer the status the wait saw rather than fail
			// a wait that worked.
			readBy := deadline
			if now := time.Now(); now.After(readBy) {
				readBy = now
			}
			rctx, cancel := s.boundedRead(ctx, readBy.Add(waitReadGrace))
			d, err := s.q.GetDeployment(rctx, deployID)
			cancel()
			if err == nil {
				dep = d
			} else {
				dep.Status = st
			}
		}
	}

	return DeploymentDetail{
		DeploymentSummary: DeploymentSummary{
			ID:        dep.ID,
			Status:    dep.Status,
			Trigger:   dep.Trigger,
			ImageTag:  dep.ImageTag,
			StartedAt: dep.StartedAt.Format(time.RFC3339),
			EndedAt:   formatOptionalTime(dep.FinishedAt),
		},
		LogTail:  tailBytes(dep.Log, buildLogTailBytes),
		Settling: dep.Status == "running" && s.dep != nil && s.dep.Settling(dep.ID),
	}, nil
}

// waitDeployment polls the deployment's status until it is no longer
// running, poll has passed, ctx ends or Krill starts shutting down, and
// returns the last status it read ("running" when it saw no change). When
// the polling time runs out, the status is read once more, by readBy, so a
// deploy that ended after the last poll is not reported running. A failed
// read is retried at the next poll; a deployment that is gone (its app was
// deleted meanwhile) is not found.
func (s *Service) waitDeployment(ctx context.Context, deployID int64, poll time.Duration, readBy time.Time) (string, error) {
	wctx, cancel, stoppedBy := s.waitContext(ctx, poll)
	defer cancel()
	t := time.NewTicker(deployWaitPoll)
	defer t.Stop()
	gone := NotFound(fmt.Sprintf("no deployment %d in this organization", deployID))
	read := func(ctx context.Context) (string, error) {
		st, err := s.q.GetDeploymentStatus(ctx, deployID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return "", gone
		case err != nil:
			if ctx.Err() == nil {
				slog.Warn("deployment wait: status read failed, retrying", "deployment_id", deployID, "err", err)
			}
			return "running", err
		}
		return st, nil
	}
	for {
		select {
		case <-wctx.Done():
			if stoppedBy() != "timeout" {
				return "running", nil // shutdown or the caller left: answer at once
			}
			rctx, rcancel := s.boundedRead(ctx, readBy)
			defer rcancel()
			st, err := read(rctx)
			if errors.Is(err, gone) {
				return "", err
			}
			return st, nil
		case <-t.C:
			st, err := read(wctx)
			switch {
			case errors.Is(err, gone):
				return "", err
			case err != nil:
				continue // transient, or the wait ended during the read: see Done
			case st != "running":
				return st, nil
			}
		}
	}
}

// waitReadGrace is how far past the requested wait the reads that close it
// may go: the status once more, then the final row. A caller should allow
// the wait plus a second.
const waitReadGrace = 250 * time.Millisecond

// withStopping is ctx ended also when Krill starts shutting down.
func (s *Service) withStopping(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	if s.stopping == nil {
		return ctx, cancel
	}
	stop := context.AfterFunc(s.stopping, cancel)
	return ctx, func() { stop(); cancel() }
}

// boundedRead is a context for one read after a wait: ended by ctx, by
// Krill's shutdown and at deadline.
func (s *Service) boundedRead(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithDeadline(ctx, deadline)
	if s.stopping == nil {
		return ctx, cancel
	}
	stop := context.AfterFunc(s.stopping, cancel)
	return ctx, func() { stop(); cancel() }
}

// logLevelOrder ranks recognized runtime log levels from least to most
// severe. AppLogs' level filter keeps only lines at or above the requested
// floor. A line whose level isn't in this list — undetected ("") or a
// non-severity tag like "success" — always passes the filter, since there is
// no basis to say whether it belongs above or below the requested floor.
var logLevelOrder = []string{"trace", "debug", "info", "warn", "error", "fatal"}

func logLevelRank(level string) (int, bool) {
	for i, l := range logLevelOrder {
		if l == level {
			return i, true
		}
	}
	return 0, false
}

// parseLevelFilter validates the caller-supplied level filter. "" disables
// filtering. An unrecognized value is rejected outright rather than silently
// ignored — a mistyped filter that quietly matched everything would look like
// a clean log, hiding exactly the noisy lines the caller meant to cut.
func parseLevelFilter(level string) (int, error) {
	if level == "" {
		return -1, nil
	}
	rank, ok := logLevelRank(strings.ToLower(level))
	if !ok {
		return -1, Invalid(fmt.Sprintf("unknown level %q: accepted values are %s", level, strings.Join(logLevelOrder, ", ")))
	}
	return rank, nil
}

// AppLogs tails an application's runtime log (stdout/stderr of its current
// task and of the stopped tasks Swarm still keeps in its history), parsed into structured lines and optionally filtered
// to a minimum severity. tail is clamped by ClampTail and passed straight to
// the engine, which honors it — docker.Engine.ServiceLogs takes an explicit
// tail argument rather than hardcoding its own window, so asking for 1000
// lines actually returns up to 1000, not a silent ~200. A scan error partway
// through the read (an oversized line, a broken connection) is surfaced
// in-band as a synthetic error-level line rather than silently truncating the
// result with nothing to say it was cut short — an agent reasoning from an
// incomplete tail with no signal it's incomplete is worse than a human
// noticing a short log and scrolling.
//
// Swarm applies the tail to each of the service's tasks separately and
// concatenates the results task by task, not by time — the live task's lines
// can come first and a dead task's older ones after them. Taking the last n
// lines of that stream would return whichever task happened to be last, so
// AppLogs merges every task's lines by their docker timestamp first and only
// then keeps the n most recent (see sortByLogTime). The read, opening
// included, is given up after appLogsTimeout (a Swarm log read can stall
// without ending): the lines read by then are returned, ending with a
// warn-level notice saying the answer is incomplete — the same notice ends an
// answer missing a task the engine could not read.
func (s *Service) AppLogs(ctx context.Context, id Identity, ref string, tail int, level string) ([]LogLine, error) {
	app, err := s.resolveApp(ctx, id, ref)
	if err != nil {
		return nil, err
	}
	return s.appLogs(ctx, app, LogsInput{Tail: tail, Level: level})
}

// appLogsTimeout bounds AppLogs' read of the log; a var so tests can
// shorten it.
var appLogsTimeout = logparse.BacklogTimeout

// cutNotice ends an incomplete answer: the read was given up, or the log of
// some task could not be read, so lines of some runs may be missing.
var cutNotice = logparse.LogLine{Level: "warn", Msg: "log read incomplete: lines of some runs may be missing"}

// timedLine is a parsed log line with the time it sorts by.
type timedLine struct {
	at   time.Time
	line LogLine
}

// sortByLogTime orders lines from every task of a service by time, oldest
// first. A line without a parseable docker timestamp (docker adds one to
// every line it was asked to, so this is rare) sorts with the line right
// before it in the stream — its sort key was inherited from that line while
// reading — so it stays next to the output it came with rather than being
// dropped or thrown to either end; one at the very start of the stream has
// nothing to follow and sorts first (zero time). The sort is stable, so lines
// with equal keys keep their stream order.
func sortByLogTime(entries []timedLine) {
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].at.Before(entries[j].at) })
}
