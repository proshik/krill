//go:build integration

package link

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/proshik/krill/internal/hostshare"
	"github.com/proshik/krill/internal/testutil"
)

const testLinkHost = "link.example.com"

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// install is one Krill installation's link service on its own database.
type install struct {
	svc  *Service
	port int // the link listener's port
}

func newInstall(t *testing.T, port int) install {
	t.Helper()
	pool := testutil.NewTestDB(t)
	return startInstall(t, pool, port)
}

func startInstall(t *testing.T, pool *pgxpool.Pool, port int) install {
	t.Helper()
	svc := NewService(pool, hostshare.NewStore(pool), Config{
		AdvertiseAddr: "127.0.0.1", ListenAddr: ":8080",
		LinkListenAddr: ":" + strconv.Itoa(port), Version: "v0.0.0-test",
	})
	if err := svc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	return install{svc: svc, port: port}
}

// fakeGateway does what the relay's Traefik does for the link host: accepts
// the client's TCP connection, opens a connection to the link listener, sends
// a PROXY v2 header with the client's address, opens TLS with the gateway's
// client certificate and passes the client's bytes through it unchanged.
type fakeGateway struct {
	ln    net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func startGateway(t *testing.T, relay install) *fakeGateway {
	t.Helper()
	return startGatewayOn(t, relay, "127.0.0.1:0")
}

func (g *fakeGateway) addr() string { return g.ln.Addr().String() }

func (g *fakeGateway) forward(c net.Conn, port int, cfg *tls.Config) {
	defer c.Close()
	up, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return
	}
	defer up.Close()
	g.mu.Lock()
	g.conns = append(g.conns, c, up)
	g.mu.Unlock()
	src := netip.MustParseAddrPort(c.RemoteAddr().String())
	dst := netip.MustParseAddrPort(c.LocalAddr().String())
	if _, err := up.Write(EncodeProxyHeader(src, dst)); err != nil {
		return
	}
	tc := tls.Client(up, cfg)
	if err := tc.Handshake(); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(tc, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, tc); done <- struct{}{} }()
	<-done
}

func (g *fakeGateway) close() {
	_ = g.ln.Close()
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, c := range g.conns {
		_ = c.Close()
	}
}

func eventually(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func connectedPeers(t *testing.T, relay install) int {
	t.Helper()
	peers, err := relay.svc.Peers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range peers {
		if p.Connected {
			n++
		}
	}
	return n
}

// pairedPair sets up a relay, its gateway and a client paired through it.
func pairedPair(t *testing.T) (relay, client install, gw *fakeGateway) {
	t.Helper()
	ctx := context.Background()
	relay = newInstall(t, freePort(t))
	if err := relay.svc.SetRelay(ctx, testLinkHost); err != nil {
		t.Fatal(err)
	}
	gw = startGateway(t, relay)
	client = newInstall(t, freePort(t))
	if err := client.svc.SetClient(ctx); err != nil {
		t.Fatal(err)
	}
	code, err := relay.svc.CreatePairingCode(ctx, "msk", gw.addr())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.svc.Pair(ctx, code); err != nil {
		t.Fatalf("pair: %v", err)
	}
	eventually(t, "the client to connect", 15*time.Second, func() bool {
		return client.svc.ClientStatus().State == StateConnected && connectedPeers(t, relay) == 1
	})
	return relay, client, gw
}

func TestLinkPairConnectAndRefusals(t *testing.T) {
	ctx := context.Background()
	relay, client, gw := pairedPair(t)

	st := client.svc.ClientStatus()
	if st.RelayVersion != "v0.0.0-test" || st.CertNotAfter.IsZero() {
		t.Fatalf("client status %+v", st)
	}
	peers, _ := relay.svc.Peers(ctx)
	if peers[0].Peer.Name != "msk" || peers[0].Session.Version != "v0.0.0-test" || !peers[0].Session.From.IsLoopback() {
		t.Fatalf("relay sees %+v", peers[0])
	}

	t.Run("code reuse refused", func(t *testing.T) {
		code, err := relay.svc.CreatePairingCode(ctx, "spb", gw.addr())
		if err != nil {
			t.Fatal(err)
		}
		other := newInstall(t, freePort(t))
		if err := other.svc.SetClient(ctx); err != nil {
			t.Fatal(err)
		}
		if err := other.svc.Pair(ctx, code); err != nil {
			t.Fatal(err)
		}
		third := newInstall(t, freePort(t))
		if err := third.svc.SetClient(ctx); err != nil {
			t.Fatal(err)
		}
		var refused *PairRefusedError
		if err := third.svc.Pair(ctx, code); !errors.As(err, &refused) || refused.Category != pairRefused {
			t.Fatalf("second use of a code: %v, want refused", err)
		}
	})

	t.Run("duplicate session refused", func(t *testing.T) {
		clone := newInstall(t, freePort(t))
		m, err := client.svc.store.Client(ctx)
		if err != nil {
			t.Fatal(err)
		}
		code := PairingCode{Addr: m.Addr, SNI: m.SNI, CASHA256: m.CASHA256}
		if err := clone.svc.store.SavePaired(ctx, code, m.Identity); err != nil {
			t.Fatal(err)
		}
		if err := clone.svc.apply(ctx); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the clone to be refused as a duplicate", 15*time.Second, func() bool {
			return clone.svc.ClientStatus().Category == CatDuplicate
		})
		if client.svc.ClientStatus().State != StateConnected {
			t.Fatal("the original session must survive a refused duplicate")
		}
		pv, _ := relay.svc.Peers(ctx)
		if pv[0].Refusal == nil || pv[0].Refusal.Category != CatDuplicate {
			t.Fatal("the relay must show the duplicate attempt")
		}
		clone.svc.Close()
	})

	t.Run("incompatible protocol", func(t *testing.T) {
		m, err := client.svc.store.Client(ctx)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := tlsCert(m.Identity)
		mux, err := client.svc.client.connect(ctx, m.Addr, pinnedTLS(m.SNI, m.CASHA256, &id))
		if err != nil {
			t.Fatal(err)
		}
		defer mux.Close()
		ctl, _ := mux.OpenStream()
		if err := WriteHeader(ctl, StreamControl, helloHeader{Proto: 2, KrillVersion: "v9"}); err != nil {
			t.Fatal(err)
		}
		reply, err := readMessage(newMessageReader(ctl))
		if err != nil || reply.Type != "error" || reply.Category != CatIncompatible {
			t.Fatalf("reply %+v, %v; want incompatible", reply, err)
		}
		pv, _ := relay.svc.Peers(ctx)
		if pv[0].Refusal == nil || pv[0].Refusal.Category != CatIncompatible || pv[0].Refusal.Proto != 2 {
			t.Fatalf("the relay page must show why: %+v", pv[0].Refusal)
		}
	})

	t.Run("unknown stream type closed", func(t *testing.T) {
		client.svc.client.mu.Lock()
		mux := client.svc.client.cur
		client.svc.client.mu.Unlock()
		st, err := mux.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		if err := WriteHeader(st, StreamType(99), map[string]string{}); err != nil {
			t.Fatal(err)
		}
		_ = st.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := st.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("unknown stream: read %v, want EOF", err)
		}
		if client.svc.ClientStatus().State != StateConnected {
			t.Fatal("an unknown stream must not end the session")
		}
	})

	t.Run("peer removal drops the session", func(t *testing.T) {
		pv, _ := relay.svc.Peers(ctx)
		var id int64
		for _, p := range pv {
			if p.Peer.Name == "msk" {
				id = p.Peer.ID
			}
		}
		if err := relay.svc.RemovePeer(ctx, id); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the removed client to be refused", 15*time.Second, func() bool {
			return client.svc.ClientStatus().Category == CatUnauthorized
		})
	})
}

func TestLinkReconnectAfterRelayRestart(t *testing.T) {
	relay, client, gw := pairedPair(t)
	pool := relay.svc.store.pool
	relay.svc.Close()
	gw.close()
	eventually(t, "the client to notice", 60*time.Second, func() bool {
		return client.svc.ClientStatus().State != StateConnected
	})
	restarted := startInstall(t, pool, relay.port)
	if restarted.svc.Role() != RoleRelay {
		t.Fatalf("restarted relay role %q", restarted.svc.Role())
	}
	// The client dials the same address: bring the gateway back there.
	startGatewayAt(t, restarted, gw.addr())
	eventually(t, "the client to reconnect", 70*time.Second, func() bool {
		return client.svc.ClientStatus().State == StateConnected && connectedPeers(t, restarted) == 1
	})
}

func startGatewayAt(t *testing.T, relay install, addr string) *fakeGateway {
	t.Helper()
	var ln net.Listener
	var err error
	for i := 0; i < 50; i++ {
		if ln, err = net.Listen("tcp", addr); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	return startGatewayOn(t, relay, addr)
}

func startGatewayOn(t *testing.T, relay install, addr string) *fakeGateway {
	t.Helper()
	local, err := relay.svc.store.Local(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	gwCert, _ := tlsCert(local.Gateway)
	listener, _ := ParseCert(local.Listener.CertPEM)
	roots := x509.NewCertPool()
	roots.AddCert(listener)
	cfg := &tls.Config{ServerName: ListenerServerName, RootCAs: roots, Certificates: []tls.Certificate{gwCert}, MinVersion: tls.VersionTLS12}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGateway{ln: ln}
	t.Cleanup(g.close)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go g.forward(c, relay.port, cfg)
		}
	}()
	return g
}

// A certificate renewed over the live session is stored by the client and
// accepted on the next connect. A session renews once: a second renewal while
// it still runs on the old certificate would retire the stored one.
func TestLinkRenewal(t *testing.T) {
	prevLife, prevCheck := PeerLifetime, renewCheck
	PeerLifetime, renewCheck = 12*time.Second, 200*time.Millisecond
	t.Cleanup(func() { PeerLifetime, renewCheck = prevLife, prevCheck })
	ctx := context.Background()
	relay, client, _ := pairedPair(t)
	first, err := client.svc.store.Client(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var renewed ClientMaterial
	eventually(t, "a renewed certificate", 20*time.Second, func() bool {
		m, err := client.svc.store.Client(ctx)
		renewed = m
		return err == nil && m.Identity.CertPEM != first.Identity.CertPEM
	})
	// Past two thirds of the renewed certificate's life, the same session
	// has not renewed again: the stored certificate is still the relay's.
	nc, _ := ParseCert(renewed.Identity.CertPEM)
	time.Sleep(time.Until(nc.NotBefore.Add(backdate).Add(PeerLifetime*2/3 + time.Second)))
	now, err := client.svc.store.Client(ctx)
	if err != nil || now.Identity.CertPEM != renewed.Identity.CertPEM {
		t.Fatal("the session renewed a second time")
	}
	fp, _ := CertFingerprint(renewed.Identity.CertPEM)
	peers, _ := relay.svc.Peers(ctx)
	if peers[0].Peer.CertSha256 != fp {
		t.Fatal("the relay no longer holds the stored certificate")
	}
	// Drop the session; the client reconnects with the new certificate. That
	// retires the first one; the new session may at once renew the second,
	// which then stays valid as the previous one.
	client.svc.client.Wake()
	oldFP, _ := CertFingerprint(first.Identity.CertPEM)
	eventually(t, "a connect with the renewed certificate", 15*time.Second, func() bool {
		peers, _ := relay.svc.Peers(ctx)
		if len(peers) != 1 || !peers[0].Connected {
			return false
		}
		prev := peers[0].Peer.PrevCertSha256
		return (prev == nil || *prev == fp) && peers[0].Peer.CertSha256 != oldFP
	})
}

// clientMux opens a raw session with the paired client's identity, through
// the gateway, without the client's own loop.
func clientMux(t *testing.T, client install) *yamux.Session {
	t.Helper()
	ctx := context.Background()
	m, err := client.svc.store.Client(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := tlsCert(m.Identity)
	mux, err := client.svc.client.connect(ctx, m.Addr, pinnedTLS(m.SNI, m.CASHA256, &id))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mux.Close() })
	return mux
}

func hello(t *testing.T, mux *yamux.Session) (message, error) {
	t.Helper()
	ctl, err := mux.OpenStream()
	if err != nil {
		return message{}, err
	}
	if err := WriteHeader(ctl, StreamControl, helloHeader{Proto: ProtoVersion}); err != nil {
		return message{}, err
	}
	_ = ctl.SetReadDeadline(time.Now().Add(10 * time.Second))
	return readMessage(newMessageReader(ctl))
}

// A connection that finished its handshake before the relay was turned off
// must not be admitted after it: turning off ends every session.
func TestRelayOffRefusesAPendingSession(t *testing.T) {
	ctx := context.Background()
	relay, client, _ := pairedPair(t)
	if err := client.svc.SetOff(ctx); err != nil { // free the peer's one session
		t.Fatal(err)
	}
	eventually(t, "the client session to end", 15*time.Second, func() bool { return connectedPeers(t, relay) == 0 })
	mux := clientMux(t, client) // TLS done, no hello yet
	if err := relay.svc.SetOff(ctx); err != nil {
		t.Fatal(err)
	}
	if reply, err := hello(t, mux); err == nil && reply.Type == "hello" {
		t.Fatal("a session was admitted after the relay was turned off")
	}
	if connectedPeers(t, relay) != 0 {
		t.Fatal("the relay lists a session while off")
	}
}

// An attempt that read the settings before a Wake (Forget, a new pairing)
// must not become the client's session.
func TestClientStaleAttemptIsDropped(t *testing.T) {
	ctx := context.Background()
	relay, client, _ := pairedPair(t)
	if err := client.svc.SetOff(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the client session to end", 15*time.Second, func() bool { return connectedPeers(t, relay) == 0 })
	c := client.svc.client
	m, err := client.svc.store.Client(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	stale := c.gen
	c.gen++
	c.mu.Unlock()
	if c.once(ctx, m, stale) {
		t.Fatal("a stale attempt reported a session")
	}
	c.mu.Lock()
	cur := c.cur
	c.mu.Unlock()
	if cur != nil {
		t.Fatal("a stale attempt became the session")
	}
	eventually(t, "the relay to drop the stale session", 15*time.Second, func() bool { return connectedPeers(t, relay) == 0 })
}

// The per-peer stream limit counts what the session holds, the control
// stream included; a stream over it is closed at once.
func TestStreamLimitPerPeer(t *testing.T) {
	prev := MaxStreamsPerPeer
	MaxStreamsPerPeer = 3
	t.Cleanup(func() { MaxStreamsPerPeer = prev })
	_, client, _ := pairedPair(t)
	client.svc.client.mu.Lock()
	mux := client.svc.client.cur
	client.svc.client.mu.Unlock()
	var held []*yamux.Stream
	for i := 0; i < 2; i++ { // control + 2 = the limit
		st, err := mux.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = st.Write([]byte{byte(StreamEgress)}) // a header that never completes
		held = append(held, st)
	}
	over, err := mux.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = over.Write([]byte{byte(StreamEgress)})
	_ = over.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := over.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("a stream over the limit: read %v, want EOF at once", err)
	}
	_ = held
}

// The relay reissues its server certificate before it expires, under the
// same CA, so clients that pinned the CA keep connecting.
func TestRelayServerCertificateRenewal(t *testing.T) {
	ctx := context.Background()
	relay, client, _ := pairedPair(t)
	before, err := relay.svc.store.Relay(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Replace the server certificate with one that expires in ten days.
	old, err := IssueServer(before.CA, testLinkHost, time.Now().Add(-serverLifetime+10*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := relay.svc.store.pool.Exec(ctx, "UPDATE link_settings SET server_cert = $1, server_key = $2", old.CertPEM, old.KeyPEM); err != nil {
		t.Fatal(err)
	}
	renewed, err := relay.svc.renewServerIfDueReport(ctx)
	if err != nil || !renewed {
		t.Fatalf("renewed=%v err=%v", renewed, err)
	}
	after, err := relay.svc.store.Relay(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.CA.CertPEM != before.CA.CertPEM {
		t.Fatal("the CA changed")
	}
	c, _ := ParseCert(after.Server.CertPEM)
	if time.Until(c.NotAfter) < serverLifetime-time.Hour || !issuedBy(after.Server, after.CA) {
		t.Fatalf("new server certificate until %v", c.NotAfter)
	}
	if again, err := relay.svc.renewServerIfDueReport(ctx); err != nil || again {
		t.Fatalf("a fresh certificate was renewed again: %v %v", again, err)
	}
	if err := relay.svc.apply(ctx); err != nil {
		t.Fatal(err)
	}
	client.svc.client.Wake()
	eventually(t, "a reconnect with the reissued server certificate", 15*time.Second, func() bool {
		return client.svc.ClientStatus().State == StateConnected && connectedPeers(t, relay) == 1
	})
}

// A direct connection to the listener — a forged PROXY header without the
// gateway's certificate or with another one, or the gateway's certificate
// without a PROXY header — is closed before its stream is read, promptly
// rather than by the test's own deadline; the same connection done right
// passes.
func TestListenerRefusesForgedProxy(t *testing.T) {
	ctx := context.Background()
	relay := newInstall(t, freePort(t))
	if err := relay.svc.SetRelay(ctx, testLinkHost); err != nil {
		t.Fatal(err)
	}
	local, err := relay.svc.store.Local(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gwCert, _ := tlsCert(local.Gateway)
	listenerCert, _ := ParseCert(local.Listener.CertPEM)
	roots := x509.NewCertPool()
	roots.AddCert(listenerCert)
	foreign, _ := NewSelfSigned("krill-link-gateway", time.Now())
	foreignCert, _ := tlsCert(foreign)
	addr := "127.0.0.1:" + strconv.Itoa(relay.port)
	src := netip.MustParseAddrPort("203.0.113.9:4444")
	dst := netip.MustParseAddrPort(addr)

	// try connects, optionally sends a PROXY header, opens TLS with cert and
	// reports the first error and how long it took to come.
	try := func(proxy bool, cert *tls.Certificate) (error, time.Duration) {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		start := time.Now()
		if proxy {
			_, _ = c.Write(EncodeProxyHeader(src, dst))
		}
		cfg := &tls.Config{ServerName: ListenerServerName, RootCAs: roots, MinVersion: tls.VersionTLS12}
		if cert != nil {
			cfg.Certificates = []tls.Certificate{*cert}
		}
		tc := tls.Client(c, cfg)
		_ = tc.SetDeadline(time.Now().Add(8 * time.Second))
		if err := tc.Handshake(); err != nil {
			return err, time.Since(start)
		}
		// TLS 1.3 reports a refused client certificate on the first read;
		// an accepted connection waits for the inner stream, so this read
		// times out.
		_ = tc.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, err = tc.Read(make([]byte, 1))
		return err, time.Since(start)
	}
	refusedFast := func(name string, err error, took time.Duration) {
		t.Helper()
		var ne net.Error
		if err == nil || (errors.As(err, &ne) && ne.Timeout()) || took > time.Second {
			t.Fatalf("%s: err %v after %v, want a prompt refusal", name, err, took)
		}
	}
	err, took := try(true, nil)
	refusedFast("forged proxy, no client certificate", err, took)
	err, took = try(true, &foreignCert)
	refusedFast("forged proxy, another certificate", err, took)
	err, took = try(false, &gwCert)
	refusedFast("gateway certificate without a PROXY header", err, took)
	// Control: the gateway's certificate after a PROXY header is accepted
	// and the listener waits for the inner stream.
	err, _ = try(true, &gwCert)
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("a correct gateway connection: %v, want accepted (waiting for the inner stream)", err)
	}
}
