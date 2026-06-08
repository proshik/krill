package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/proshik/krill/internal/metrics"
	"github.com/proshik/krill/internal/web/templates"
)

var monPalette = []string{"#bef264", "#5b9bd5", "#9ae66e", "#d9a441", "#b794f6", "#e5534b", "#4dd0e1", "#f48fb1"}

// monSeries is one component's chart series. CPU is %, Mem is MEGABYTES (chart
// y-axis); both arrays align to monData.X (nil = gap). Note the unit split vs
// monRow below, whose Mem is raw bytes.
type monSeries struct {
	Component string     `json:"component"`
	Name      string     `json:"name"`
	Group     string     `json:"group"`
	Color     string     `json:"color"`
	CPU       []*float64 `json:"cpu"`
	Mem       []*float64 `json:"mem"`
}

// monRow is one component's current snapshot for the table. Mem/MemLimit are
// raw BYTES (the client formats them); CPU is %.
type monRow struct {
	Component string  `json:"component"`
	Name      string  `json:"name"`
	Group     string  `json:"group"`
	Color     string  `json:"color"`
	CPU       float64 `json:"cpu"`
	Mem       int64   `json:"mem"`
	MemLimit  int64   `json:"mem_limit"`
}
type monHost struct {
	CPUPct     float64 `json:"cpu_pct"`
	MemUsed    int64   `json:"mem_used"`
	MemTotal   int64   `json:"mem_total"`
	Containers int     `json:"containers"`
}
type monData struct {
	Host   monHost     `json:"host"`
	X      []float64   `json:"x"`
	Series []monSeries `json:"series"`
	Rows   []monRow    `json:"rows"`
}

func (s *Server) monitoring(w http.ResponseWriter, r *http.Request) {
	o, role, ok := s.loadOrg(w, r)
	if !ok {
		return
	}
	render(w, r, http.StatusOK, templates.Monitoring(o, role))
}

func (s *Server) monitoringData(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.loadOrg(w, r); !ok {
		return
	}
	rng := parseRange(r.URL.Query().Get("range"))
	apps, dbs, selfComp := s.metricLabels(r)

	monBuckets := monBucketCount(rng, s.cfg.MetricsInterval)
	now := time.Now()
	start := now.Add(-rng)
	x := metrics.GridTimes(start, now, monBuckets)

	samples, _ := s.metrics.Since(r.Context(), start)
	cpuPts := map[string][]metrics.Point{}
	memPts := map[string][]metrics.Point{}
	for _, sm := range samples {
		cpuPts[sm.Component] = append(cpuPts[sm.Component], metrics.Point{T: sm.TS, V: sm.CPUPct})
		memPts[sm.Component] = append(memPts[sm.Component], metrics.Point{T: sm.TS, V: float64(sm.MemBytes) / (1 << 20)})
	}
	comps := make([]string, 0, len(cpuPts))
	for c := range cpuPts {
		comps = append(comps, c)
	}
	sort.Strings(comps)
	colorByComp := map[string]string{}
	series := make([]monSeries, 0, len(comps))
	for i, c := range comps {
		grp, name := metrics.Classify(c, c == selfComp, apps, dbs)
		colorByComp[c] = monPalette[i%len(monPalette)]
		series = append(series, monSeries{
			Component: c, Name: name, Group: grp, Color: colorByComp[c],
			CPU: metrics.BucketAvg(cpuPts[c], start, now, monBuckets),
			Mem: metrics.BucketAvg(memPts[c], start, now, monBuckets),
		})
	}

	// "Latest" only counts components that reported recently, so containers that
	// died don't linger in the snapshot for the whole retention window.
	freshness := 3 * s.cfg.MetricsInterval
	if freshness < 2*time.Minute {
		freshness = 2 * time.Minute
	}
	latest, _ := s.metrics.Latest(r.Context(), now.Add(-freshness))
	rows := make([]monRow, 0, len(latest))
	var sumCPU float64
	var sumMem int64
	for _, sm := range latest {
		grp, name := metrics.Classify(sm.Component, sm.Component == selfComp, apps, dbs)
		rows = append(rows, monRow{
			Component: sm.Component, Name: name, Group: grp,
			Color: colorByComp[sm.Component], CPU: sm.CPUPct, Mem: sm.MemBytes, MemLimit: sm.MemLimitBytes,
		})
		sumCPU += sm.CPUPct
		sumMem += sm.MemBytes
	}
	sortRows(rows)
	host := monHost{MemUsed: sumMem, Containers: len(latest)}
	if ni, err := s.engine.NodeInfo(r.Context()); err == nil {
		host.MemTotal = ni.MemTotal
		if ni.NCPU > 0 {
			host.CPUPct = sumCPU / float64(ni.NCPU)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(monData{Host: host, X: x, Series: series, Rows: rows})
}

// monBucketCount picks how many time buckets to render for a range so each
// bucket spans at least ~2 sample intervals. With finer buckets, samples would
// land in non-adjacent buckets (nil gaps between them) and the line — drawn only
// between consecutive non-null points, with points hidden — would be invisible.
// Capped at 240 buckets for chart resolution and floored at 12 for tiny ranges.
func monBucketCount(rng, interval time.Duration) int {
	n := 240
	if interval > 0 {
		if m := int(rng / (2 * interval)); m < n {
			n = m
		}
	}
	if n < 12 {
		n = 12
	}
	return n
}

func parseRange(s string) time.Duration {
	switch s {
	case "1h":
		return time.Hour
	case "6h":
		return 6 * time.Hour
	default:
		return 24 * time.Hour
	}
}

var monGroupOrder = map[string]int{
	metrics.GroupControl: 0,
	metrics.GroupInfra:   1,
	metrics.GroupApp:     2,
	metrics.GroupDB:      3,
}

func sortRows(rows []monRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		if monGroupOrder[rows[i].Group] != monGroupOrder[rows[j].Group] {
			return monGroupOrder[rows[i].Group] < monGroupOrder[rows[j].Group]
		}
		return rows[i].Name < rows[j].Name
	})
}

// metricLabels builds component-key -> display-name maps for apps and managed
// DBs, plus the control-plane component key. Monitoring is an admin-only,
// host-wide operator view, so these lookups are intentionally cross-org (all
// apps/DBs on the node), used purely to label node-wide container stats.
func (s *Server) metricLabels(r *http.Request) (apps, dbs map[string]metrics.Labeled, selfComp string) {
	apps, dbs = map[string]metrics.Labeled{}, map[string]metrics.Labeled{}
	if rows, err := s.q.ListWatchedApps(r.Context()); err == nil {
		for _, a := range rows {
			apps[dockerName(a.AppID)] = metrics.Labeled{Name: a.AppName, Detail: a.ProjectName + "/" + a.EnvName}
		}
	}
	if pgs, err := s.q.ListAllPostgres(r.Context()); err == nil {
		for _, p := range pgs {
			dbs[p.AppName] = metrics.Labeled{Name: p.Name, Detail: "postgres"}
		}
	}
	if rds, err := s.q.ListAllRedis(r.Context()); err == nil {
		for _, d := range rds {
			dbs[d.AppName] = metrics.Labeled{Name: d.Name, Detail: "redis"}
		}
	}
	return apps, dbs, s.selfComponent(r.Context())
}

// selfComponent returns the component key of Krill's own container, detected via
// a live stats scan and cached for the process lifetime (the container id is
// stable). It is resolved at most once on success; an empty result is cached too
// (Krill running outside a container is never the control-plane container). Only
// an engine error leaves it unresolved so a later request can retry.
func (s *Server) selfComponent(ctx context.Context) string {
	s.selfMu.Lock()
	defer s.selfMu.Unlock()
	if s.selfResolved {
		return s.selfComp
	}
	stats, err := s.engine.ListContainerStats(ctx)
	if err != nil {
		return ""
	}
	for _, st := range stats {
		if st.SelfControl {
			s.selfComp = st.Component
			break
		}
	}
	s.selfResolved = true
	return s.selfComp
}
