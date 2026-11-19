package notify

import (
	"context"
	"fmt"
	"html"
	"log/slog"
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
	// memNearLimit is the share of an app's memory limit, and of a node's
	// memory, past which the digest asks for attention.
	memNearLimit = 90.0
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

// TaskFailureCounter counts each Swarm service's failed tasks since a time
// (an optional docker engine capability).
type TaskFailureCounter interface {
	TaskFailuresSince(ctx context.Context, since time.Time) (map[string]int, error)
}

// Digest builds the daily digest and schedules it per channel.
type Digest struct {
	svc     *Service
	store   Store
	metrics MetricsReader
	tasks   TaskFailureCounter // nil: restarts are not reported
	log     *slog.Logger
	now     func() time.Time
}

// NewDigest creates the digest builder and scheduler. tasks may be nil.
func NewDigest(svc *Service, store Store, m MetricsReader, tasks TaskFailureCounter) *Digest {
	return &Digest{svc: svc, store: store, metrics: m, tasks: tasks, log: slog.Default(), now: time.Now}
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
				if err := d.store.PruneBackupRuns(ctx, d.now().Add(-backupRunsKeep)); err != nil {
					d.log.Warn("notify digest: prune backup runs failed", "err", err)
				}
			}
			n++
			d.tick(ctx)
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
	Stopped  []string
	Infra    bool
	Nodes    []nodeRow
	// Thresholds for the attention line.
	DiskThreshold, MemThreshold int
	RestartsKnown               bool
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
	if d.tasks != nil {
		if restarts, err = d.tasks.TaskFailuresSince(ctx, since); err != nil {
			d.log.Warn("notify digest: task failures unavailable", "err", err)
			restarts = nil
		}
	}
	data.RestartsKnown = restarts != nil
	data.Apps, data.Stopped = appRows(apps, samples, memTotal, restarts, now)
	if c.NotifyInfra {
		disks, err := d.metrics.ListDisks(ctx)
		if err != nil {
			return data, fmt.Errorf("node disks: %w", err)
		}
		data.Nodes = nodeRows(caps, disks, samples, now)
	}
	return data, nil
}

// limited reports whether a container's reported limit is a real limit.
func limited(limit, nodeTotal int64) bool {
	return limit > 0 && (nodeTotal <= 0 || limit < nodeTotal)
}

func appRows(apps []WatchedApp, samples []metrics.Sample, memTotal map[string]int64, restarts map[string]int, now time.Time) (rows []appRow, stopped []string) {
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
		var newest time.Time
		for _, s := range latest {
			if now.Sub(s.TS) > staleAfter {
				continue
			}
			row.Now += s.MemBytes
			if s.TS.After(newest) {
				newest = s.TS
				row.Limit = 0
				if limited(s.MemLimitBytes, memTotal[s.Node]) {
					row.Limit = s.MemLimitBytes
				}
			}
		}
		if newest.IsZero() {
			if row.Restarts > 0 {
				rows = append(rows, row) // crash-looping: worth showing
			} else {
				stopped = append(stopped, path)
			}
			continue
		}
		rows = append(rows, row)
	}
	return rows, stopped
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

// attention lists what in the digest is at or near a threshold.
func attention(ctx context.Context, d digestData) []string {
	var out []string
	for _, dep := range d.Deploys {
		if dep.Status == "error" {
			out = append(out, i18n.Tf(ctx, "tg.dg.att_deploy", targetPath(dep.Project, dep.Env, dep.App)))
		}
	}
	for _, r := range d.Runs {
		if r.Status == "error" {
			out = append(out, i18n.Tf(ctx, "tg.dg.att_backup", kindLabel(ctx, r.Kind), targetPath(r.Project, r.Env, r.Target)))
		}
	}
	for _, a := range d.Apps {
		if a.Limit > 0 && pct(a.Peak, a.Limit) >= memNearLimit {
			out = append(out, i18n.Tf(ctx, "tg.dg.att_app_mem", a.Path, pct(a.Peak, a.Limit)))
		}
		if a.Restarts > 0 {
			out = append(out, i18n.Tf(ctx, "tg.dg.att_app_restarts", a.Path, a.Restarts))
		}
	}
	for _, n := range d.Nodes {
		if n.Stale {
			out = append(out, i18n.Tf(ctx, "tg.dg.att_node_stale", n.Name))
		}
		for _, dk := range n.Disks {
			if p := dk.UsedPct(); p >= float64(d.DiskThreshold) {
				out = append(out, i18n.Tf(ctx, "tg.dg.att_disk", n.Name, dk.Mount, p))
			}
		}
		if n.MemTotal > 0 {
			if p := pct(n.Limits, n.MemTotal); p >= float64(d.MemThreshold) {
				out = append(out, i18n.Tf(ctx, "tg.dg.att_limits", n.Name, p))
			}
			if p := pct(n.MemUsed, n.MemTotal); p >= memNearLimit {
				out = append(out, i18n.Tf(ctx, "tg.dg.att_mem", n.Name, p))
			}
		}
	}
	return out
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
		add("⚠️ <b>%s</b>", esc(i18n.T(ctx, "tg.dg.attention")))
		for i, a := range att {
			if i == digestListMax {
				add("%s", esc(i18n.Tf(ctx, "tg.dg.more", len(att)-i)))
				break
			}
			add("• %s", esc(a))
		}
	} else {
		add("")
		add("✅ %s", esc(i18n.T(ctx, "tg.dg.all_ok")))
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
	add("<b>%s</b>: %s", esc(i18n.T(ctx, "tg.dg.deploys")), esc(i18n.Tf(ctx, "tg.dg.counts", okN, errN)))
	for i, dep := range d.Deploys {
		if i == digestListMax {
			add("%s", esc(i18n.Tf(ctx, "tg.dg.more", len(d.Deploys)-i)))
			break
		}
		mark, parts := "✅", []string{targetPath(dep.Project, dep.Env, dep.App)}
		if dep.Status == "error" {
			mark = "❌"
		}
		if img := shortImage(dep.ImageTag); img != "" {
			parts = append(parts, img)
		}
		parts = append(parts, whenLabel(dep.FinishedAt, d.Now, d.Loc, d.Lang))
		if dep.Status == "error" && strings.TrimSpace(dep.Error) != "" {
			parts = append(parts, oneLine(dep.Error, 120))
		}
		add("%s %s", mark, esc(strings.Join(parts, " · ")))
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
	add("<b>%s</b>: %s", esc(i18n.T(ctx, "tg.dg.backups")), esc(i18n.Tf(ctx, "tg.dg.counts", okN, errN)))
	for i, r := range d.Runs {
		if i == digestListMax {
			add("%s", esc(i18n.Tf(ctx, "tg.dg.more", len(d.Runs)-i)))
			break
		}
		mark, parts := "✅", []string{kindLabel(ctx, r.Kind) + " " + targetPath(r.Project, r.Env, r.Target)}
		if r.Status == "error" {
			mark = "❌"
		} else if r.SizeBytes > 0 {
			parts = append(parts, humanBytes(r.SizeBytes))
		}
		parts = append(parts, whenLabel(r.FinishedAt, d.Now, d.Loc, d.Lang))
		if r.Status == "error" && strings.TrimSpace(r.Error) != "" {
			parts = append(parts, oneLine(r.Error, 120))
		}
		add("%s %s", mark, esc(strings.Join(parts, " · ")))
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
			add("<b>%s</b>", esc(name))
			for _, dk := range n.Disks {
				add("%s", esc(i18n.Tf(ctx, "tg.dg.node_disk", dk.Mount, dk.UsedPct(), humanBytes(dk.Used), humanBytes(dk.Total))))
			}
			if len(n.Disks) == 0 {
				add("%s", esc(i18n.T(ctx, "tg.dg.node_disk_unknown")))
			}
			mem := i18n.Tf(ctx, "tg.dg.node_mem", humanBytes(n.MemUsed), humanBytes(n.MemTotal), humanBytes(n.Limits), pct(n.Limits, n.MemTotal))
			if n.Unlimited > 0 {
				mem += " · " + i18n.Tf(ctx, "tg.dg.unlimited", n.Unlimited)
			}
			add("%s", esc(mem))
			if n.CPUKnown {
				add("%s", esc(i18n.Tf(ctx, "tg.dg.node_cpu", n.CPUAvg, n.CPUPeak, n.NCPU)))
			}
		}
		if len(d.Nodes) == 0 {
			add("%s", esc(i18n.T(ctx, "tg.dg.no_data")))
		}
	}

	// Apps.
	add("")
	add("<b>%s</b>", esc(i18n.T(ctx, "tg.dg.apps")))
	for _, a := range d.Apps {
		line := a.Path + ": " + humanBytes(a.Now)
		if a.Limit > 0 {
			line += " · " + i18n.Tf(ctx, "tg.dg.app_peak_limit", humanBytes(a.Peak), humanBytes(a.Limit), pct(a.Peak, a.Limit))
		} else {
			line += " · " + i18n.Tf(ctx, "tg.dg.app_peak", humanBytes(a.Peak))
		}
		if d.RestartsKnown {
			line += " · " + i18n.Tf(ctx, "tg.dg.app_restarts", a.Restarts)
		}
		add("%s", esc(line))
	}
	if len(d.Stopped) > 0 {
		names := d.Stopped
		more := ""
		if len(names) > digestListMax {
			names, more = names[:digestListMax], " "+i18n.Tf(ctx, "tg.dg.more", len(d.Stopped)-digestListMax)
		}
		add("%s", esc(i18n.T(ctx, "tg.dg.stopped")+": "+strings.Join(names, ", ")+more))
	}
	if len(d.Apps) == 0 && len(d.Stopped) == 0 {
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
