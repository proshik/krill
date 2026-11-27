package link

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"
)

// maxPending bounds connections not yet admitted: those still proving they
// come from the gateway and those whose client is not yet admitted or paired.
const maxPending = 256

// GatewayListener is the authenticated gateway receiver on
// KRILL_LINK_LISTEN_ADDR. Only the gateway (Traefik) of this installation
// connects to it, yet any container on the host can reach the port and send a
// PROXY header, so every connection must present, in this order:
//
//  1. a PROXY v2 header, in clear — the original source;
//  2. TLS in which the peer presents the gateway's client certificate, pinned
//     by fingerprint;
//  3. the original stream inside that TLS.
//
// A connection failing either step is closed before its inner stream is read.
// The PROXY header is believed only together with step 2.
type GatewayListener struct {
	ln      net.Listener
	cfg     *tls.Config
	handle  func(net.Conn, netip.AddrPort, func())
	pending chan struct{}
	wg      sync.WaitGroup
}

// ListenGateway listens on addr and authenticates connections with local's
// certificates; handle gets each authenticated inner stream and its source.
// handle must call the func it gets once the connection no longer counts as
// pending (admitted, paired or refused); it is called again when handle
// returns, which is harmless.
func ListenGateway(addr string, local LocalMaterial, handle func(net.Conn, netip.AddrPort, func())) (*GatewayListener, error) {
	cfg, err := gatewayServerTLS(local)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	l := &GatewayListener{ln: ln, cfg: cfg, handle: handle, pending: make(chan struct{}, maxPending)}
	l.wg.Add(1)
	go l.serve()
	return l, nil
}

// gatewayServerTLS is the listener's TLS: its own certificate, and a client
// certificate that must be exactly the gateway's.
func gatewayServerTLS(local LocalMaterial) (*tls.Config, error) {
	own, err := tlsCert(local.Listener)
	if err != nil {
		return nil, err
	}
	want, err := CertFingerprint(local.Gateway.CertPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12, // the gateway's own TLS client decides
		Certificates: []tls.Certificate{own},
		ClientAuth:   tls.RequireAnyClientCert,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 || Fingerprint(raw[0]) != want {
				return errNotGateway
			}
			return nil
		},
	}, nil
}

var errNotGateway = errors.New("client certificate is not the gateway's")

// Addr is where the listener listens.
func (l *GatewayListener) Addr() net.Addr { return l.ln.Addr() }

// Close stops accepting. Sessions already handed over end with their owner.
func (l *GatewayListener) Close() error {
	err := l.ln.Close()
	l.wg.Wait()
	return err
}

func (l *GatewayListener) serve() {
	defer l.wg.Done()
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Warn("link: accept failed", "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		select {
		case l.pending <- struct{}{}:
		default:
			_ = conn.Close() // too many unauthenticated connections
			continue
		}
		go l.authenticate(conn)
	}
}

func (l *GatewayListener) authenticate(conn net.Conn) {
	release := func() { <-l.pending }
	_ = conn.SetDeadline(time.Now().Add(handshakeLimit))
	hdr, err := ReadProxyHeader(conn)
	if err != nil {
		release()
		slog.Debug("link listener: connection without a PROXY v2 header closed", "remote", conn.RemoteAddr())
		_ = conn.Close()
		return
	}
	tc := tls.Server(conn, l.cfg)
	ctx, cancel := context.WithTimeout(context.Background(), handshakeLimit)
	err = tc.HandshakeContext(ctx)
	cancel()
	if err != nil {
		release()
		slog.Warn("link listener: connection without the gateway certificate closed", "remote", conn.RemoteAddr())
		_ = conn.Close()
		return
	}
	_ = conn.SetDeadline(time.Time{})
	// The slot stays taken until the handler admits or refuses the
	// connection: the inner TLS, yamux and the first header still cost.
	var once sync.Once
	free := func() { once.Do(release) }
	defer free()
	l.handle(tc, hdr.Src, free)
}
