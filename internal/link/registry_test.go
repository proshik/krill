package link

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

type fakeSession struct{ closed atomic.Bool }

func (f *fakeSession) Close() error { f.closed.Store(true); return nil }

// peers stands in for link_peers.
type peers struct {
	mu sync.Mutex
	m  map[int64]bool
}

func (p *peers) exists(id int64) func() error {
	return func() error {
		p.mu.Lock()
		defer p.mu.Unlock()
		if !p.m[id] {
			return ErrPeerGone
		}
		return nil
	}
}

func (p *peers) delete(id int64) func() error {
	return func() error {
		p.mu.Lock()
		defer p.mu.Unlock()
		delete(p.m, id)
		return nil
	}
}

// The handshake checked the fingerprint, then the peer was removed, then the
// session tried to register: it must be refused, not registered.
func TestAdmitAfterRevokeIsRefused(t *testing.T) {
	reg := newRegistry()
	db := &peers{m: map[int64]bool{7: true}}
	// handshake: the peer exists
	if err := db.exists(7)(); err != nil {
		t.Fatal(err)
	}
	if err := reg.revoke(7, db.delete(7)); err != nil {
		t.Fatal(err)
	}
	s := &fakeSession{}
	if err := reg.admit(7, s, db.exists(7), nil); !errors.Is(err, ErrPeerGone) {
		t.Fatalf("admit after removal: %v, want ErrPeerGone", err)
	}
	if reg.registered(7, s) {
		t.Fatal("a session of a removed peer is registered")
	}
}

// Revoke after admission closes the session and unregisters it, so its next
// stream is refused.
func TestRevokeClosesTheLiveSession(t *testing.T) {
	reg := newRegistry()
	db := &peers{m: map[int64]bool{7: true}}
	s := &fakeSession{}
	if err := reg.admit(7, s, db.exists(7), nil); err != nil {
		t.Fatal(err)
	}
	if !reg.registered(7, s) {
		t.Fatal("admitted session not registered")
	}
	if err := reg.revoke(7, db.delete(7)); err != nil {
		t.Fatal(err)
	}
	if !s.closed.Load() || reg.registered(7, s) {
		t.Fatalf("after revoke: closed=%v registered=%v", s.closed.Load(), reg.registered(7, s))
	}
}

// Under any interleaving of admission and removal, no session survives the
// removal: it is either refused or closed.
func TestAdmitRevokeRace(t *testing.T) {
	for i := 0; i < 2000; i++ {
		reg := newRegistry()
		db := &peers{m: map[int64]bool{1: true}}
		s := &fakeSession{}
		var admitErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); admitErr = reg.admit(1, s, db.exists(1), nil) }()
		go func() { defer wg.Done(); _ = reg.revoke(1, db.delete(1)) }()
		wg.Wait()
		if admitErr == nil && !s.closed.Load() {
			t.Fatalf("iteration %d: session admitted and not closed after the peer was removed", i)
		}
		if reg.registered(1, s) {
			t.Fatalf("iteration %d: session still registered after removal", i)
		}
	}
}

func TestDuplicateSession(t *testing.T) {
	reg := newRegistry()
	db := &peers{m: map[int64]bool{1: true}}
	first, second := &fakeSession{}, &fakeSession{}
	if err := reg.admit(1, first, db.exists(1), nil); err != nil {
		t.Fatal(err)
	}
	alive := func(sessionCloser) bool { return false }
	if err := reg.admit(1, second, db.exists(1), alive); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("second session: %v, want ErrDuplicate", err)
	}
	if first.closed.Load() || !reg.registered(1, first) {
		t.Fatal("a refused duplicate must leave the first session alone")
	}
	// A first session that no longer answers is replaced.
	dead := func(sessionCloser) bool { return true }
	if err := reg.admit(1, second, db.exists(1), dead); err != nil {
		t.Fatal(err)
	}
	if !first.closed.Load() || !reg.registered(1, second) {
		t.Fatal("a dead first session must be closed and replaced")
	}
	// The replaced session ending must not unregister its successor.
	reg.release(1, first)
	if !reg.registered(1, second) {
		t.Fatal("release of a replaced session unregistered the new one")
	}
}

// A delete whose answer was lost may have committed: the session closes
// whatever the delete reports.
func TestRevokeClosesOnDeleteError(t *testing.T) {
	reg := newRegistry()
	db := &peers{m: map[int64]bool{7: true}}
	s := &fakeSession{}
	if err := reg.admit(7, s, db.exists(7), nil); err != nil {
		t.Fatal(err)
	}
	lost := errors.New("connection reset after commit")
	if err := reg.revoke(7, func() error { return lost }); !errors.Is(err, lost) {
		t.Fatalf("revoke: %v, want the delete's error", err)
	}
	if !s.closed.Load() || reg.registered(7, s) {
		t.Fatal("the session must close even when the delete's answer is lost")
	}
}
