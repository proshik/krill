package backup

import "testing"

func TestRunningFollowsTheClaim(t *testing.T) {
	s := &Service{inFlight: map[int64]bool{}}
	release, ok := s.claim(3)
	if !ok || !s.Running(3) || s.Running(4) {
		t.Fatal("a claimed id must read running, another must not")
	}
	release()
	if s.Running(3) {
		t.Fatal("a released id must not read running")
	}
}
