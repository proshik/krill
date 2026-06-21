package docker

import "testing"

func TestCPUCacheDelta(t *testing.T) {
	c := NewCPUCache()
	// first sample for an id has no baseline -> 0
	if got := c.delta("x", cpuCounters{total: 100, system: 1000}, 2); got != 0 {
		t.Fatalf("first sample want 0, got %v", got)
	}
	// second sample: delta total=100, system=1000, online=2 -> (100/1000)*2*100 = 20
	if got := c.delta("x", cpuCounters{total: 200, system: 2000}, 2); got != 20 {
		t.Fatalf("want 20, got %v", got)
	}
	// restart (counters drop) -> 0, not underflow
	if got := c.delta("x", cpuCounters{total: 5, system: 50}, 2); got != 0 {
		t.Fatalf("restart want 0, got %v", got)
	}
}

func TestCPUCacheIsolation(t *testing.T) {
	// two caches (= two nodes) with the same container id must not interfere
	a, b := NewCPUCache(), NewCPUCache()
	a.delta("c", cpuCounters{total: 100, system: 1000}, 1)
	// b has never seen "c" -> still a baseline-less 0 (proves caches are independent)
	if got := b.delta("c", cpuCounters{total: 500, system: 5000}, 1); got != 0 {
		t.Fatalf("independent cache want 0, got %v", got)
	}
}
