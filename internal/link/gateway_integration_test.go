//go:build integration

package link

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/proshik/krill/internal/panel"
)

// TestRelayGatewayOnRealTraefik serves the relay's dynamic configuration —
// the panel's HTTP routes merged with the link's TCP route — to the pinned
// Traefik and checks what the relay relies on: the link host passes through
// to the link listener (PROXY v2 + the gateway's client certificate) and a
// client pairs and connects across it, the panel route still answers on the
// same entrypoint, and a direct connection to the listener with a forged PROXY
// header and no gateway certificate is refused.
func TestRelayGatewayOnRealTraefik(t *testing.T) {
	ctx := context.Background()
	relay := newInstall(t, freePort(t))
	if err := relay.svc.SetRelay(ctx, testLinkHost); err != nil {
		t.Fatal(err)
	}
	local, err := relay.svc.store.Local(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// The panel's upstream: a plain HTTP server standing in for Krill's UI.
	uiLn, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	uiPort := uiLn.Addr().(*net.TCPAddr).Port
	ui := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(panel.ForwardedHeader) != "fwd-token" {
			http.Error(w, "not via the gateway", http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, "panel ok")
	})}
	go func() { _ = ui.Serve(uiLn) }()
	t.Cleanup(func() { _ = ui.Close() })

	host := testcontainers.HostInternal
	cfg := Merge(
		panel.DynamicConfig(panel.Settings{Host: "krill.example.com", State: panel.StateActive}, "http://"+net.JoinHostPort(host, strconv.Itoa(uiPort)), "fwd-token"),
		GatewayConfig(testLinkHost, net.JoinHostPort(host, strconv.Itoa(relay.port)), local),
	)
	dynamic, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Traefik polls the configuration over its HTTP provider, as it polls
	// /_krill/gateway/config in production.
	provLn, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	provPort := provLn.Addr().(*net.TCPAddr).Port
	prov := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != panel.ProviderPath || r.Header.Get(panel.ProviderHeader) != "prov-token" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(dynamic)
	})}
	go func() { _ = prov.Serve(provLn) }()
	t.Cleanup(func() { _ = prov.Close() })
	gw, err := testcontainers.Run(ctx, "traefik:v3.6.1",
		testcontainers.WithHostPortAccess(uiPort, relay.port, provPort),
		testcontainers.WithCmd("--entrypoints.web.address=:80", "--entrypoints.websecure.address=:443",
			"--providers.http.endpoint=http://"+net.JoinHostPort(host, strconv.Itoa(provPort))+panel.ProviderPath,
			"--providers.http.headers."+panel.ProviderHeader+"=prov-token",
			"--providers.http.pollInterval=1s", "--log.level=INFO"),
		testcontainers.WithExposedPorts("443/tcp"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("443/tcp")))
	if err != nil {
		t.Fatal(err)
	}
	testcontainers.CleanupContainer(t, gw)
	gwHost, err := gw.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mapped, err := gw.MappedPort(ctx, "443/tcp")
	if err != nil {
		t.Fatal(err)
	}
	gwAddr := net.JoinHostPort(gwHost, mapped.Port())

	t.Run("panel route still answers", func(t *testing.T) {
		c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, ServerName: "krill.example.com"}, //nolint:gosec // Traefik's default certificate
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", gwAddr)
			},
		}}
		var body string
		eventually(t, "the panel route", 30*time.Second, func() bool {
			resp, err := c.Get("https://krill.example.com/")
			if err != nil {
				return false
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			body = string(b)
			return resp.StatusCode == http.StatusOK
		})
		if body != "panel ok" {
			t.Fatalf("panel answered %q", body)
		}
	})

	t.Run("client pairs and connects through the gateway", func(t *testing.T) {
		client := newInstall(t, freePort(t))
		if err := client.svc.SetClient(ctx); err != nil {
			t.Fatal(err)
		}
		code, err := relay.svc.CreatePairingCode(ctx, "msk", gwAddr)
		if err != nil {
			t.Fatal(err)
		}
		if err := client.svc.Pair(ctx, code); err != nil {
			t.Fatalf("pair through Traefik: %v", err)
		}
		eventually(t, "the session through Traefik", 20*time.Second, func() bool {
			return client.svc.ClientStatus().State == StateConnected && connectedPeers(t, relay) == 1
		})
		peers, _ := relay.svc.Peers(ctx)
		// The connection reaches the listener from the port forwarder on
		// loopback; the source the relay records came from the PROXY header.
		if from := peers[0].Session.From; !from.IsValid() || from.IsLoopback() {
			t.Fatalf("relay saw the client from %v: want the address Traefik saw, from the PROXY header", from)
		}
	})

	t.Run("forged PROXY header without the gateway certificate", func(t *testing.T) {
		c, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(relay.port))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_, _ = c.Write(EncodeProxyHeader(netip.MustParseAddrPort("203.0.113.9:1234"), netip.MustParseAddrPort("127.0.0.1:443")))
		tc := tls.Client(c, &tls.Config{InsecureSkipVerify: true, ServerName: testLinkHost}) //nolint:gosec
		start := time.Now()
		_ = tc.SetDeadline(time.Now().Add(8 * time.Second))
		err = tc.Handshake()
		if err == nil {
			_, err = tc.Read(make([]byte, 1))
		}
		// An accepted connection would wait for the inner stream and time
		// out here: only a prompt refusal counts.
		var ne net.Error
		if err == nil || (errors.As(err, &ne) && ne.Timeout()) || time.Since(start) > time.Second {
			t.Fatalf("a forged PROXY header without the gateway certificate: %v after %v, want a prompt refusal", err, time.Since(start))
		}
	})
}
