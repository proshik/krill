//go:build integration

package link

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/proshik/krill/internal/netguard"
)

const testDest = "api.example.com"

// tlsEcho is a TLS server for one host that answers "pong" to "ping".
type tlsEcho struct {
	ln   net.Listener
	pool *x509.CertPool
}

func startTLSEcho(t *testing.T, host string) *tlsEcho {
	t.Helper()
	kp, err := NewSelfSigned(host, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tlsCert(kp)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4)
				if _, err := io.ReadFull(c, buf); err == nil && string(buf) == "ping" {
					_, _ = c.Write([]byte("pong"))
				}
			}(c)
		}
	}()
	pool := x509.NewCertPool()
	leaf, _ := ParseCert(kp.CertPEM)
	pool.AddCert(leaf)
	return &tlsEcho{ln: ln, pool: pool}
}

// relayDialsTo makes the relay reach every destination at addr, recording
// what it dialed.
func relayDialsTo(relay install, addr string) func() []string {
	var mu sync.Mutex
	var dialed []string
	relay.svc.relay.dial = func(ctx context.Context, network, dest string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, dest)
		mu.Unlock()
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(dialed)
	}
}

// roundTrip opens an egress stream for d and speaks TLS through it to sni.
func roundTrip(t *testing.T, client install, d Destination, sni string, roots *x509.CertPool) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, _, err := client.svc.client.OpenEgress(ctx, d, 7)
	if err != nil {
		return err
	}
	defer st.Close()
	tc := tls.Client(st, &tls.Config{ServerName: sni, RootCAs: roots, MinVersion: tls.VersionTLS12})
	_ = tc.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tc.HandshakeContext(ctx); err != nil {
		return err
	}
	if _, err := tc.Write([]byte("ping")); err != nil {
		return err
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(tc, buf); err != nil {
		return err
	}
	if string(buf) != "pong" {
		return errors.New("unexpected answer " + string(buf))
	}
	return nil
}

// An egress stream through two in-process link endpoints reaches a TLS
// server end to end; the relay refuses a destination off its list and a
// ClientHello naming another host than the declared one, before it dials
// anything.
func TestEgressStreamThroughTheLink(t *testing.T) {
	relay, client, _ := pairedPair(t)
	ctx := context.Background()
	srv := startTLSEcho(t, testDest)
	dialed := relayDialsTo(relay, srv.ln.Addr().String())
	d := Destination{Host: testDest, Port: 443}

	// Not on the relay's list yet.
	if err := roundTrip(t, client, d, testDest, srv.pool); err == nil {
		t.Fatal("a destination off the relay's list went through")
	}
	if got := dialed(); len(got) != 0 {
		t.Fatalf("the relay dialed %v for a refused destination", got)
	}

	if err := relay.svc.AddDestination(ctx, d); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the client to get the list", 10*time.Second, func() bool {
		ds, ok := client.svc.client.RelayDestinations()
		return ok && slices.Contains(ds, d)
	})
	if err := roundTrip(t, client, d, testDest, srv.pool); err != nil {
		t.Fatalf("egress round trip: %v", err)
	}
	if got := dialed(); !slices.Equal(got, []string{"api.example.com:443"}) {
		t.Fatalf("dialed %v", got)
	}

	// The ClientHello names another host behind the allowed one.
	if err := roundTrip(t, client, d, "other.example.com", nil); err == nil {
		t.Fatal("a ClientHello for another host went through")
	}
	if got := dialed(); len(got) != 1 {
		t.Fatalf("the relay dialed for a mismatched SNI: %v", got)
	}

	// Counted on the relay, by the service the stream named.
	peers, _ := relay.svc.Peers(ctx)
	tr := relay.svc.relay.PeerTraffic(peers[0].Peer.ID)
	if len(tr) != 1 || tr[0].Service != 7 || tr[0].Conns != 1 || tr[0].Up == 0 || tr[0].Down == 0 || tr[0].Refused < 2 {
		t.Fatalf("relay traffic %+v", tr)
	}

	// Removed from the list: new streams are refused at once.
	if err := relay.svc.RemoveDestination(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := roundTrip(t, client, d, testDest, srv.pool); err == nil {
		t.Fatal("a removed destination went through")
	}
	eventually(t, "the client to drop the destination", 10*time.Second, func() bool {
		ds, _ := client.svc.client.RelayDestinations()
		return !slices.Contains(ds, d)
	})
}

// The client's manifest reaches the relay on connect and on change.
func TestManifestReachesTheRelay(t *testing.T) {
	relay, client, _ := pairedPair(t)
	client.svc.client.SetManifest([]ManifestService{{ID: 3, Path: "bots/prod/notifier", Destinations: []string{"api.telegram.org:443"}}})
	peers, _ := relay.svc.Peers(context.Background())
	id := peers[0].Peer.ID
	eventually(t, "the manifest", 10*time.Second, func() bool {
		s, ok := relay.svc.relay.Session(id)
		return ok && len(s.Services) == 1 && s.Services[0].Path == "bots/prod/notifier"
	})
}

// The relay dials destinations through netguard with private egress
// refused: a name that resolves to a loopback or private address is not
// reached.
func TestRelayDialRefusesPrivate(t *testing.T) {
	r := newRelay(context.Background(), nil, "test")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, addr := range []string{"127.0.0.1:443", "10.1.2.3:443", "169.254.169.254:443"} {
		_, err := r.dial(ctx, "tcp", addr)
		if !errors.Is(err, netguard.ErrBlocked) || dialCategory(err) != "blocked" {
			t.Fatalf("dial %s: %v", addr, err)
		}
	}
}

// A client that did not say it speaks egress (the link core release) gets no
// destination list in the relay's hello, nor lists later: its control line
// stays what that release reads.
func TestRelayHelloWithoutEgress(t *testing.T) {
	relay, client, _ := pairedPair(t)
	ctx := context.Background()
	if err := relay.svc.AddDestination(ctx, Destination{testDest, 443}); err != nil {
		t.Fatal(err)
	}
	// The client's own session holds the peer: forget it so the raw one is
	// admitted.
	client.svc.Close()
	eventually(t, "the session to end", 10*time.Second, func() bool { return connectedPeers(t, relay) == 0 })
	mux := clientMux(t, client)
	ctl, err := mux.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteHeader(ctl, StreamControl, helloHeader{Proto: ProtoVersion}); err != nil {
		t.Fatal(err)
	}
	br := newMessageReader(ctl)
	_ = ctl.SetReadDeadline(time.Now().Add(10 * time.Second))
	m, err := readMessage(br)
	if err != nil || m.Type != "hello" || len(m.Destinations) != 0 {
		t.Fatalf("hello %+v %v, want no destinations", m, err)
	}
	// A change of the list later sends it nothing either.
	if err := relay.svc.AddDestination(ctx, Destination{"other.example.com", 443}); err != nil {
		t.Fatal(err)
	}
	_ = ctl.SetReadDeadline(time.Now().Add(2 * time.Second))
	if m, err := readMessage(br); err == nil {
		t.Fatalf("a client without egress got %+v", m)
	}
}
