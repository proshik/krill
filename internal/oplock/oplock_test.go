package oplock

import (
	"context"
	"errors"
	"testing"
	"time"
)

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

func TestAcquireCancellationAndRelease(t *testing.T) {
	key := AppRouting(987654321)
	if !TryAcquire(key) {
		t.Fatal("fresh lock busy")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Acquire(ctx, key) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter ignored cancellation")
	}
	if !Held(key) {
		t.Fatal("canceled waiter released another owner's lock")
	}
	Release(key)
	if err := Acquire(t.Context(), key); err != nil {
		t.Fatal(err)
	}
	go func() { done <- Acquire(t.Context(), key) }()
	Release(key)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("release did not wake waiter")
	}
	Release(key)
	if Held(key) {
		t.Fatal("lock retained")
	}
	if err := Acquire(ctx, key); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled caller acquired free lock")
	}
	if Held(key) {
		t.Fatal("canceled acquisition leaked lock")
	}
}
