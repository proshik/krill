package server

import "testing"

// TestAcquireLogSlot verifies the live-log-stream semaphore caps concurrency and
// that releasing a slot frees capacity again.
func TestAcquireLogSlot(t *testing.T) {
	s := &Server{logSem: make(chan struct{}, maxLiveLogStreams)}

	releases := make([]func(), 0, maxLiveLogStreams)
	for i := 0; i < maxLiveLogStreams; i++ {
		rel, ok := s.acquireLogSlot()
		if !ok {
			t.Fatalf("slot %d should be available", i)
		}
		releases = append(releases, rel)
	}

	if _, ok := s.acquireLogSlot(); ok {
		t.Fatal("acquiring beyond the cap must fail")
	}

	releases[0]() // free one
	rel, ok := s.acquireLogSlot()
	if !ok {
		t.Fatal("a slot should be free after a release")
	}
	rel()
	for _, r := range releases[1:] {
		r()
	}
}

// The cap was a single host-wide pool, so ONE member could open 24 streams and
// leave every other tenant unable to view any logs at all — a denial of service
// available to any authenticated user. A per-user share bounds that.
func TestAcquireLogSlotPerUserShare(t *testing.T) {
	s := &Server{logSem: make(chan struct{}, maxLiveLogStreams)}

	var greedy []func()
	for i := 0; i < maxLiveLogStreamsPerUser; i++ {
		rel, ok := s.acquireLogSlotFor(7)
		if !ok {
			t.Fatalf("user 7 slot %d should be available", i)
		}
		greedy = append(greedy, rel)
	}
	if _, ok := s.acquireLogSlotFor(7); ok {
		t.Fatal("one user must not exceed their share")
	}

	// Another tenant is unaffected.
	rel, ok := s.acquireLogSlotFor(8)
	if !ok {
		t.Fatal("a different user was starved by the first one's streams")
	}
	rel()

	// Releasing frees the user's share again.
	greedy[0]()
	rel2, ok := s.acquireLogSlotFor(7)
	if !ok {
		t.Fatal("a released slot did not free the user's share")
	}
	rel2()
	for _, r := range greedy[1:] {
		r()
	}
}
