package docker

import (
	"context"
	"errors"
	"net"
	"testing"
)

var errStub = errors.New("stub")

// NewRemoteStatsWithCache must reuse the caller's cache so CPU% survives across
// sampler ticks (a fresh cache each tick makes every sample a "first sample" -> 0%).
func TestNewRemoteStatsWithCacheReusesCache(t *testing.T) {
	c := NewCPUCache()
	dial := func(_ context.Context, _, _ string) (net.Conn, error) { return nil, errStub }
	rs, err := NewRemoteStatsWithCache(dial, c)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if rs.sc.cache != c {
		t.Fatal("statsCollector must use the provided cache, not a fresh one")
	}
}
