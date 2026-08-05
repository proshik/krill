package server

import (
	"testing"
	"time"
)

// parseRange collapses anything it does not recognise to 24h, but the cache was
// keyed on the RAW query parameter. Every distinct ?range=... string therefore
// minted its own entry for the same underlying response, and nothing ever
// removed them: an unbounded map fed straight from the URL.
func TestMonitoringCacheKeyIsNormalized(t *testing.T) {
	if got, want := monCacheKey(7, "1h"), monCacheKey(7, "1h"); got != want {
		t.Fatalf("stable input produced different keys: %q vs %q", got, want)
	}
	// Junk and the default both mean 24h, so they must share one entry.
	a := monCacheKey(7, "24h")
	for _, junk := range []string{"", "banana", "24h ", "1e9h", "../../etc"} {
		if got := monCacheKey(7, junk); got != a {
			t.Errorf("range %q keyed as %q, want the normalized %q", junk, got, a)
		}
	}
	// Different orgs must still be isolated.
	if monCacheKey(7, "1h") == monCacheKey(8, "1h") {
		t.Error("cache key does not separate organizations")
	}
	// Genuinely different ranges must not collide.
	if monCacheKey(7, "1h") == monCacheKey(7, "6h") {
		t.Error("1h and 6h share a cache key")
	}
}

// Stale entries were kept forever: once written, a key stayed in the map for
// the process's lifetime even though it could never be served again.
func TestMonCachePutDropsExpiredEntries(t *testing.T) {
	s := &Server{}
	s.monCachePut("old", []byte("x"))

	s.monMu.Lock()
	e := s.monCache["old"]
	e.at = time.Now().Add(-2 * time.Hour) // long past any TTL
	s.monCache["old"] = e
	s.monMu.Unlock()

	s.monCachePut("fresh", []byte("y"))

	s.monMu.Lock()
	defer s.monMu.Unlock()
	if _, ok := s.monCache["old"]; ok {
		t.Error("expired entry survived a later put: the cache only ever grows")
	}
	if _, ok := s.monCache["fresh"]; !ok {
		t.Error("fresh entry missing")
	}
}
