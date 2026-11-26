package api

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
)

// TaskStatus is one Swarm task of an app's service — what `docker service ps`
// shows.
type TaskStatus struct {
	ID           string `json:"id"`
	Node         string `json:"node,omitempty"`
	State        string `json:"state"`
	DesiredState string `json:"desired_state"`
	Message      string `json:"message,omitempty"`
	Error        string `json:"error,omitempty"`
	ExitCode     int    `json:"exit_code"`
	CreatedAt    string `json:"created_at,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

// TaskHealth is the healthcheck state of a task's container. The probe's
// output is never returned: a probe can print an env value or a credential.
type TaskHealth struct {
	TaskID        string `json:"task_id"`
	Node          string `json:"node,omitempty"`
	Status        string `json:"status"` // starting | healthy | unhealthy
	FailingStreak int    `json:"failing_streak"`
	LastExitCode  int    `json:"last_exit_code"`
	LastAt        string `json:"last_at,omitempty"`
}

// TaskUsage is what one task's container uses: memory read live from its
// node, CPU from the monitoring sampler when the sample can only be this
// container's (see attributeCPU).
type TaskUsage struct {
	TaskID        string   `json:"task_id"`
	Node          string   `json:"node,omitempty"`
	MemBytes      int64    `json:"mem_bytes"`
	MemLimitBytes int64    `json:"mem_limit_bytes"`
	CPUPct        *float64 `json:"cpu_pct,omitempty"`
	CPUSampledAt  string   `json:"cpu_sampled_at,omitempty"`
}

const (
	// statusTaskCount is how many recent tasks AppStatus shows.
	statusTaskCount = 5
	// statusTaskHistory bounds the task read the failure count goes over;
	// Swarm itself keeps about five tasks per replica slot.
	statusTaskHistory = 100
	// statusTaskTimeout bounds the task read, statusProbeTimeout every
	// container inspection together.
	statusTaskTimeout  = 5 * time.Second
	statusProbeTimeout = 3 * time.Second
	// usageWindowIntervals is how many sampler intervals back a sample still
	// counts as current.
	usageWindowIntervals = 3
)

// taskEnded reports a task that ran and stopped on its own: a crash, a
// refused start, or a process that exited and was restarted.
func taskEnded(state string) bool {
	return state == "failed" || state == "rejected" || state == "complete"
}

// statusDetail is what AppStatus adds from Swarm and the sampler.
type statusDetail struct {
	node        string
	tasks       []TaskStatus
	failed      *int
	failedSince string
	health      []TaskHealth
	usage       []TaskUsage
}

// appStatusDetail reads the app's recent tasks, their health and the latest
// usage sample. Every part is best-effort: a part that fails is left out and
// logged, never failing the status call. lastDeploy is the start of the
// latest deployment (zero: none).
func (s *Service) appStatusDetail(ctx context.Context, app db.Application, lastDeploy time.Time) statusDetail {
	var out statusDetail
	name := docker.ServiceName(app.ID)
	if s.engine != nil {
		th, ok := s.engine.(docker.TaskHistory)
		if !ok {
			out.node = s.runningNode(ctx, name)
		} else {
			tctx, cancel := context.WithTimeout(ctx, statusTaskTimeout)
			recs, err := th.ServiceTaskHistory(tctx, name, statusTaskHistory)
			cancel()
			if err != nil {
				slog.Warn("app status: task history unavailable", "app_id", app.ID, "err", err)
			} else {
				out.node = firstRunningNode(recs)
				out.tasks = taskStatuses(recs, statusTaskCount)
				if !lastDeploy.IsZero() {
					n := failedSince(recs, lastDeploy)
					out.failed, out.failedSince = &n, lastDeploy.Format(time.RFC3339)
				}
				out.health, out.usage = s.probeTasks(ctx, app.ID, recs)
				s.attributeCPU(ctx, app.ID, recs, out.usage)
			}
		}
	}
	return out
}

// runningNode is the node of the service's first running task, for an engine
// without task history.
func (s *Service) runningNode(ctx context.Context, name string) string {
	tasks, err := s.engine.ServiceTasks(ctx, name)
	if err != nil {
		return ""
	}
	for _, t := range tasks {
		if t.State == "running" {
			return t.NodeName
		}
	}
	return ""
}

func firstRunningNode(recs []docker.TaskRecord) string {
	for _, r := range recs {
		if r.State == "running" {
			return r.NodeName
		}
	}
	return ""
}

// shortID is the 12-character form `docker service ps` shows.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// taskStatuses maps the newest n records (already newest first).
func taskStatuses(recs []docker.TaskRecord, n int) []TaskStatus {
	if len(recs) > n {
		recs = recs[:n]
	}
	out := make([]TaskStatus, 0, len(recs))
	for _, r := range recs {
		out = append(out, TaskStatus{
			ID: shortID(r.ID), Node: r.NodeName, State: r.State, DesiredState: r.Desired,
			Message: r.Message, Error: r.Err, ExitCode: r.ExitCode,
			CreatedAt: formatTime(r.Created), UpdatedAt: formatTime(r.Updated),
		})
	}
	return out
}

// failedSince counts the tasks created at or after since that ended on their
// own. Swarm keeps only a few tasks per replica slot, so this is a lower
// bound on a long crash loop.
func failedSince(recs []docker.TaskRecord, since time.Time) int {
	n := 0
	for _, r := range recs {
		if !r.Created.IsZero() && !r.Created.Before(since) && taskEnded(r.State) {
			n++
		}
	}
	return n
}

// probed is one task's container probe result.
type probed struct {
	rec   docker.TaskRecord
	probe docker.ContainerProbe
}

// probeTasks inspects the container of every task meant to run (a task still
// starting behind its healthcheck included), on its node, in parallel and
// within statusProbeTimeout. A failed probe, or an engine that cannot probe,
// leaves that task out.
func (s *Service) probeTasks(ctx context.Context, appID int64, recs []docker.TaskRecord) ([]TaskHealth, []TaskUsage) {
	pr, ok := s.engine.(docker.ContainerProber)
	if !ok {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, statusProbeTimeout)
	defer cancel()
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		done []probed
	)
	for _, r := range recs {
		if r.Desired != "running" || r.ContainerID == "" {
			continue
		}
		wg.Add(1)
		go func(r docker.TaskRecord) {
			defer wg.Done()
			p, err := pr.ProbeContainer(ctx, r.NodeID, r.ContainerID)
			if err != nil {
				slog.Warn("app status: container probe failed", "app_id", appID, "task", shortID(r.ID), "err", err)
				return
			}
			mu.Lock()
			done = append(done, probed{r, p})
			mu.Unlock()
		}(r)
	}
	wg.Wait()
	sort.Slice(done, func(i, j int) bool { return done[i].rec.ID < done[j].rec.ID })
	var health []TaskHealth
	var usage []TaskUsage
	for _, d := range done {
		id := shortID(d.rec.ID)
		if h := d.probe.Health; h != nil {
			health = append(health, TaskHealth{
				TaskID: id, Node: d.rec.NodeName, Status: h.Status, FailingStreak: h.FailingStreak,
				LastExitCode: h.LastExitCode, LastAt: formatTime(h.LastAt),
			})
		}
		if d.probe.MemOK {
			usage = append(usage, TaskUsage{
				TaskID: id, Node: d.rec.NodeName, MemBytes: d.probe.MemBytes, MemLimitBytes: d.probe.MemLimitBytes,
			})
		}
	}
	return health, usage
}

// controlPlaneNode is the name the monitoring sampler records the local
// node's samples under (cmd/krill); workers use their cluster_nodes name.
const controlPlaneNode = "control-plane"

// attributeCPU fills in the CPU of each usage entry from the monitoring
// sampler, but only where the sample can only be that task's container: the
// sampler keys rows by node and service, not by container. That holds when
// the task is the only one of the service meant to run on its node, and the
// sample was taken after the task was created and after every other task of
// the service on that node last changed state (a predecessor stopping during
// a rolling update is sampled under the same key). A sample older than a few
// sampler intervals is stale and not used either.
func (s *Service) attributeCPU(ctx context.Context, appID int64, recs []docker.TaskRecord, usage []TaskUsage) {
	if len(usage) == 0 {
		return
	}
	interval := s.metricsInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	rows, err := s.q.MetricSamplesSinceForComponents(ctx, db.MetricSamplesSinceForComponentsParams{
		Ts: time.Now().Add(-usageWindowIntervals * interval), Components: []string{docker.ServiceName(appID)},
	})
	if err != nil {
		slog.Warn("app status: usage samples unavailable", "app_id", appID, "err", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	latest := map[string]db.MetricSamplesSinceForComponentsRow{}
	for _, r := range rows {
		if cur, ok := latest[r.Node]; !ok || r.Ts.After(cur.Ts) {
			latest[r.Node] = r
		}
	}
	names, err := s.samplerNodeNames(ctx)
	if err != nil {
		slog.Warn("app status: node names unavailable", "app_id", appID, "err", err)
		return
	}
	byID := map[string]docker.TaskRecord{}
	for _, r := range recs {
		byID[shortID(r.ID)] = r
	}
	for i := range usage {
		t, ok := byID[usage[i].TaskID]
		if !ok || t.Created.IsZero() {
			continue
		}
		cutoff := t.Created
		alone := true
		for _, o := range recs {
			if o.ID == t.ID || o.NodeID != t.NodeID {
				continue
			}
			if o.Desired == "running" {
				alone = false
				break
			}
			if o.Updated.After(cutoff) {
				cutoff = o.Updated
			}
		}
		if !alone {
			continue
		}
		node := names[t.NodeID]
		if node == "" {
			node = controlPlaneNode
		}
		if smp, ok := latest[node]; ok && smp.Ts.After(cutoff) {
			cpu := float64(smp.CpuPct)
			usage[i].CPUPct, usage[i].CPUSampledAt = &cpu, formatTime(smp.Ts)
		}
	}
}

// samplerNodeNames maps a worker's Swarm node id to the name the sampler
// records it under.
func (s *Service) samplerNodeNames(ctx context.Context) (map[string]string, error) {
	nodes, err := s.q.ListClusterNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(nodes))
	for _, n := range nodes {
		if n.SwarmNodeID != "" {
			out[n.SwarmNodeID] = n.Name
		}
	}
	return out, nil
}
