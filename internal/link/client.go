package link

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"
)

// Client states.
const (
	StateOff        = "off"        // not a client
	StateUnpaired   = "unpaired"   // a client with no relay yet
	StateConnecting = "connecting" // dialing, or waiting before the next attempt
	StateConnected  = "connected"
)

// renewCheck is how often a session looks at its certificate's age; a
// variable for tests.
var renewCheck = time.Hour

// Reconnect backoff bounds.
const (
	backoffMin = time.Second
	backoffMax = 60 * time.Second
	dialLimit  = 10 * time.Second
)

// ClientStatus is the client's view of its link, for the page.
type ClientStatus struct {
	State        string
	Category     string // the latest failure, "" when none since the last connect
	Message      string
	FailedAt     time.Time
	Since        time.Time // connected since
	Latency      time.Duration
	RelayVersion string
	CertNotAfter time.Time
}

// Client keeps the one session of a paired client with its relay.
type Client struct {
	store   *Store
	version string
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)

	mu     sync.Mutex
	status ClientStatus
	wake   chan struct{}
	cur    *yamux.Session
	// gen counts Wake calls: an attempt that read the settings before the
	// latest one (a Forget, a new pairing) must not become the session.
	gen     uint64
	pending map[string]pendingKey // pairing keys by the code's secret hash
	lat     atomic.Int64

	// The relay's destination list as its last hello or lists said, and
	// whether that relay speaks egress at all. Kept across sessions for the
	// pages; egress decisions also require the session to be up.
	dests       []Destination
	relayEgress bool
	// ctl is the live session's control stream, ctlMu orders writes to it.
	ctl      net.Conn
	ctlMu    sync.Mutex
	manifest []ManifestService
}

// pendingKey is the key of a pairing attempt that has not been stored yet. A
// retry of the same code reuses it, so a relay that already redeemed the code
// for it answers with the same certificate.
type pendingKey struct {
	keyPEM, csrPEM string
	until          time.Time
}

func newClient(store *Store, version string) *Client {
	d := &net.Dialer{Timeout: dialLimit}
	return &Client{store: store, version: version, dial: d.DialContext,
		wake: make(chan struct{}, 1), status: ClientStatus{State: StateOff}, pending: map[string]pendingKey{}}
}

// Status returns a copy of the client's state.
func (c *Client) Status() ClientStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.status
	if st.State == StateConnected {
		st.Latency = time.Duration(c.lat.Load())
	}
	return st
}

func (c *Client) set(f func(*ClientStatus)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f(&c.status)
}

// fail records a failed attempt.
func (c *Client) fail(cat, msg string) {
	c.set(func(s *ClientStatus) {
		s.State, s.Category, s.Message, s.FailedAt = StateConnecting, cat, msg, time.Now()
	})
}

// Wake makes a waiting client re-read its settings now: paired, forgotten or
// re-paired.
func (c *Client) Wake() {
	c.mu.Lock()
	c.gen++
	cur := c.cur
	c.mu.Unlock()
	if cur != nil {
		_ = cur.Close()
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// run keeps the session up until ctx ends.
func (c *Client) run(ctx context.Context) {
	defer c.set(func(s *ClientStatus) { *s = ClientStatus{State: StateOff} })
	delay := time.Duration(0)
	for {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return
			case <-c.wake:
				delay = 0
			case <-time.After(jitter(delay)):
			}
		}
		c.mu.Lock()
		gen := c.gen
		c.mu.Unlock()
		m, err := c.store.Client(ctx)
		if ctx.Err() != nil {
			return
		}
		switch {
		case errors.Is(err, ErrNotPaired):
			c.set(func(s *ClientStatus) { *s = ClientStatus{State: StateUnpaired} })
			select {
			case <-ctx.Done():
				return
			case <-c.wake:
			}
			delay = 0
			continue
		case err != nil:
			slog.Error("link: read client settings failed", "err", err)
			c.fail(CatTLS, "the client key cannot be read: "+err.Error())
			delay = nextDelay(delay)
			continue
		}
		if cert, perr := ParseCert(m.Identity.CertPEM); perr == nil {
			c.set(func(s *ClientStatus) { s.CertNotAfter = cert.NotAfter })
		}
		c.set(func(s *ClientStatus) {
			if s.State != StateConnecting {
				s.State = StateConnecting
			}
		})
		connected := c.once(ctx, m, gen)
		if connected {
			delay = 0
		}
		delay = nextDelay(delay)
	}
}

// once runs one session attempt and reports whether it got connected.
func (c *Client) once(ctx context.Context, m ClientMaterial, gen uint64) bool {
	id, err := tlsCert(m.Identity)
	if err != nil {
		c.fail(CatTLS, "the client certificate is unusable")
		return false
	}
	mux, err := c.connect(ctx, m.Addr, pinnedTLS(m.SNI, m.CASHA256, &id))
	if err != nil {
		c.fail(category(err), err.Error())
		slog.Info("link: connect failed", "relay", m.SNI, "category", category(err))
		return false
	}
	defer mux.Close()
	ctl, err := mux.OpenStream()
	if err != nil {
		c.fail(category(err), err.Error())
		return false
	}
	if err := WriteHeader(ctl, StreamControl, helloHeader{Proto: ProtoVersion, KrillVersion: c.version, Egress: true}); err != nil {
		c.fail(category(err), err.Error())
		return false
	}
	br := newMessageReader(ctl)
	_ = ctl.SetReadDeadline(time.Now().Add(replyTimeout))
	reply, err := readMessage(br)
	if err != nil {
		c.fail(category(err), "no answer from the relay: "+err.Error())
		slog.Info("link: no hello from the relay", "relay", m.SNI, "category", category(err))
		return false
	}
	_ = ctl.SetReadDeadline(time.Time{})
	if reply.Type == "error" {
		cat := sessionCategory(reply.Category)
		c.fail(cat, reply.Message)
		slog.Warn("link: relay refused the session", "relay", m.SNI, "category", cat)
		return false
	}
	if reply.Type != "hello" || reply.Proto != ProtoVersion {
		c.fail(CatIncompatible, fmt.Sprintf("the relay speaks link protocol %d, this Krill %d; update Krill on one side", reply.Proto, ProtoVersion))
		slog.Warn("link: incompatible relay", "relay", m.SNI, "proto", reply.Proto, "category", CatIncompatible)
		return false
	}
	c.mu.Lock()
	if c.gen != gen {
		// Forgotten or re-paired while this attempt was connecting.
		c.mu.Unlock()
		return false
	}
	c.cur = mux
	c.status = ClientStatus{State: StateConnected, Since: time.Now(), RelayVersion: reply.KrillVersion, CertNotAfter: c.status.CertNotAfter}
	c.relayEgress = reply.Egress
	c.dests = SortDestinations(reply.Destinations)
	c.ctl = ctl
	c.mu.Unlock()
	c.lat.Store(0)
	slog.Info("link: connected to the relay", "relay", m.SNI, "relay_version", reply.KrillVersion)
	defer func() {
		c.mu.Lock()
		if c.cur == mux {
			c.cur = nil
			c.ctl = nil
		}
		c.mu.Unlock()
	}()
	c.sendManifest()

	go measureLatency(mux, &c.lat)
	go c.refuseStreams(mux)
	if c.renewals(ctx, mux, ctl, br, m) {
		c.set(func(s *ClientStatus) { s.State = StateConnecting })
		return true
	}
	if ctx.Err() == nil {
		c.fail(CatTCP, "the session to the relay ended")
		slog.Info("link: session to the relay ended", "relay", m.SNI)
	}
	return true
}

// sessionCategory keeps a relay's refusal category to the known ones: it is
// shown, logged and part of a translation key.
func sessionCategory(cat string) string {
	switch cat {
	case CatDNS, CatTCP, CatTLS, CatUnauthorized, CatIncompatible, CatDuplicate:
		return cat
	}
	return CatTCP
}

// connect dials the relay and opens a yamux session over TLS.
func (c *Client) connect(ctx context.Context, addr string, cfg *tls.Config) (*yamux.Session, error) {
	dctx, cancel := context.WithTimeout(ctx, dialLimit)
	defer cancel()
	raw, err := c.dial(dctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	tc := tls.Client(raw, cfg)
	hctx, hcancel := context.WithTimeout(ctx, handshakeLimit)
	defer hcancel()
	if err := tc.HandshakeContext(hctx); err != nil {
		_ = raw.Close()
		return nil, err
	}
	mux, err := yamux.Client(tc, muxConfig())
	if err != nil {
		_ = tc.Close()
		return nil, err
	}
	return mux, nil
}

// refuseStreams closes every stream the relay opens: ingress arrives with a
// later release.
func (c *Client) refuseStreams(mux *yamux.Session) {
	for {
		st, err := mux.AcceptStream()
		if err != nil {
			return
		}
		if mux.NumStreams() > MaxStreamsPerPeer {
			slog.Warn("link: session closed, stream limit from the relay", "limit", MaxStreamsPerPeer)
			_ = mux.Close()
			return
		}
		go func() {
			defer st.Close()
			typ, _, err := readHeaderWithin(st, HeaderTimeout)
			if err != nil {
				return
			}
			slog.Warn("link: stream from the relay refused, unsupported type", "stream", typ.String())
		}()
	}
}

// renewals watches the certificate and renews it over the live session at two
// thirds of its lifetime. It returns when the session ends.
//
// It reports true when it ended the session itself to reconnect with a
// renewed certificate.
func (c *Client) renewals(ctx context.Context, mux *yamux.Session, ctl net.Conn, br *bufio.Reader, m ClientMaterial) bool {
	cert, err := ParseCert(m.Identity.CertPEM)
	if err != nil {
		return false
	}
	replies := make(chan message)
	go func() {
		defer close(replies)
		for {
			msg, err := readMessage(br)
			if err != nil {
				_ = mux.Close()
				return
			}
			select {
			case replies <- msg:
			case <-mux.CloseChan():
				return
			}
		}
	}()
	check := time.NewTicker(renewCheck)
	defer check.Stop()
	var pendingKey string
	// One renewal per session: the session stays authenticated by the
	// certificate it connected with, and the relay keeps that one valid
	// only until the first connect with the new one. A second renewal here
	// would retire the stored certificate in favour of one whose answer
	// might be lost.
	renewed := false
	ask := func() {
		if renewed || pendingKey != "" || !needsRenewal(cert.NotBefore, cert.NotAfter, time.Now()) {
			return
		}
		key, csr, err := NewCSR()
		if err != nil {
			slog.Error("link: create renewal key failed", "err", err)
			return
		}
		c.ctlMu.Lock()
		err = writeMessage(ctl, message{Type: "renew", CSR: csr})
		c.ctlMu.Unlock()
		if err != nil {
			return
		}
		pendingKey = key
	}
	ask()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-mux.CloseChan():
			return false
		case <-check.C:
			ask()
		case msg, ok := <-replies:
			if !ok {
				return false
			}
			if msg.Type == "lists" {
				c.setDests(msg.Destinations)
				continue
			}
			if msg.Type != "cert" || pendingKey == "" {
				slog.Warn("link: renewal refused by the relay", "category", sessionCategory(msg.Category))
				pendingKey = ""
				continue
			}
			next := KeyPair{CertPEM: msg.Cert, KeyPEM: pendingKey}
			pendingKey = ""
			if err := matches(next); err != nil {
				slog.Error("link: renewed certificate does not match the key", "err", err)
				continue
			}
			saved, err := c.store.SaveRenewed(ctx, m.Identity.CertPEM, next)
			if err != nil {
				slog.Error("link: store renewed certificate failed", "err", err)
				continue
			}
			if saved {
				renewed = true
				if nc, _ := ParseCert(next.CertPEM); nc != nil {
					c.set(func(s *ClientStatus) { s.CertNotAfter = nc.NotAfter })
					slog.Info("link: certificate renewed; reconnecting with it", "not_after", nc.NotAfter.Format(time.DateOnly))
				}
				// Reconnect with the new certificate: it retires the old
				// one on the relay, and the next renewal then starts from a
				// session that holds the stored certificate. A session left
				// running could outlive both.
				_ = mux.Close()
				return true
			}
		}
	}
}

// matches checks a key pair's certificate is for its key.
func matches(p KeyPair) error {
	_, err := tls.X509KeyPair([]byte(p.CertPEM), []byte(p.KeyPEM))
	return err
}

// Pair redeems a pairing code: it creates the client's key, opens a pair
// stream to the relay with the code's secret and a CSR, and stores the
// certificate it gets. The answer can be lost after the relay committed, so a
// failed exchange is retried once with the same key, which the relay answers
// with the same certificate.
func (c *Client) Pair(ctx context.Context, raw string) error {
	code, err := DecodeCode(raw)
	if err != nil {
		return err
	}
	key, csr, err := c.pairingKey(code)
	if err != nil {
		return err
	}
	var cert string
	for attempt := 0; attempt < 2; attempt++ {
		cert, err = c.pairOnce(ctx, code, csr)
		var refused *PairRefusedError
		if err == nil || errors.As(err, &refused) || ctx.Err() != nil {
			break
		}
	}
	var refused *PairRefusedError
	if errors.As(err, &refused) && (refused.Category == pairRefused || refused.Category == pairPeerExists) {
		// Final for this code and key. A rate limit or a relay error is
		// not: the relay may already hold a certificate for this key.
		c.dropPairingKey(code)
	}
	if err != nil {
		return err
	}
	id := KeyPair{CertPEM: cert, KeyPEM: key}
	if err := matches(id); err != nil {
		return fmt.Errorf("the relay's certificate is not for this key: %w", err)
	}
	if err := c.store.SavePaired(ctx, code, id); err != nil {
		return err
	}
	c.dropPairingKey(code)
	c.Wake()
	return nil
}

// pairingKey returns the key of an earlier attempt with the same code that
// never got stored — its answer lost, or the local write failed — or a new
// one.
func (c *Client) pairingKey(code PairingCode) (string, string, error) {
	h := SecretSHA256(code.Secret)
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, p := range c.pending {
		if now.After(p.until) {
			delete(c.pending, k)
		}
	}
	if p, ok := c.pending[h]; ok {
		return p.keyPEM, p.csrPEM, nil
	}
	key, csr, err := NewCSR()
	if err != nil {
		return "", "", err
	}
	c.pending[h] = pendingKey{keyPEM: key, csrPEM: csr, until: now.Add(CodeLifetime)}
	return key, csr, nil
}

func (c *Client) dropPairingKey(code PairingCode) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, SecretSHA256(code.Secret))
}

// PairRefusedError is the relay's own refusal of a pairing attempt.
type PairRefusedError struct {
	Category string // refused | rate_limited | peer_exists | internal
	Message  string
}

func (e *PairRefusedError) Error() string { return "pairing refused: " + e.Message }

// PairFailedError is a pairing attempt that never got the relay's answer.
type PairFailedError struct {
	Category string // dns | tcp | tls
	Err      error
}

func (e *PairFailedError) Error() string {
	return "pairing failed (" + e.Category + "): " + e.Err.Error()
}
func (e *PairFailedError) Unwrap() error { return e.Err }

func (c *Client) pairOnce(ctx context.Context, code PairingCode, csr string) (string, error) {
	mux, err := c.connect(ctx, code.Addr, pinnedTLS(code.SNI, code.CASHA256, nil))
	if err != nil {
		return "", &PairFailedError{Category: category(err), Err: err}
	}
	defer mux.Close()
	st, err := mux.OpenStream()
	if err != nil {
		return "", &PairFailedError{Category: category(err), Err: err}
	}
	if err := WriteHeader(st, StreamPair, pairHeader{Secret: code.Secret, CSR: csr}); err != nil {
		return "", &PairFailedError{Category: category(err), Err: err}
	}
	_ = st.SetReadDeadline(time.Now().Add(replyTimeout))
	reply, err := readMessage(newMessageReader(st))
	if err != nil {
		return "", &PairFailedError{Category: category(err), Err: err}
	}
	if reply.Type != "cert" {
		// The relay's words are shown and logged: accept only the known
		// categories, and keep its message out of the log.
		cat := reply.Category
		switch cat {
		case pairRefused, pairRateLimited, pairPeerExists, pairInternal:
		default:
			cat = pairRefused
		}
		return "", &PairRefusedError{Category: cat, Message: reply.Message}
	}
	return reply.Cert, nil
}

// jitter spreads d over [d/2, d).
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

// nextDelay doubles the wait from backoffMin up to backoffMax.
func nextDelay(d time.Duration) time.Duration {
	if d < backoffMin {
		return backoffMin
	}
	d *= 2
	if d > backoffMax {
		return backoffMax
	}
	return d
}
