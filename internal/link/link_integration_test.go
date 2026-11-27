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
		if pv[0].Duplicate == nil {
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
// accepted on the next connect.
func TestLinkRenewal(t *testing.T) {
	prevLife, prevCheck := PeerLifetime, renewCheck
	PeerLifetime, renewCheck = 6*time.Second, 200*time.Millisecond
	t.Cleanup(func() { PeerLifetime, renewCheck = prevLife, prevCheck })
	ctx := context.Background()
	relay, client, _ := pairedPair(t)
	first, err := client.svc.store.Client(ctx)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a renewed certificate", 15*time.Second, func() bool {
		m, err := client.svc.store.Client(ctx)
		return err == nil && m.Identity.CertPEM != first.Identity.CertPEM
	})
	// Drop the session; the client reconnects with the new certificate.
	client.svc.client.Wake()
	eventually(t, "a connect with the renewed certificate", 15*time.Second, func() bool {
		peers, _ := relay.svc.Peers(ctx)
		return len(peers) == 1 && peers[0].Connected && peers[0].Peer.PrevCertSha256 == nil
	})
}

// A direct connection to the listener — a forged PROXY header without the
// gateway's certificate, or no PROXY header — is closed before its stream is
// read.
func TestListenerRefusesForgedProxy(t *testing.T) {
	ctx := context.Background()
	relay := newInstall(t, freePort(t))
	if err := relay.svc.SetRelay(ctx, testLinkHost); err != nil {
		t.Fatal(err)
	}
	addr := "127.0.0.1:" + strconv.Itoa(relay.port)
	src := netip.MustParseAddrPort("203.0.113.9:4444")
	dst := netip.MustParseAddrPort("127.0.0.1:" + strconv.Itoa(relay.port))

	t.Run("forged proxy, no client certificate", func(t *testing.T) {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_, _ = c.Write(EncodeProxyHeader(src, dst))
		tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) //nolint:gosec
		_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
		err = tc.Handshake()
		if err == nil {
			_, err = tc.Read(make([]byte, 1))
		}
		if err == nil {
			t.Fatal("a connection without the gateway certificate was accepted")
		}
	})
	t.Run("forged proxy, wrong client certificate", func(t *testing.T) {
		other, _ := NewSelfSigned("krill-link-gateway", time.Now())
		cert, _ := tlsCert(other)
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_, _ = c.Write(EncodeProxyHeader(src, dst))
		tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true, Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}) //nolint:gosec
		_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
		err = tc.Handshake()
		if err == nil {
			_, err = tc.Read(make([]byte, 1))
		}
		if err == nil {
			t.Fatal("a connection with a foreign certificate was accepted")
		}
	})
	t.Run("no proxy header", func(t *testing.T) {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}) //nolint:gosec
		_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
		if err := tc.Handshake(); err == nil {
			t.Fatal("TLS without a PROXY header was accepted")
		}
	})
}
