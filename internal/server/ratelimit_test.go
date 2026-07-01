package server

import (
	"testing"
	"time"
)

func TestLoginRateLimiter(t *testing.T) {
	l := newLoginRateLimiter(3, time.Minute)
	base := time.Unix(1_700_000_000, 0)

	// First 3 attempts from one IP pass, the 4th is blocked.
	for i := 0; i < 3; i++ {
		if !l.allow("1.2.3.4", base) {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
	}
	if l.allow("1.2.3.4", base) {
		t.Fatal("4th attempt should be blocked")
	}

	// A different IP is unaffected.
	if !l.allow("5.6.7.8", base) {
		t.Fatal("other IP should be allowed")
	}

	// After the window elapses the counter resets.
	if !l.allow("1.2.3.4", base.Add(time.Minute+time.Second)) {
		t.Fatal("attempt after window should be allowed")
	}
}
