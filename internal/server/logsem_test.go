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
