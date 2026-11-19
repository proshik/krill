package notify

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/metrics"
)

const (
	// diskRepeat is how often a disk that stays over its threshold is
	// reported again.
	diskRepeat = 24 * time.Hour
	// diskHysteresis is how far (percentage points) under the threshold a
	// disk has to fall before "back to normal" is sent, so a disk hovering
	// at the threshold does not alternate alerts and recoveries.
	diskHysteresis = 3.0
	// overcommitTicks is how many sampler ticks a deployed app's container
	// may take to appear before its memory check is dropped.
	overcommitTicks = 4
	// monitorTimeout bounds one evaluation pass (state lookups and sends).
	monitorTimeout = 2 * time.Minute
)

type diskAction int

const (
	diskNone diskAction = iota
	diskAlert
	diskRecover
)

// diskDecide decides what one channel says about one filesystem.
func diskDecide(st AlertState, pct float64, threshold int, now time.Time) diskAction {
	if pct >= float64(threshold) {
		if !st.Active || st.LastSent == nil || now.Sub(*st.LastSent) >= diskRepeat {
			return diskAlert
		}
		return diskNone
	}
	if st.Active && pct < float64(threshold)-diskHysteresis {
		return diskRecover
	}
	return diskNone
}

// diskKey names a filesystem's alert in notify_alert_state.
func diskKey(node, mount string) string { return "disk:" + node + ":" + mount }

// nodeLimits sums the memory limits of a node's containers. A container
// without a limit reports the host's memory (or 0) as its limit; it is
// counted apart, since it is bounded only by the node itself.
func nodeLimits(ns metrics.NodeSample) (limits int64, unlimited int) {
	total := ns.Capacity.MemTotal
	for _, c := range ns.Containers {
		if c.MemLimitBytes <= 0 || (total > 0 && c.MemLimitBytes >= total) {
			unlimited++
			continue
		}
		limits += c.MemLimitBytes
	}
	return limits, unlimited
}

type pendingCheck struct {
	target Target
	at     time.Time
	ticks  int
}

// ResourceMonitor watches the sampler's ticks for node-level alerts: a disk
// over its channel's threshold, and memory limits overcommitted on the node a
// just-deployed app landed on. It sends only to channels with NotifyInfra.
type ResourceMonitor struct {
	svc   *Service
	store Store
	log   *slog.Logger
	now   func() time.Time

	mu      sync.Mutex
	pending map[int64]pendingCheck // app id → overcommit check
	busy    bool                   // an evaluation pass is running
	// wg tracks the running pass (tests wait on it).
	wg sync.WaitGroup
}

// NewResourceMonitor creates a monitor that sends through svc.
func NewResourceMonitor(svc *Service, store Store) *ResourceMonitor {
	return &ResourceMonitor{svc: svc, store: store, log: slog.Default(), now: time.Now, pending: map[int64]pendingCheck{}}
}

// QueueOvercommitCheck asks for the memory check of the node(s) appID runs
// on, once a tick that started after `at` has sampled them.
func (m *ResourceMonitor) QueueOvercommitCheck(appID int64, target Target, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending[appID] = pendingCheck{target: target, at: at}
}

// Observe implements metrics.Observer. The evaluation (database lookups and
// Telegram sends) runs off the sampler goroutine; a tick arriving while the
// previous pass still runs leaves its disks unchecked and its overcommit
// checks queued for the next tick.
func (m *ResourceMonitor) Observe(_ context.Context, t metrics.Tick) {
	m.mu.Lock()
	if m.busy {
		m.mu.Unlock()
		return
	}
	due := map[int64]pendingCheck{}
	for id, p := range m.pending {
		if p.at.Before(t.Start) {
			due[id] = p
		}
	}
	if !t.Disks && len(due) == 0 {
		m.mu.Unlock()
		return
	}
	m.busy = true
	m.wg.Add(1)
	m.mu.Unlock()

	go func() {
		defer m.wg.Done()
		defer func() { m.mu.Lock(); m.busy = false; m.mu.Unlock() }()
		ctx, cancel := context.WithTimeout(context.Background(), monitorTimeout)
		defer cancel()
		m.evaluate(ctx, t, due)
	}()
}

func (m *ResourceMonitor) evaluate(ctx context.Context, t metrics.Tick, due map[int64]pendingCheck) {
	chans, err := m.store.InfraChannels(ctx)
	if err != nil {
		m.log.Warn("notify monitor: load infra channels failed", "err", err)
		if len(chans) == 0 {
			return // keep the checks queued for the next tick
		}
	}
	if t.Disks {
		for _, ns := range t.Nodes {
			if !ns.DisksOK {
				continue
			}
			for _, d := range ns.Disks {
				for _, c := range chans {
					m.checkDisk(ctx, c, ns.Node, d)
				}
			}
		}
	}
	for appID, p := range due {
		done := m.checkOvercommit(ctx, chans, t, appID, p)
		m.mu.Lock()
		cur, still := m.pending[appID]
		switch {
		case !still || cur.at != p.at:
			// re-queued by a newer deploy meanwhile: leave the new check
		case done || p.ticks+1 >= overcommitTicks:
			delete(m.pending, appID)
		default:
			cur.ticks++
			m.pending[appID] = cur
		}
		m.mu.Unlock()
	}
}

func (m *ResourceMonitor) checkDisk(ctx context.Context, c Channel, node string, d metrics.DiskUsage) {
	key := diskKey(node, d.Mount)
	st, err := m.store.AlertState(ctx, c.ID, key)
	if err != nil {
		m.log.Warn("notify monitor: load alert state failed", "channel", c.ID, "key", key, "err", err)
		return
	}
	now := m.now()
	pct := d.UsedPct()
	action := diskDecide(st, pct, c.DiskThresholdPct, now)
	if action == diskNone {
		return
	}
	kind, next := DiskHigh, AlertState{Active: true, LastSent: &now}
	if action == diskRecover {
		kind, next = DiskRecovered, AlertState{Active: false, LastSent: &now}
	}
	ev := Event{Kind: kind, Time: now, Instance: m.svc.instance(ctx), Disk: &DiskDetail{
		Node: node, Mount: d.Mount, Pct: pct, Used: d.Used, Total: d.Total, Avail: d.Avail, Threshold: c.DiskThresholdPct,
	}}
	// The state moves only once the message is out, so a failed send is
	// retried on the next disk snapshot instead of waiting a day.
	if err := m.svc.sendTo(ctx, c, ev); err != nil {
		return
	}
	if err := m.store.SetAlertState(ctx, c.ID, key, next); err != nil {
		m.log.Warn("notify monitor: save alert state failed", "channel", c.ID, "key", key, "err", err)
	}
}

// checkOvercommit evaluates the nodes appID's containers run on, reporting
// whether it found any (false: the container is not sampled yet).
func (m *ResourceMonitor) checkOvercommit(ctx context.Context, chans []Channel, t metrics.Tick, appID int64, p pendingCheck) bool {
	svc := docker.ServiceName(appID)
	found := false
	for _, ns := range t.Nodes {
		if !ns.OK || !ns.CapacityOK || ns.Capacity.MemTotal <= 0 || !hasComponent(ns, svc) {
			continue
		}
		found = true
		limits, unlimited := nodeLimits(ns)
		pct := float64(limits) * 100 / float64(ns.Capacity.MemTotal)
		for _, c := range chans {
			if pct < float64(c.MemThresholdPct) {
				continue
			}
			// The node is the operator's business, another organization's
			// app is not: name the deploy only to its own organization.
			app := ""
			if c.OrgID == p.target.OrgID {
				app = targetPath(p.target.Project, p.target.Env, p.target.Name)
			}
			_ = m.svc.sendTo(ctx, c, Event{Kind: MemOvercommit, Time: m.now(), Instance: m.svc.instance(ctx), Mem: &MemDetail{
				Node: ns.Node, App: app,
				Limits: limits, Total: ns.Capacity.MemTotal, Pct: pct,
				Threshold: c.MemThresholdPct, Unlimited: unlimited,
			}})
		}
	}
	return found
}

func hasComponent(ns metrics.NodeSample, component string) bool {
	for _, c := range ns.Containers {
		if c.Component == component {
			return true
		}
	}
	return false
}
