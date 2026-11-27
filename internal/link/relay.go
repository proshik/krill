package link

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	db "github.com/proshik/krill/internal/database/gen"
)

// relayTLS is what the relay serves with; replaced as a whole on reload.
type relayTLS struct {
	cfg *tls.Config
	ca  KeyPair
}

// Relay serves the client end of the link: it accepts connections the gateway
// listener authenticated, admits paired peers and pairs new ones.
type Relay struct {
	store   *Store
	version string
	reg     *registry
	limit   *rateLimiter
	tls     atomic.Pointer[relayTLS]
	ctx     context.Context

	mu   sync.Mutex
	dups map[int64]DuplicateAttempt // the latest refused second session per peer
}

// DuplicateAttempt is a refused second session for a peer: a clone of the
// client (restored from a state backup) is the usual cause.
type DuplicateAttempt struct {
	From netip.Addr
	At   time.Time
}

// newRelay creates a relay; ctx bounds every database call of its sessions.
func newRelay(ctx context.Context, store *Store, version string) *Relay {
	return &Relay{
		store: store, version: version, reg: newRegistry(), ctx: ctx,
		limit: newRateLimiter(10, time.Minute), dups: map[int64]DuplicateAttempt{},
	}
}

// load reads the CA and server certificate; a relay without them serves
// nothing.
func (r *Relay) load(ctx context.Context) error {
	m, err := r.store.Relay(ctx)
	if err != nil {
		return err
	}
	chain, err := tlsCert(m.Server, m.CA.CertPEM)
	if err != nil {
		return err
	}
	caCert, err := ParseCert(m.CA.CertPEM)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	r.tls.Store(&relayTLS{
		ca: m.CA,
		cfg: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{chain},
			// Pairing connects without a certificate; anything else must
			// present one the link CA signed. The fingerprint is checked
			// against link_peers after the handshake, on every connection.
			ClientAuth: tls.VerifyClientCertIfGiven,
			ClientCAs:  pool,
		},
	})
	return nil
}

// unload stops serving and closes every session.
func (r *Relay) unload() {
	r.tls.Store(nil)
	r.reg.closeAll()
}

// RemovePeer deletes a peer and closes its session, serialized with any
// admission of the same peer.
func (r *Relay) RemovePeer(ctx context.Context, id int64) error {
	return r.reg.revoke(id, func() error { return r.store.deletePeer(ctx, id) })
}

// relaySession is one admitted client session.
type relaySession struct {
	mux     *yamux.Session
	peerID  int64
	peer    string
	fp      string
	src     netip.AddrPort
	since   time.Time
	version string
	latency atomic.Int64 // nanoseconds, 0 until the first ping
	streams atomic.Int32
}

func (s *relaySession) Close() error { return s.mux.Close() }

// PeerSession is the state of a peer's live session, for the page.
type PeerSession struct {
	From    netip.Addr
	Since   time.Time
	Version string
	Latency time.Duration
	Streams int
}

// Session returns the peer's live session, if any.
func (r *Relay) Session(id int64) (PeerSession, bool) {
	s, ok := r.reg.live(id).(*relaySession)
	if !ok || s == nil {
		return PeerSession{}, false
	}
	return PeerSession{From: s.src.Addr(), Since: s.since, Version: s.version,
		Latency: time.Duration(s.latency.Load()), Streams: int(s.streams.Load())}, true
}

// Duplicate returns the latest refused second session of a peer.
func (r *Relay) Duplicate(id int64) (DuplicateAttempt, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.dups[id]
	return d, ok
}

// ServeConn serves one connection the gateway listener authenticated; src is
// its original source from the PROXY header. It returns when the connection
// ends.
func (r *Relay) ServeConn(conn net.Conn, src netip.AddrPort) {
	defer conn.Close()
	t := r.tls.Load()
	if t == nil {
		return
	}
	tc := tls.Server(conn, t.cfg)
	hctx, cancel := context.WithTimeout(r.ctx, handshakeLimit)
	err := tc.HandshakeContext(hctx)
	cancel()
	if err != nil {
		slog.Info("link: handshake failed", "from", src.Addr(), "category", CatTLS)
		return
	}
	mux, err := yamux.Server(tc, muxConfig())
	if err != nil {
		return
	}
	defer mux.Close()
	first, err := acceptWithin(mux, HeaderTimeout)
	if err != nil {
		return
	}
	typ, body, err := readHeaderWithin(first, HeaderTimeout)
	if err != nil {
		slog.Warn("link: no stream header", "from", src.Addr())
		return
	}
	certs := tc.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		if typ != StreamPair {
			slog.Warn("link: stream refused without a client certificate", "from", src.Addr(), "stream", typ.String())
			_ = first.Close()
			return
		}
		r.pair(first, body, src, t.ca)
		return
	}
	if typ != StreamControl {
		slog.Warn("link: session must open with a control stream", "from", src.Addr(), "stream", typ.String())
		_ = first.Close()
		return
	}
	r.session(mux, first, body, Fingerprint(certs[0].Raw), src, t.ca)
}

// pair handles the one pair stream of a connection without a certificate.
func (r *Relay) pair(st *yamux.Stream, body []byte, src netip.AddrPort, ca KeyPair) {
	defer st.Close()
	reply := func(m message) {
		if err := writeMessage(st, m); err != nil {
			slog.Info("link: pairing answer not delivered", "from", src.Addr())
		}
	}
	if !r.limit.allow(src.Addr().String(), time.Now()) {
		slog.Warn("link: pairing rate limit", "from", src.Addr())
		reply(message{Type: "error", Category: pairRateLimited, Message: "too many pairing attempts, try again in a minute"})
		return
	}
	var h pairHeader
	if err := json.Unmarshal(body, &h); err != nil || h.Secret == "" || h.CSR == "" {
		reply(message{Type: "error", Category: pairRefused, Message: "malformed pairing request"})
		return
	}
	cert, peer, err := r.store.Redeem(r.ctx, h.Secret, h.CSR, ca)
	switch {
	case errors.Is(err, ErrCodeRefused):
		slog.Warn("link: pairing refused", "from", src.Addr())
		reply(message{Type: "error", Category: pairRefused, Message: "the pairing code is unknown, expired or already used"})
		return
	case errors.Is(err, ErrPeerExists):
		slog.Warn("link: pairing refused, peer name taken", "peer", peer, "from", src.Addr())
		reply(message{Type: "error", Category: pairPeerExists, Message: "a peer with this name already exists on the relay"})
		return
	case err != nil:
		slog.Error("link: pairing failed", "peer", peer, "err", err)
		reply(message{Type: "error", Category: pairInternal, Message: "the relay could not complete pairing"})
		return
	}
	slog.Info("link: peer paired", "peer", peer, "from", src.Addr())
	reply(message{Type: "cert", Cert: cert})
}

// session admits a peer and serves its session until it ends.
func (r *Relay) session(mux *yamux.Session, ctl *yamux.Stream, body []byte, fp string, src netip.AddrPort, ca KeyPair) {
	refuse := func(cat, msg string) {
		_ = writeMessage(ctl, message{Type: "error", Category: cat, Message: msg})
		_ = ctl.Close()
	}
	var hello helloHeader
	if err := json.Unmarshal(body, &hello); err != nil {
		refuse(CatIncompatible, "malformed hello")
		return
	}
	peer, err := r.store.PeerByFingerprint(r.ctx, fp)
	if errors.Is(err, ErrPeerGone) {
		slog.Warn("link: unknown peer certificate", "from", src.Addr(), "category", CatUnauthorized)
		refuse(CatUnauthorized, "this client is not paired with the relay (removed or re-paired)")
		return
	}
	if err != nil {
		slog.Error("link: peer lookup failed", "err", err)
		refuse(CatTCP, "the relay could not check the peer")
		return
	}
	if hello.Proto != ProtoVersion {
		slog.Warn("link: incompatible protocol", "peer", peer.Name, "proto", hello.Proto, "category", CatIncompatible)
		refuse(CatIncompatible, "link protocol "+strconv.Itoa(hello.Proto)+" is not supported by the relay (it speaks "+strconv.Itoa(ProtoVersion)+"); update Krill on one side")
		return
	}
	s := &relaySession{mux: mux, peerID: peer.ID, peer: peer.Name, fp: fp, src: src, since: time.Now(), version: hello.KrillVersion}
	err = r.reg.admit(peer.ID, s, func() error {
		// Re-read by ID under the peer's lock: a removal since the
		// fingerprint check above must win.
		p, err := r.store.Peer(r.ctx, peer.ID)
		if err != nil {
			return err
		}
		if p.CertSha256 != fp && deref(p.PrevCertSha256) != fp {
			return ErrPeerGone
		}
		peer = p
		return nil
	}, func(old sessionCloser) bool {
		o, ok := old.(*relaySession)
		return ok && !pingWithin(o.mux, 5*time.Second)
	})
	switch {
	case errors.Is(err, ErrDuplicate):
		r.mu.Lock()
		r.dups[peer.ID] = DuplicateAttempt{From: src.Addr(), At: time.Now()}
		r.mu.Unlock()
		slog.Warn("link: duplicate session refused", "peer", peer.Name, "from", src.Addr(), "category", CatDuplicate)
		refuse(CatDuplicate, "another installation holds this peer's session (a clone restored from a backup?)")
		return
	case errors.Is(err, ErrPeerGone):
		refuse(CatUnauthorized, "this client is not paired with the relay (removed or re-paired)")
		return
	case err != nil:
		slog.Error("link: admission failed", "peer", peer.Name, "err", err)
		refuse(CatTCP, "the relay could not admit the session")
		return
	}
	defer r.reg.release(peer.ID, s)
	if err := writeMessage(ctl, message{Type: "hello", Proto: ProtoVersion, KrillVersion: r.version}); err != nil {
		return
	}
	if err := r.store.markConnected(r.ctx, peer.ID, fp); err != nil {
		slog.Error("link: record connect failed", "peer", peer.Name, "err", err)
	}
	slog.Info("link: session connected", "peer", peer.Name, "from", src.Addr(), "version", hello.KrillVersion)
	start := time.Now()
	defer func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), 5*time.Second)
		defer cancel()
		if err := r.store.markDisconnected(dctx, peer.ID); err != nil {
			slog.Error("link: record disconnect failed", "peer", peer.Name, "err", err)
		}
		slog.Info("link: session closed", "peer", peer.Name, "duration", time.Since(start).Round(time.Second))
	}()

	go measureLatency(mux, &s.latency)
	go r.control(s, ctl, peer, ca)
	for {
		st, err := mux.AcceptStream()
		if err != nil {
			return
		}
		if !r.reg.registered(peer.ID, s) {
			_ = st.Close()
			_ = mux.Close()
			return
		}
		if s.streams.Add(1) > MaxStreamsPerPeer {
			s.streams.Add(-1)
			slog.Warn("link: stream refused, per-peer limit", "peer", peer.Name, "limit", MaxStreamsPerPeer)
			_ = st.Close()
			continue
		}
		go func() {
			defer s.streams.Add(-1)
			defer st.Close()
			typ, _, err := readHeaderWithin(st, HeaderTimeout)
			if err != nil {
				slog.Warn("link: stream refused, no header", "peer", peer.Name)
				return
			}
			// Egress and ingress streams arrive with later releases.
			slog.Warn("link: stream refused, unsupported type", "peer", peer.Name, "stream", typ.String())
		}()
	}
}

// control serves a session's control stream: certificate renewal. An unknown
// message closes the session.
func (r *Relay) control(s *relaySession, ctl *yamux.Stream, peer db.LinkPeer, ca KeyPair) {
	defer s.mux.Close()
	br := newMessageReader(ctl)
	for {
		m, err := readMessage(br)
		if err != nil {
			return
		}
		switch m.Type {
		case "renew":
			cert, err := r.store.Renew(r.ctx, peer, s.fp, m.CSR, ca)
			if err != nil {
				slog.Warn("link: certificate renewal refused", "peer", peer.Name, "err", err)
				if writeMessage(ctl, message{Type: "error", Category: CatUnauthorized, Message: "renewal refused"}) != nil {
					return
				}
				continue
			}
			slog.Info("link: certificate renewed", "peer", peer.Name)
			if writeMessage(ctl, message{Type: "cert", Cert: cert}) != nil {
				return
			}
		default:
			slog.Warn("link: unknown control message", "peer", peer.Name, "type", m.Type)
			return
		}
	}
}

// pingWithin reports whether the session answers a ping within d.
func pingWithin(mux *yamux.Session, d time.Duration) bool {
	done := make(chan error, 1)
	go func() { _, err := mux.Ping(); done <- err }()
	select {
	case err := <-done:
		return err == nil
	case <-time.After(d):
		return false
	}
}

// measureLatency pings the session every keepalive interval until it ends.
func measureLatency(mux *yamux.Session, into *atomic.Int64) {
	t := time.NewTicker(keepAlive)
	defer t.Stop()
	for {
		if rtt, err := mux.Ping(); err == nil {
			into.Store(int64(rtt))
		}
		select {
		case <-mux.CloseChan():
			return
		case <-t.C:
		}
	}
}
