package api

import (
	"bufio"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/proshik/krill/internal/docker"
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
	Trigger   string `json:"trigger"` // manual | webhook | schedule
	ImageTag  string `json:"image_tag,omitempty"`
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at,omitempty"`
}

// DeploymentDetail is one deployment's full status plus its bounded build log.
type DeploymentDetail struct {
	DeploymentSummary
	LogTail string `json:"log_tail"` // last ~8 KB, with a truncation marker when it was cut
}

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
	notFound := NotFound(fmt.Sprintf("no deployment %d in this organization", deployID))

	dep, err := s.q.GetDeployment(ctx, deployID)
	if err != nil {
		return DeploymentDetail{}, notFound
	}
	chain, err := s.q.GetApplicationChain(ctx, dep.ApplicationID)
	if err != nil || chain.OrgID != id.OrgID {
		return DeploymentDetail{}, notFound
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
		LogTail: tailBytes(dep.Log, buildLogTailBytes),
	}, nil
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

// AppLogs tails an application's live runtime log (stdout/stderr from its
// current container), parsed into structured lines and optionally filtered
// to a minimum severity. tail is clamped by ClampTail; the underlying engine
// already caps what ServiceLogs itself returns, so tail can only narrow that
// further, never widen it.
func (s *Service) AppLogs(ctx context.Context, id Identity, ref string, tail int, level string) ([]LogLine, error) {
	app, err := s.resolveApp(ctx, id, ref)
	if err != nil {
		return nil, err
	}
	minRank, err := parseLevelFilter(level)
	if err != nil {
		return nil, err
	}
	// No engine (unit tests, or docker unreachable): there is no log source to
	// read from, so report no lines rather than dereferencing a nil engine.
	if s.engine == nil {
		return []LogLine{}, nil
	}
	rc, err := s.engine.ServiceLogs(ctx, docker.ServiceName(app.ID), false)
	if err != nil {
		return nil, fmt.Errorf("app logs: service logs: %w", err)
	}
	if rc == nil {
		return []LogLine{}, nil
	}
	defer rc.Close()

	var lines []LogLine
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // tolerate long log lines
	for sc.Scan() {
		parsed := logparse.ParseLogLine(sc.Text())
		if minRank >= 0 {
			if r, ok := logLevelRank(parsed.Level); ok && r < minRank {
				continue
			}
		}
		lines = append(lines, LogLine{Time: parsed.Time, Level: parsed.Level, Message: parsed.Msg})
	}
	// A scan error (e.g. a line over the 1 MiB limit) still leaves everything
	// read so far usable — return that partial tail rather than discarding it;
	// for something advisory like log viewing, partial beats empty.

	n := ClampTail(tail)
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if lines == nil {
		lines = []LogLine{}
	}
	return lines, nil
}
