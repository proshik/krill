package metrics_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/metrics"
)

type fakeSrc struct {
	stats []docker.ContainerStat
	cap   docker.NodeInfo
	err   error
}

func (f fakeSrc) ListContainerStats(ctx context.Context) ([]docker.ContainerStat, error) {
	return f.stats, f.err
}
func (f fakeSrc) NodeInfo(ctx context.Context) (docker.NodeInfo, error) { return f.cap, f.err }

func TestSampleAllLocalPlusWorkers(t *testing.T) {
	local := fakeSrc{stats: []docker.ContainerStat{{Component: "krill", CPUPct: 5}}, cap: docker.NodeInfo{NCPU: 2, MemTotal: 100}}
	workerOK := fakeSrc{stats: []docker.ContainerStat{{Component: "app", CPUPct: 9}}, cap: docker.NodeInfo{NCPU: 4, MemTotal: 200}}
	workerBad := fakeSrc{err: errors.New("ssh down")}

	var closed atomic.Int32
	workers := func(ctx context.Context) ([]metrics.Worker, error) {
		mk := func(s fakeSrc) func(context.Context) (metrics.NodeStatsSource, func() error, error) {
			return func(context.Context) (metrics.NodeStatsSource, func() error, error) {
				return s, func() error { closed.Add(1); return nil }, nil
			}
		}
		return []metrics.Worker{
			{Name: "w-ok", Connect: mk(workerOK)},
			{Name: "w-bad", Connect: mk(workerBad)},
		}, nil
	}

	cs := metrics.NewClusterSource("cp", local, workers, time.Second)
	samples := cs.SampleAll(context.Background())

	byNode := map[string]metrics.NodeSample{}
	for _, s := range samples {
		byNode[s.Node] = s
	}
	if !byNode["cp"].OK || len(byNode["cp"].Containers) != 1 {
		t.Fatalf("control-plane sample wrong: %+v", byNode["cp"])
	}
	if !byNode["w-ok"].OK || byNode["w-ok"].Capacity.NCPU != 4 {
		t.Fatalf("worker w-ok wrong: %+v", byNode["w-ok"])
	}
	if byNode["w-bad"].OK {
		t.Fatalf("failing worker must be OK=false, got %+v", byNode["w-bad"])
	}
	if closed.Load() != 2 {
		t.Fatalf("both worker connections must be closed, closed=%d", closed.Load())
	}
}
