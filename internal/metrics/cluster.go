package metrics

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/proshik/krill/internal/docker"
)

// NodeStatsSource collects stats + capacity from one node's docker daemon.
type NodeStatsSource interface {
	ListContainerStats(ctx context.Context) ([]docker.ContainerStat, error)
	NodeInfo(ctx context.Context) (docker.NodeInfo, error)
}

// Worker is one worker node to sample. Connect opens a fresh stats source
// (e.g. an SSH-tunnelled docker client) and returns it plus a cleanup func.
type Worker struct {
	Name    string
	Connect func(ctx context.Context) (src NodeStatsSource, closer func() error, err error)
}

// WorkerLister returns the workers to sample this tick.
type WorkerLister func(ctx context.Context) ([]Worker, error)

// NodeSample is one node's collected stats + capacity for a tick.
type NodeSample struct {
	Node       string
	Containers []docker.ContainerStat
	Capacity   docker.NodeInfo
	OK         bool
}

// ClusterSource samples the control-plane node plus every worker each tick.
type ClusterSource struct {
	localName string
	local     NodeStatsSource
	workers   WorkerLister
	timeout   time.Duration
	log       *slog.Logger
}

// NewClusterSource creates a ClusterSource. If timeout <= 0, defaults to 10s.
func NewClusterSource(localName string, local NodeStatsSource, workers WorkerLister, timeout time.Duration) *ClusterSource {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &ClusterSource{localName: localName, local: local, workers: workers, timeout: timeout, log: slog.Default()}
}

// SampleAll samples the local node first, then all workers concurrently.
// Each worker runs under its own per-node timeout; a failing worker yields
// NodeSample{Node, OK:false} and never aborts others.
func (c *ClusterSource) SampleAll(ctx context.Context) []NodeSample {
	out := []NodeSample{c.sampleOne(ctx, c.localName, c.local)}

	workers, err := c.workers(ctx)
	if err != nil {
		c.log.Warn("metrics: list workers failed", "err", err)
		return out
	}

	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, w := range workers {
		wg.Add(1)
		go func(w Worker) {
			defer wg.Done()

			wctx, cancel := context.WithTimeout(ctx, c.timeout)
			defer cancel()

			src, closer, cerr := w.Connect(wctx)
			if cerr != nil {
				c.log.Warn("metrics: worker connect failed", "node", w.Name, "err", cerr)
				mu.Lock()
				out = append(out, NodeSample{Node: w.Name})
				mu.Unlock()
				return
			}
			defer func() {
				if closer != nil {
					_ = closer()
				}
			}()

			s := c.sampleOne(wctx, w.Name, src)
			mu.Lock()
			out = append(out, s)
			mu.Unlock()
		}(w)
	}

	wg.Wait()
	return out
}

// sampleOne collects stats + capacity from one node. On any error it returns OK=false.
func (c *ClusterSource) sampleOne(ctx context.Context, name string, src NodeStatsSource) NodeSample {
	stats, err := src.ListContainerStats(ctx)
	if err != nil {
		c.log.Warn("metrics: node stats failed", "node", name, "err", err)
		return NodeSample{Node: name}
	}
	info, err := src.NodeInfo(ctx)
	if err != nil {
		c.log.Warn("metrics: node info failed", "node", name, "err", err)
		// still report containers; capacity stays zero
		return NodeSample{Node: name, Containers: stats, OK: true}
	}
	return NodeSample{Node: name, Containers: stats, Capacity: info, OK: true}
}
