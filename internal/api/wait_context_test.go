package api

import (
	"context"
	"testing"
	"time"
)

// stoppedBy names why a wait ended. A wait runs on a parent that a shutdown
// also cancels (withStopping), and the two shutdown callbacks run in no fixed
// order, so the parent may be cancelled before the wait's own shutdown cause
// is set; it is still a shutdown.
func TestWaitContextStoppedBy(t *testing.T) {
	t.Run("shutdown that cancelled the parent first", func(t *testing.T) {
		stopping, stop := context.WithCancel(context.Background())
		s := &Service{stopping: stopping}
		parent, cancelParent := context.WithCancel(context.Background())
		ctx, cancel, stoppedBy := s.waitContext(parent, time.Hour)
		defer cancel()
		cancelParent() // the outer shutdown callback won the race
		<-ctx.Done()
		stop()
		if got := stoppedBy(); got != "shutdown" {
			t.Fatalf("stoppedBy = %q, want shutdown", got)
		}
	})
	t.Run("shutdown", func(t *testing.T) {
		stopping, stop := context.WithCancel(context.Background())
		s := &Service{stopping: stopping}
		ctx, cancel, stoppedBy := s.waitContext(context.Background(), time.Hour)
		defer cancel()
		stop()
		<-ctx.Done()
		if got := stoppedBy(); got != "shutdown" {
			t.Fatalf("stoppedBy = %q, want shutdown", got)
		}
	})
	t.Run("caller left while Krill runs", func(t *testing.T) {
		s := &Service{stopping: context.Background()}
		parent, cancelParent := context.WithCancel(context.Background())
		ctx, cancel, stoppedBy := s.waitContext(parent, time.Hour)
		defer cancel()
		cancelParent()
		<-ctx.Done()
		if got := stoppedBy(); got != "cancelled" {
			t.Fatalf("stoppedBy = %q, want cancelled", got)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		s := &Service{stopping: context.Background()}
		ctx, cancel, stoppedBy := s.waitContext(context.Background(), time.Millisecond)
		defer cancel()
		<-ctx.Done()
		if got := stoppedBy(); got != "timeout" {
			t.Fatalf("stoppedBy = %q, want timeout", got)
		}
	})
	t.Run("no stopping context wired", func(t *testing.T) {
		s := &Service{}
		parent, cancelParent := context.WithCancel(context.Background())
		ctx, cancel, stoppedBy := s.waitContext(parent, time.Hour)
		defer cancel()
		cancelParent()
		<-ctx.Done()
		if got := stoppedBy(); got != "cancelled" {
			t.Fatalf("stoppedBy = %q, want cancelled", got)
		}
	})
}
