package metrics

import (
	"testing"
	"time"
)

func TestBucketAvg(t *testing.T) {
	start, end := time.Unix(0, 0), time.Unix(100, 0)
	var pts []Point
	for i := 0; i < 100; i++ {
		pts = append(pts, Point{T: time.Unix(int64(i), 0), V: float64(i)})
	}
	got := BucketAvg(pts, start, end, 10)
	if len(got) != 10 {
		t.Fatalf("want 10 buckets, got %d", len(got))
	}
	if got[0] == nil || *got[0] < 4 || *got[0] > 5 { // 0..9 avg = 4.5
		t.Errorf("bucket0 = %v, want ~4.5", got[0])
	}
	gap := BucketAvg([]Point{{T: time.Unix(0, 0), V: 1}}, start, end, 10)
	if gap[5] != nil {
		t.Errorf("empty bucket should be nil, got %v", *gap[5])
	}
}

func TestGridTimes(t *testing.T) {
	x := GridTimes(time.Unix(0, 0), time.Unix(100, 0), 10)
	if len(x) != 10 {
		t.Fatalf("want 10, got %d", len(x))
	}
	if x[0] != 0 || x[1] <= x[0] {
		t.Errorf("grid not monotonic: %v", x)
	}
}

func TestClassify(t *testing.T) {
	apps := map[string]Labeled{"krill-7": {Name: "web", Detail: "proj/prod"}}
	dbs := map[string]Labeled{"demodb": {Name: "demodb", Detail: "postgres"}}
	cases := []struct {
		comp string
		self bool
		grp  string
		name string
	}{
		{"krill-self", true, GroupControl, "Krill"},
		{"krill-7", false, GroupApp, "web"},
		{"demodb", false, GroupDB, "demodb"},
		{"krill-traefik", false, GroupInfra, "Traefik"},
		{"traefik", false, GroupInfra, "traefik"}, // bare name is not the swarm service; stays raw
		{"grafana", false, GroupInfra, "grafana"},
	}
	for _, tc := range cases {
		g, n := Classify(tc.comp, tc.self, apps, dbs)
		if g != tc.grp || n != tc.name {
			t.Errorf("Classify(%q,self=%v) = (%q,%q), want (%q,%q)", tc.comp, tc.self, g, n, tc.grp, tc.name)
		}
	}
}
