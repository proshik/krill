package link

import (
	"errors"
	"sync"
)

// ErrDuplicate refuses a second session for a peer whose first one lives.
var ErrDuplicate = errors.New("the peer already has a live session")

// ErrPeerGone refuses a session whose peer was removed after its handshake.
var ErrPeerGone = errors.New("the peer was removed")

// sessionCloser is the part of a relay session the registry needs.
type sessionCloser interface {
	Close() error
}

// registry holds the relay's live sessions, one per peer. Admission and
// revocation of a peer are serialized by that peer's own lock: a session whose
// certificate was checked before the peer was removed but registered after
// would otherwise outlive the removal.
type registry struct {
	mu    sync.Mutex
	slots map[int64]*peerSlot
}

type peerSlot struct {
	mu   sync.Mutex // admission and revocation of this peer
	live sessionCloser
}

func newRegistry() *registry { return &registry{slots: map[int64]*peerSlot{}} }

func (r *registry) slot(id int64) *peerSlot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.slots[id]
	if s == nil {
		s = &peerSlot{}
		r.slots[id] = s
	}
	return s
}

// admit registers sess for peer id. Under the peer's lock it first runs
// stillThere, which re-reads the peer by ID: a peer removed since the
// handshake fails it (ErrPeerGone). A live session refuses the new one
// (ErrDuplicate) unless stale reports it dead, in which case it is closed and
// replaced.
func (r *registry) admit(id int64, sess sessionCloser, stillThere func() error, stale func(old sessionCloser) bool) error {
	s := r.slot(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := stillThere(); err != nil {
		return err
	}
	if s.live != nil {
		if stale == nil || !stale(s.live) {
			return ErrDuplicate
		}
		_ = s.live.Close()
	}
	s.live = sess
	return nil
}

// revoke removes a peer: under the peer's lock it runs del (the row delete)
// and then closes the peer's live session, so no admission can slip between.
func (r *registry) revoke(id int64, del func() error) error {
	s := r.slot(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	// Close whatever del reports: a delete whose answer was lost may have
	// committed, and closing a session that was not revoked after all only
	// makes its client reconnect and be admitted again.
	err := del()
	if s.live != nil {
		_ = s.live.Close()
		s.live = nil
	}
	return err
}

// registered reports whether sess is still peer id's live session. Every new
// stream asks.
func (r *registry) registered(id int64, sess sessionCloser) bool {
	s := r.slot(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live == sess
}

// release unregisters sess when it ends, unless another session replaced it.
func (r *registry) release(id int64, sess sessionCloser) {
	s := r.slot(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live == sess {
		s.live = nil
	}
}

// live returns the peer's live session, nil when none.
func (r *registry) live(id int64) sessionCloser {
	s := r.slot(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.live
}

// closeAll closes every live session (the role changed or Krill stops).
func (r *registry) closeAll() {
	r.mu.Lock()
	slots := make([]*peerSlot, 0, len(r.slots))
	for _, s := range r.slots {
		slots = append(slots, s)
	}
	r.mu.Unlock()
	for _, s := range slots {
		s.mu.Lock()
		if s.live != nil {
			_ = s.live.Close()
			s.live = nil
		}
		s.mu.Unlock()
	}
}
