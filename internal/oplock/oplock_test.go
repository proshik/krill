package oplock

import "testing"

func TestTryAcquireReleaseHeld(t *testing.T) {
	n := DBInstance("krill-pg-x")
	if Held(n) {
		t.Fatal("fresh lock reported held")
	}
	if !TryAcquire(n) {
		t.Fatal("first acquire failed")
	}
	if TryAcquire(n) {
		t.Fatal("second acquire succeeded")
	}
	if !Held(n) {
		t.Fatal("held lock not reported")
	}
	Release(n)
	if Held(n) {
		t.Fatal("released lock reported held")
	}
	if !TryAcquire(n) {
		t.Fatal("re-acquire after release failed")
	}
	Release(n)
}
