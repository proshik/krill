package metrics

import "time"

const (
	GroupControl = "control"
	GroupInfra   = "infra"
	GroupApp     = "app"
	GroupDB      = "db"
)

// Point is one (time, value) sample for a chart series.
type Point struct {
	T time.Time
	V float64
}

// Labeled is a display name + detail for a known component.
type Labeled struct {
	Name   string
	Detail string
}

// GridTimes returns n bucket-start timestamps (unix seconds) spanning [start,end]
// — the shared x-axis for all component series (uPlot needs one x + parallel y).
func GridTimes(start, end time.Time, n int) []float64 {
	out := make([]float64, n)
	if n <= 0 {
		return out
	}
	span := end.Sub(start).Seconds()
	for i := 0; i < n; i++ {
		out[i] = float64(start.Unix()) + span*float64(i)/float64(n)
	}
	return out
}

// BucketAvg averages pts into n equal buckets over the FIXED range [start,end]
// (aligned to GridTimes). Returns length n; nil where a bucket had no points
// (rendered as a gap). The fixed range keeps every component aligned to the
// same x, including partial-history ones.
func BucketAvg(pts []Point, start, end time.Time, n int) []*float64 {
	out := make([]*float64, n)
	if n <= 0 {
		return out
	}
	span := end.UnixNano() - start.UnixNano()
	if span <= 0 {
		return out
	}
	sum := make([]float64, n)
	cnt := make([]int, n)
	for _, p := range pts {
		idx := int((p.T.UnixNano() - start.UnixNano()) * int64(n) / span)
		if idx < 0 || idx >= n {
			continue
		}
		sum[idx] += p.V
		cnt[idx]++
	}
	for i := range out {
		if cnt[i] > 0 {
			v := sum[i] / float64(cnt[i])
			out[i] = &v
		}
	}
	return out
}

// Classify maps a component key (== swarm service name, or container name) to a
// display group and name using the known app/db lookups.
func Classify(component string, self bool, apps, dbs map[string]Labeled) (group, name string) {
	switch {
	case self:
		return GroupControl, "Krill"
	case component == "krill-traefik":
		return GroupInfra, "Traefik"
	}
	if l, ok := apps[component]; ok {
		return GroupApp, l.Name
	}
	if l, ok := dbs[component]; ok {
		return GroupDB, l.Name
	}
	return GroupInfra, component
}
