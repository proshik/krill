package notify

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/metrics"
	"github.com/proshik/krill/internal/web/i18n"
)

const (
	digestWindow = 24 * time.Hour
	// digestMaxWindow bounds how far back the events of a late digest reach.
	digestMaxWindow = 48 * time.Hour
	// digestLate is how long after its scheduled time a missed digest (Krill
	// was down at the time) is still sent; later it waits for the next day,
	// so a digest never arrives at night.
	digestLate = 3 * time.Hour
	// digestPartLimit keeps every message part well under Telegram's 4096
	// characters.
	digestPartLimit = 3500
	// staleAfter marks a node or app whose last sample is older than this as
	// not reporting.
	staleAfter = 10 * time.Minute
	// tickGap separates two sampler ticks in the stored samples: a tick's rows
	// are written within milliseconds, ticks are the sampling interval apart.
	tickGap = 5 * time.Second
	// Traffic-light bands (percent). The channel's own disk and memory-limit
	// thresholds are the red marks; the rest are fixed. An app's memory peak
	// is measured against its limit, a node's container memory and CPU
	// against the node (CPU as the day's average: a peak is a spike).
	appMemWarn, appMemCrit   = 80.0, 95.0
	nodeMemWarn, nodeMemCrit = 80.0, 90.0
	limitsWarn               = 80.0 // or the channel's threshold, if lower
	diskWarnBand             = 15.0 // yellow this many points under the threshold
	cpuWarn, cpuCrit         = 70.0, 90.0
	defaultDiskThreshold     = 85
	defaultMemThreshold      = 100
	// digestListMax caps the deploy and backup lines; the rest is counted.
	digestListMax = 20
	// backupRunsKeep is how long the backup journal is kept.
	backupRunsKeep = 30 * 24 * time.Hour
)

// DefaultDigestTZ is the zone a channel's digest time is read in by default.
const DefaultDigestTZ = "Europe/Moscow"

// MetricsReader is the slice of metrics.Store the digest reads.
type MetricsReader interface {
	Since(ctx context.Context, since time.Time) ([]metrics.Sample, error)
	ListCapacity(ctx context.Context) ([]metrics.NodeCapacity, error)
	ListDisks(ctx context.Context) ([]metrics.NodeDisk, error)
}

// SwarmReader is what the digest reads from Swarm (an optional docker engine
// capability): each service's failed tasks since a time, for crash counts,
// and service states, to tell an app stopped on purpose from one that should
// be running and is not.
type SwarmReader interface {
	TaskFailuresSince(ctx context.Context, since time.Time) (map[string]int, error)
	ServiceStates(ctx context.Context, names []string) (map[string]docker.ServiceState, error)
}

// Digest builds the daily digest and schedules it per channel.
type Digest struct {
	svc     *Service
	store   Store
	metrics MetricsReader
	swarm   SwarmReader // nil: crashes and stopped apps are not told apart
	state   StateReader // nil: no "Krill state" block
	log     *slog.Logger
	now     func() time.Time
}

// NewDigest creates the digest builder and scheduler. sw may be nil.
func NewDigest(svc *Service, store Store, m MetricsReader, sw SwarmReader) *Digest {
	return &Digest{svc: svc, store: store, metrics: m, swarm: sw, log: slog.Default(), now: time.Now}
}

// Location returns the zone a channel's digest is scheduled in.
func Location(tz string) *time.Location {
	if tz == "" {
		tz = DefaultDigestTZ
	}
	if loc, err := time.LoadLocation(tz); err == nil {
		return loc
	}
	return time.UTC
}

// ValidTZ reports whether tz is an IANA zone name a channel can use.
func ValidTZ(tz string) bool {
	if tz == "" || tz == "Local" {
		return false
	}
	_, err := time.LoadLocation(tz)
	return err == nil
}

// dueAt reports whether a channel's digest is due at now, returning the
// start of the local day it belongs to: now falls in [scheduled,
// scheduled+digestLate) of today's scheduled time in the channel's zone, and
// no digest was sent yet that local day — so moving the time or the zone
// after today's digest went out does not send a second one.
func dueAt(c Channel, now time.Time) (day time.Time, ok bool) {
	loc := Location(c.DigestTZ)
	l := now.In(loc)
	day = time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
	sched := time.Date(l.Year(), l.Month(), l.Day(), c.DigestMinute/60, c.DigestMinute%60, 0, 0, loc)
	if now.Before(sched) || !now.Before(sched.Add(digestLate)) {
		return time.Time{}, false
	}
	if c.DigestLastAt != nil && !c.DigestLastAt.Before(day) {
		return time.Time{}, false
	}
	return day, true
}

// Run checks every minute for due digests until ctx is canceled, and prunes
// the backup journal once an hour.
func (d *Digest) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	n := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n%60 == 0 {
				d.prune(ctx)
			}
			n++
			d.tick(ctx)
		}
	}
}

// prune drops journal rows older than backupRunsKeep.
func (d *Digest) prune(ctx context.Context) {
	before := d.now().Add(-backupRunsKeep)
	if err := d.store.PruneBackupRuns(ctx, before); err != nil {
		d.log.Warn("notify digest: prune backup runs failed", "err", err)
	}
	if d.state != nil {
		if err := d.state.PruneStateRuns(ctx, before); err != nil {
			d.log.Warn("notify digest: prune state backup runs failed", "err", err)
		}
	}
}

func (d *Digest) tick(ctx context.Context) {
	chans, err := d.store.DigestChannels(ctx)
	if err != nil {
		d.log.Warn("notify digest: load channels failed", "err", err)
	}
	now := d.now().Truncate(time.Microsecond) // the database's precision: the claim is compared back
	for _, c := range chans {
		day, ok := dueAt(c, now)
		if !ok {
			continue
		}
		// One digest per local day: the claim fails once anything was sent
		// since the day began.
		claimed, err := d.store.ClaimDigest(ctx, c.ID, now, day)
		if err != nil {
			d.log.Warn("notify digest: claim failed", "channel", c.ID, "err", err)
			continue
		}
		if !claimed {
			continue
		}
		if err := d.send(ctx, c, now); err != nil {
			d.log.Warn("notify digest: send failed, will retry", "channel", c.ID, "org", c.OrgID, "err", err)
			if rerr := d.store.ReleaseDigest(ctx, c.ID, now, c.DigestLastAt); rerr != nil {
				d.log.Warn("notify digest: release claim failed", "channel", c.ID, "err", rerr)
			}
			continue
		}
		d.log.Info("notify digest sent", "channel", c.ID, "org", c.OrgID)
	}
}

// send builds and sends c's digest. An error means nothing reached the chat:
// once the first part is out, a later failure is logged and the digest
// counts as sent, since a retry would repeat what already arrived.
func (d *Digest) send(ctx context.Context, c Channel, now time.Time) error {
	parts, err := d.Build(ctx, c, now)
	if err != nil {
		return err
	}
	for i, p := range parts {
		if err := d.svc.send(ctx, c.BotToken, c.ChatID, p); err != nil {
			if i == 0 {
				return err
			}
			d.log.Warn("notify digest: a later part failed", "channel", c.ID, "part", i+1, "err", err)
			return nil
		}
	}
	return nil
}

// SendNow sends c's digest right away (the settings page's preview button).
func (d *Digest) SendNow(ctx context.Context, c Channel) error {
	return d.send(ctx, c, d.now())
}

// Build renders c's digest at now as one or more message parts.
func (d *Digest) Build(ctx context.Context, c Channel, now time.Time) ([]string, error) {
	data, err := d.collect(ctx, c, now)
	if err != nil {
		return nil, err
	}
	return renderDigest(data), nil
}

// digestData is everything one digest says, collected for one channel.
type digestData struct {
	Now      time.Time
	Loc      *time.Location
	Lang     string
	Instance string
	Deploys  []DeploymentInfo
	Runs     []BackupRun
	Apps     []appRow
	idleApps
	Infra bool
	Nodes []nodeRow
	// State is the "Krill state" block (infra channels only); nil = none.
	State *stateBlock
	// Thresholds for the attention line.
	DiskThreshold, MemThreshold int
	RestartsKnown               bool
}

// idleApps are the apps without fresh samples (and no crashes), by what
// Swarm says about them.
type idleApps struct {
	Down    []string // should be running (desired > 0) and run nothing
	Stopped []string // stopped on purpose (0 replicas) or never deployed
	NoData  []string // running per Swarm but not sampled, or Swarm unreadable
}

type appRow struct {
	Path     string
	Now      int64 // bytes, summed over replicas
	Peak     int64 // highest single container in the window
	Limit    int64 // per container; 0 = no limit
	Restarts int
}

type nodeRow struct {
	Name      string
	Disks     []metrics.DiskUsage
	MemUsed   int64
	MemTotal  int64
	Limits    int64
	Unlimited int
	NCPU      int
	CPUAvg    float64
	CPUPeak   float64
	CPUKnown  bool
	Stale     bool
}

func (d *Digest) collect(ctx context.Context, c Channel, now time.Time) (digestData, error) {
	since := now.Add(-digestWindow)
	// Deploys and backups since the previous digest, when that one went out
	// late (a restart, a retried send) — otherwise the hours between its
	// scheduled and actual time would fall between two digests. Bounded, so
	// a long pause does not dump a week of history.
	eventsSince := since
	if c.DigestLastAt != nil && c.DigestLastAt.Before(since) && c.DigestLastAt.After(now.Add(-digestMaxWindow)) {
		eventsSince = *c.DigestLastAt
	}
	data := digestData{
		Now: now, Loc: Location(c.DigestTZ), Lang: c.Lang, Instance: d.svc.instance(ctx),
		Infra: c.NotifyInfra, DiskThreshold: c.DiskThresholdPct, MemThreshold: c.MemThresholdPct,
	}
	if data.DiskThreshold <= 0 {
		data.DiskThreshold = defaultDiskThreshold
	}
	if data.MemThreshold <= 0 {
		data.MemThreshold = defaultMemThreshold
	}
	deps, err := d.store.OrgDeploymentsSince(ctx, c.OrgID, eventsSince)
	if err != nil {
		return data, fmt.Errorf("deployments: %w", err)
	}
	for _, dep := range deps {
		if dep.Trigger != triggerSystem && dep.Status != "running" {
			data.Deploys = append(data.Deploys, dep)
		}
	}
	if data.Runs, err = d.store.BackupRunsSince(ctx, c.OrgID, eventsSince); err != nil {
		return data, fmt.Errorf("backup runs: %w", err)
	}
	apps, err := d.store.OrgApps(ctx, c.OrgID)
	if err != nil {
		return data, fmt.Errorf("apps: %w", err)
	}
	samples, err := d.metrics.Since(ctx, since)
	if err != nil {
		return data, fmt.Errorf("metric samples: %w", err)
	}
	caps, err := d.metrics.ListCapacity(ctx)
	if err != nil {
		return data, fmt.Errorf("node capacity: %w", err)
	}
	memTotal := map[string]int64{}
	for _, cp := range caps {
		memTotal[cp.Node] = cp.MemTotal
	}
	var restarts map[string]int
	var states map[string]docker.ServiceState
	if d.swarm != nil {
		if restarts, err = d.swarm.TaskFailuresSince(ctx, since); err != nil {
			d.log.Warn("notify digest: task failures unavailable", "err", err)
			restarts = nil
		}
		names := make([]string, len(apps))
		for i, a := range apps {
			names[i] = docker.ServiceName(a.AppID)
		}
		if states, err = d.swarm.ServiceStates(ctx, names); err != nil {
			d.log.Warn("notify digest: service states unavailable", "err", err)
			states = nil
		}
	}
	data.RestartsKnown = restarts != nil
	data.Apps, data.idleApps = appRows(apps, samples, memTotal, restarts, states, now)
	if c.NotifyInfra {
		disks, err := d.metrics.ListDisks(ctx)
		if err != nil {
			return data, fmt.Errorf("node disks: %w", err)
		}
		data.Nodes = nodeRows(caps, disks, samples, now)
		data.State = d.stateBlock(ctx, eventsSince, now)
	}
	return data, nil
}

// limited reports whether a container's reported limit is a real limit.
func limited(limit, nodeTotal int64) bool {
	return limit > 0 && (nodeTotal <= 0 || limit < nodeTotal)
}

// appRows summarizes each app's memory and crashes; an app with no fresh
// sample is sorted into idle by its Swarm state (states nil: unknown).
func appRows(apps []WatchedApp, samples []metrics.Sample, memTotal map[string]int64, restarts map[string]int, states map[string]docker.ServiceState, now time.Time) (rows []appRow, idle idleApps) {
	byComp := map[string][]metrics.Sample{}
	for _, s := range samples {
		byComp[s.Component] = append(byComp[s.Component], s)
	}
	for _, a := range apps {
		comp := docker.ServiceName(a.AppID)
		path := targetPath(a.Project, a.Env, a.Name)
		ss := byComp[comp]
		latest := map[string]metrics.Sample{} // node → latest sample
		var row appRow
		row.Path = path
		row.Restarts = restarts[comp]
		for _, s := range ss {
			if s.MemBytes > row.Peak {
				row.Peak = s.MemBytes
			}
			if l, ok := latest[s.Node]; !ok || s.TS.After(l.TS) {
				latest[s.Node] = s
			}
		}
		// The limit comes from the newest sample even when it is old: a
		// crash-looping app keeps its row, and its peak against the limit is
		// usually the reason it crashes.
		var newest, newestAny time.Time
		for _, s := range latest {
			if s.TS.After(newestAny) {
				newestAny = s.TS
				row.Limit = 0
				if limited(s.MemLimitBytes, memTotal[s.Node]) {
					row.Limit = s.MemLimitBytes
				}
			}
			if now.Sub(s.TS) > staleAfter {
				continue
			}
			row.Now += s.MemBytes
			if s.TS.After(newest) {
				newest = s.TS
			}
		}
		if newest.IsZero() {
			st, known := states[comp]
			switch {
			case row.Restarts > 0:
				rows = append(rows, row) // crash-looping: worth showing
			case states == nil:
				idle.NoData = append(idle.NoData, path)
			case known && st.Found && st.Running > 0:
				idle.NoData = append(idle.NoData, path) // its node's metrics are missing
			case known && st.Found && st.Desired > 0:
				idle.Down = append(idle.Down, path)
			default:
				idle.Stopped = append(idle.Stopped, path)
			}
			continue
		}
		rows = append(rows, row)
	}
	return rows, idle
}

func nodeRows(caps []metrics.NodeCapacity, disks []metrics.NodeDisk, samples []metrics.Sample, now time.Time) []nodeRow {
	byNode := map[string][]metrics.Sample{}
	for _, s := range samples {
		byNode[s.Node] = append(byNode[s.Node], s)
	}
	disksBy := map[string][]metrics.DiskUsage{}
	for _, dk := range disks {
		disksBy[dk.Node] = append(disksBy[dk.Node], dk.DiskUsage)
	}
	sort.Slice(caps, func(i, j int) bool { return caps[i].Node < caps[j].Node })
	rows := make([]nodeRow, 0, len(caps))
	for _, cp := range caps {
		r := nodeRow{Name: cp.Node, MemTotal: cp.MemTotal, NCPU: cp.NCPU, Disks: disksBy[cp.Node]}
		ticks := splitTicks(byNode[cp.Node])
		// The sampler refreshes a node's capacity on every tick it reaches the
		// node, containers or not, so an old capacity row is what tells a node
		// that stopped reporting (a node with no containers has no samples).
		r.Stale = now.Sub(cp.SampledAt) > staleAfter
		if len(ticks) > 0 && now.Sub(ticks[len(ticks)-1][0].TS) <= staleAfter {
			for _, s := range ticks[len(ticks)-1] {
				r.MemUsed += s.MemBytes
				if limited(s.MemLimitBytes, cp.MemTotal) {
					r.Limits += s.MemLimitBytes
				} else {
					r.Unlimited++
				}
			}
			if cp.NCPU > 0 {
				var sum float64
				for _, t := range ticks {
					var cpu float64
					for _, s := range t {
						cpu += s.CPUPct
					}
					cpu /= float64(cp.NCPU)
					sum += cpu
					if cpu > r.CPUPeak {
						r.CPUPeak = cpu
					}
				}
				r.CPUAvg = sum / float64(len(ticks))
				r.CPUKnown = true
			}
		}
		rows = append(rows, r)
	}
	return rows
}

// splitTicks groups one node's samples into sampler ticks, oldest first.
func splitTicks(ss []metrics.Sample) [][]metrics.Sample {
	if len(ss) == 0 {
		return nil
	}
	ss = append([]metrics.Sample(nil), ss...)
	sort.Slice(ss, func(i, j int) bool { return ss[i].TS.Before(ss[j].TS) })
	var out [][]metrics.Sample
	start := 0
	for i := 1; i <= len(ss); i++ {
		if i == len(ss) || ss[i].TS.Sub(ss[i-1].TS) > tickGap {
			out = append(out, ss[start:i])
			start = i
		}
	}
	return out
}

// pct is a/b in percent (0 when b is not positive).
func pct(a, b int64) float64 {
	if b <= 0 {
		return 0
	}
	return float64(a) * 100 / float64(b)
}

// level is a traffic-light status: green, yellow (worth a look), red.
type level int

const (
	lvlOK level = iota
	lvlWarn
	lvlCrit
)

func (l level) mark() string {
	switch l {
	case lvlCrit:
		return "🔴"
	case lvlWarn:
		return "🟡"
	}
	return "🟢"
}

func worst(ls ...level) level {
	w := lvlOK
	for _, l := range ls {
		if l > w {
			w = l
		}
	}
	return w
}

// grade places v against a yellow and a red mark. v is rounded the way the
// digest prints it (%.0f), so a figure shown as 80% is never green.
func grade(v, warn, crit float64) level {
	v = math.RoundToEven(v)
	switch {
	case v >= crit:
		return lvlCrit
	case v >= warn:
		return lvlWarn
	}
	return lvlOK
}

func (a appRow) memLevel() level {
	if a.Limit <= 0 {
		return lvlOK // no limit: nothing to run into
	}
	return grade(pct(a.Peak, a.Limit), appMemWarn, appMemCrit)
}

func (a appRow) level() level {
	l := a.memLevel()
	if a.Restarts > 0 {
		l = lvlCrit
	}
	return l
}

func (d digestData) diskLevel(dk metrics.DiskUsage) level {
	t := float64(d.DiskThreshold)
	return grade(dk.UsedPct(), t-diskWarnBand, t)
}

func (d digestData) limitsLevel(n nodeRow) level {
	if n.MemTotal <= 0 {
		return lvlOK
	}
	t := float64(d.MemThreshold)
	return grade(pct(n.Limits, n.MemTotal), math.Min(limitsWarn, t), t)
}

func memUsedLevel(n nodeRow) level {
	if n.MemTotal <= 0 {
		return lvlOK
	}
	return grade(pct(n.MemUsed, n.MemTotal), nodeMemWarn, nodeMemCrit)
}

func cpuLevel(n nodeRow) level {
	if !n.CPUKnown {
		return lvlOK
	}
	return grade(n.CPUAvg, cpuWarn, cpuCrit)
}

// nodeLevel is the worst of a node's figures. A node that stopped reporting
// is red, whatever its last figures said.
func (d digestData) nodeLevel(n nodeRow) level {
	if n.Stale {
		return lvlCrit
	}
	l := lvlOK
	for _, f := range d.nodeFigures(n) {
		l = worst(l, f.lvl)
	}
	return l
}

// figure is one graded figure of a node, with the short i18n phrase that
// names it in the node's heading.
type figure struct {
	lvl  level
	key  string
	args []any
}

// nodeFigures grades every figure of a reporting node, in the order its
// lines are printed.
func (d digestData) nodeFigures(n nodeRow) []figure {
	var fs []figure
	for _, dk := range n.Disks {
		fs = append(fs, figure{d.diskLevel(dk), "tg.dg.why_disk", []any{dk.Mount, dk.UsedPct()}})
	}
	if len(n.Disks) == 0 {
		fs = append(fs, figure{lvlWarn, "tg.dg.why_disk_unknown", nil}) // the snapshot failed: cannot tell
	}
	fs = append(fs,
		figure{d.limitsLevel(n), "tg.dg.why_limits", []any{pct(n.Limits, n.MemTotal)}},
		figure{memUsedLevel(n), "tg.dg.why_mem", []any{pct(n.MemUsed, n.MemTotal)}},
		figure{cpuLevel(n), "tg.dg.why_cpu", []any{n.CPUAvg}},
	)
	return fs
}

// nodeWhyMax caps the reasons in a node's heading; the rest are counted.
const nodeWhyMax = 2

// nodeWhy names what made a yellow or red node that colour: the figures at
// its level, so a red heading over a green disk line is not read as the
// disk's. Empty for a green or a stale node (that one says so itself).
func (d digestData) nodeWhy(ctx context.Context, n nodeRow) string {
	l := d.nodeLevel(n)
	if n.Stale || l == lvlOK {
		return ""
	}
	var why []string
	for _, f := range d.nodeFigures(n) {
		if f.lvl == l {
			why = append(why, i18n.Tf(ctx, f.key, f.args...))
		}
	}
	if len(why) > nodeWhyMax {
		why = append(why[:nodeWhyMax], i18n.Tf(ctx, "tg.dg.why_more", len(why)-nodeWhyMax))
	}
	return strings.Join(why, ", ")
}

type attItem struct {
	lvl  level
	text string
}

// attention lists every yellow and red figure of the digest, red first.
func attention(ctx context.Context, d digestData) []attItem {
	var out []attItem
	add := func(l level, key string, args ...any) {
		if l > lvlOK {
			out = append(out, attItem{l, i18n.Tf(ctx, key, args...)})
		}
	}
	for _, dep := range d.Deploys {
		if dep.Status == "error" {
			add(lvlCrit, "tg.dg.att_deploy", targetPath(dep.Project, dep.Env, dep.App))
		}
	}
	for _, r := range d.Runs {
		if r.Status == "error" {
			add(lvlCrit, "tg.dg.att_backup", kindLabel(ctx, r.Kind), targetPath(r.Project, r.Env, r.Target))
		}
	}
	for _, a := range d.Apps {
		add(a.memLevel(), "tg.dg.att_app_mem", a.Path, pct(a.Peak, a.Limit))
		if a.Restarts > 0 {
			add(lvlCrit, "tg.dg.att_app_restarts", a.Path, a.Restarts)
		}
	}
	for _, p := range d.Down {
		add(lvlCrit, "tg.dg.att_app_down", p)
	}
	for _, n := range d.Nodes {
		if n.Stale {
			add(lvlCrit, "tg.dg.att_node_stale", n.Name)
			continue // its last figures are old
		}
		for _, dk := range n.Disks {
			add(d.diskLevel(dk), "tg.dg.att_disk", n.Name, dk.Mount, dk.UsedPct())
		}
		if len(n.Disks) == 0 {
			add(lvlWarn, "tg.dg.att_disk_unknown", n.Name)
		}
		add(d.limitsLevel(n), "tg.dg.att_limits", n.Name, pct(n.Limits, n.MemTotal))
		add(memUsedLevel(n), "tg.dg.att_mem", n.Name, pct(n.MemUsed, n.MemTotal))
		add(cpuLevel(n), "tg.dg.att_cpu", n.Name, n.CPUAvg)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].lvl > out[j].lvl })
	return out
}

// counts is a section's "ok / failed" summary, or "none" with no runs.
func counts(ctx context.Context, ok, failed int) string {
	if ok+failed == 0 {
		return i18n.T(ctx, "tg.dg.none")
	}
	return i18n.Tf(ctx, "tg.dg.counts", ok, failed)
}

func kindLabel(ctx context.Context, kind string) string {
	return i18n.T(ctx, "tg.kind."+kind)
}

// whenLabel formats a time in the digest's zone, short for today's.
func whenLabel(t, now time.Time, loc *time.Location, lang string) string {
	l, n := t.In(loc), now.In(loc)
	if l.Year() == n.Year() && l.YearDay() == n.YearDay() {
		return l.Format("15:04")
	}
	if lang == "ru" {
		return l.Format("02.01 15:04")
	}
	return l.Format("Jan 2 15:04")
}

// renderDigest renders the digest as Telegram HTML, split into parts at line
// boundaries (every tag opens and closes on one line).
func renderDigest(d digestData) []string {
	ctx := msgCtx(d.Lang)
	esc := html.EscapeString
	var lines []string
	add := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }

	head := i18n.T(ctx, "tg.dg.title")
	if d.Instance != "" {
		head += " · " + d.Instance
	}
	add("<b>%s</b>", esc(head))
	add("%s (%s)", esc(whenLabel(d.Now, d.Now, d.Loc, d.Lang)), esc(d.Loc.String()))
	if att := attention(ctx, d); len(att) > 0 {
		add("")
		add("%s <b>%s</b>", att[0].lvl.mark(), esc(i18n.T(ctx, "tg.dg.attention")))
		for i, a := range att {
			if i == digestListMax {
				add("%s", esc(i18n.Tf(ctx, "tg.dg.more", len(att)-i)))
				break
			}
			add("%s %s", a.lvl.mark(), esc(a.text))
		}
	} else {
		add("")
		add("%s %s", lvlOK.mark(), esc(i18n.T(ctx, "tg.dg.all_ok")))
	}
	// metric adds a figure's line, marked only when it is not green, so the
	// line behind a yellow or red node stands out.
	metric := func(l level, text string) {
		if l > lvlOK {
			add("%s %s", l.mark(), esc(text))
		} else {
			add("%s", esc(text))
		}
	}
	outcome := func(failed bool) string {
		if failed {
			return lvlCrit.mark()
		}
		return lvlOK.mark()
	}

	// Deploys.
	okN, errN := 0, 0
	for _, dep := range d.Deploys {
		if dep.Status == "error" {
			errN++
		} else {
			okN++
		}
	}
	add("")
	add("<b>%s</b>: %s", esc(i18n.T(ctx, "tg.dg.deploys")), esc(counts(ctx, okN, errN)))
	for i, dep := range d.Deploys {
		if i == digestListMax {
			add("%s", esc(i18n.Tf(ctx, "tg.dg.more", len(d.Deploys)-i)))
			break
		}
		parts := []string{targetPath(dep.Project, dep.Env, dep.App)}
		if img := shortImage(dep.ImageTag); img != "" {
			parts = append(parts, img)
		}
		parts = append(parts, whenLabel(dep.FinishedAt, d.Now, d.Loc, d.Lang))
		if dep.Status == "error" && strings.TrimSpace(dep.Error) != "" {
			parts = append(parts, oneLine(dep.Error, 120))
		}
		add("%s %s", outcome(dep.Status == "error"), esc(strings.Join(parts, " · ")))
	}

	// Backups.
	okN, errN = 0, 0
	for _, r := range d.Runs {
		if r.Status == "error" {
			errN++
		} else {
			okN++
		}
	}
	add("")
	add("<b>%s</b>: %s", esc(i18n.T(ctx, "tg.dg.backups")), esc(counts(ctx, okN, errN)))
	for i, r := range d.Runs {
		if i == digestListMax {
			add("%s", esc(i18n.Tf(ctx, "tg.dg.more", len(d.Runs)-i)))
			break
		}
		parts := []string{kindLabel(ctx, r.Kind) + " " + targetPath(r.Project, r.Env, r.Target)}
		if r.Status != "error" && r.SizeBytes > 0 {
			parts = append(parts, humanBytes(r.SizeBytes))
		}
		parts = append(parts, whenLabel(r.FinishedAt, d.Now, d.Loc, d.Lang))
		if r.Status == "error" && strings.TrimSpace(r.Error) != "" {
			parts = append(parts, oneLine(r.Error, 120))
		}
		add("%s %s", outcome(r.Status == "error"), esc(strings.Join(parts, " · ")))
	}

	// Nodes (infrastructure channels only).
	if d.Infra {
		add("")
		add("<b>%s</b>", esc(i18n.T(ctx, "tg.dg.nodes")))
		for _, n := range d.Nodes {
			name := n.Name
			if n.Stale {
				name += " — " + i18n.T(ctx, "tg.dg.stale")
			}
			if why := d.nodeWhy(ctx, n); why != "" {
				add("%s <b>%s</b> — %s", d.nodeLevel(n).mark(), esc(name), esc(why))
			} else {
				add("%s <b>%s</b>", d.nodeLevel(n).mark(), esc(name))
			}
			if n.Stale {
				continue // its last figures would read as current
			}
			for _, dk := range n.Disks {
				metric(d.diskLevel(dk), i18n.Tf(ctx, "tg.dg.node_disk", dk.Mount, dk.UsedPct(), bytesPair(ctx, "tg.pair", dk.Used, dk.Total), humanBytes(dk.Avail)))
			}
			if len(n.Disks) == 0 {
				metric(lvlWarn, i18n.T(ctx, "tg.dg.node_disk_unknown"))
			}
			mem := i18n.Tf(ctx, "tg.dg.node_mem", bytesPair(ctx, "tg.pair", n.MemUsed, n.MemTotal), humanBytes(n.Limits), pct(n.Limits, n.MemTotal))
			if n.Unlimited > 0 {
				mem += " · " + i18n.Tf(ctx, "tg.dg.unlimited", n.Unlimited)
			}
			metric(worst(d.limitsLevel(n), memUsedLevel(n)), mem)
			if n.CPUKnown {
				metric(cpuLevel(n), i18n.Tf(ctx, "tg.dg.node_cpu", n.CPUAvg, n.CPUPeak, n.NCPU))
			}
		}
		if len(d.Nodes) == 0 {
			add("%s", esc(i18n.T(ctx, "tg.dg.no_data")))
		}
	}

	// Krill's own state backups (infrastructure channels only).
	if d.Infra && d.State != nil {
		add("")
		add("<b>%s</b>", esc(i18n.T(ctx, "tg.dg.state")))
		for _, l := range stateLines(ctx, d) {
			if l.lvl == nil {
				add("%s", esc(l.text))
			} else {
				add("%s %s", l.lvl.mark(), esc(l.text))
			}
		}
	}

	// Apps.
	add("")
	add("<b>%s</b>", esc(i18n.T(ctx, "tg.dg.apps")))
	for _, a := range d.Apps {
		line := a.Path + ": " + i18n.Tf(ctx, "tg.dg.app_now", humanBytes(a.Now))
		if a.Limit > 0 {
			line += " · " + i18n.Tf(ctx, "tg.dg.app_peak_limit", bytesPair(ctx, "tg.pair_limit", a.Peak, a.Limit), pct(a.Peak, a.Limit))
		} else {
			line += " · " + i18n.Tf(ctx, "tg.dg.app_peak", humanBytes(a.Peak))
		}
		if d.RestartsKnown {
			line += " · " + i18n.Tf(ctx, "tg.dg.app_restarts", a.Restarts)
		}
		add("%s %s", a.level().mark(), esc(line))
	}
	names := func(mark, key string, list []string) {
		if len(list) == 0 {
			return
		}
		more := ""
		if len(list) > digestListMax {
			list, more = list[:digestListMax], " "+i18n.Tf(ctx, "tg.dg.more", len(list)-digestListMax)
		}
		add("%s %s", mark, esc(i18n.T(ctx, key)+": "+strings.Join(list, ", ")+more))
	}
	names(lvlCrit.mark(), "tg.dg.down", d.Down)
	names("⚪", "tg.dg.no_samples", d.NoData)
	names("⚪", "tg.dg.stopped", d.Stopped)
	if len(d.Apps) == 0 && len(d.Down) == 0 && len(d.NoData) == 0 && len(d.Stopped) == 0 {
		add("%s", esc(i18n.T(ctx, "tg.dg.no_data")))
	}
	return splitParts(lines, digestPartLimit)
}

// cutLine shortens an escaped line to at most limit runes without cutting
// inside an HTML entity (Telegram rejects the whole message then).
func cutLine(r []rune, limit int) string {
	cut := limit
	if amp := strings.LastIndex(string(r[:cut]), "&"); amp >= 0 {
		if semi := strings.IndexByte(string(r[:cut])[amp:], ';'); semi < 0 {
			cut = len([]rune(string(r[:cut])[:amp]))
		}
	}
	return string(r[:cut])
}

// oneLine flattens and shortens an error message for a digest line.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}

// splitParts joins lines into messages of at most limit characters, never
// splitting a line (a longer single line is cut).
func splitParts(lines []string, limit int) []string {
	var parts []string
	var b strings.Builder
	for _, l := range lines {
		if b.Len() == 0 && l == "" {
			continue // a part never starts with a blank line
		}
		if r := []rune(l); len(r) > limit {
			l = cutLine(r, limit)
		}
		if b.Len() > 0 && len([]rune(b.String()))+1+len([]rune(l)) > limit {
			parts = append(parts, strings.TrimRight(b.String(), "\n"))
			b.Reset()
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(l)
	}
	if strings.TrimSpace(b.String()) != "" {
		parts = append(parts, strings.TrimRight(b.String(), "\n"))
	}
	return parts
}
