package link

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
)

// ProtoVersion is the link protocol's major version. A relay refuses a client
// whose major version differs; the two installations update independently.
const ProtoVersion = 1

// Error categories shown on the Link page, from the client's side.
const (
	CatDNS          = "dns"
	CatTCP          = "tcp"
	CatTLS          = "tls"
	CatUnauthorized = "unauthorized"
	CatIncompatible = "incompatible"
	CatDuplicate    = "duplicate"
)

// MaxStreamsPerPeer bounds the concurrent streams one peer may hold, the
// control stream and streams closed on one side included. A variable for tests.
var MaxStreamsPerPeer = 1024

// Limits of a session.
const (
	keepAlive = 15 * time.Second
	// deadAfter is when a session with no answer to its pings is closed: one
	// keepalive interval plus yamux's wait for the answer.
	deadAfter      = 45 * time.Second
	handshakeLimit = 10 * time.Second
	replyTimeout   = 15 * time.Second
	maxMessage     = 16 << 10
)

// muxConfig is the yamux configuration of both ends.
func muxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.EnableKeepAlive = true
	c.KeepAliveInterval = keepAlive
	// An unanswered keepalive ping fails after ConnectionWriteTimeout, which
	// closes the session: 15 s + 30 s = dead after 45 s.
	c.ConnectionWriteTimeout = deadAfter - keepAlive
	c.StreamOpenTimeout = replyTimeout
	// A closed stream whose other side never closes is reset after this, so
	// refused streams do not hold the session's per-peer budget for long.
	c.StreamCloseTimeout = 30 * time.Second
	c.LogOutput = muxLog{}
	return c
}

// muxLog sends yamux's own log lines to slog at debug level. They carry
// stream IDs and transport errors, never stream bytes.
type muxLog struct{}

func (muxLog) Write(p []byte) (int, error) {
	slog.Debug("link: yamux", "msg", strings.TrimSpace(string(p)))
	return len(p), nil
}

// helloHeader opens the control stream.
type helloHeader struct {
	Proto        int    `json:"proto"`
	KrillVersion string `json:"krill_version"`
}

// pairHeader opens the pair stream.
type pairHeader struct {
	Secret string `json:"secret"`
	CSR    string `json:"csr"`
}

// message is one JSON line on the control and pair streams.
type message struct {
	Type         string `json:"type"` // hello | error | renew | cert
	Proto        int    `json:"proto,omitempty"`
	KrillVersion string `json:"krill_version,omitempty"`
	Category     string `json:"category,omitempty"`
	Message      string `json:"message,omitempty"`
	CSR          string `json:"csr,omitempty"`
	Cert         string `json:"cert,omitempty"`
}

// Pairing refusals, in the pair stream's error category.
const (
	pairRefused     = "refused"
	pairRateLimited = "rate_limited"
	pairPeerExists  = "peer_exists"
	pairInternal    = "internal"
)

func writeMessage(c net.Conn, m message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := c.SetWriteDeadline(time.Now().Add(replyTimeout)); err != nil {
		return err
	}
	_, err = c.Write(append(b, '\n'))
	if err == nil {
		err = c.SetWriteDeadline(time.Time{})
	}
	return err
}

// readMessage reads one JSON line of at most maxMessage bytes.
func readMessage(br *bufio.Reader) (message, error) {
	line, err := br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return message{}, errors.New("control message too large")
	}
	if err != nil {
		return message{}, err
	}
	var m message
	if err := json.Unmarshal(line, &m); err != nil {
		return message{}, fmt.Errorf("bad control message: %w", err)
	}
	return m, nil
}

func newMessageReader(r io.Reader) *bufio.Reader { return bufio.NewReaderSize(r, maxMessage) }

// pinnedTLS is the client's TLS configuration: the relay's certificate must
// chain to the CA whose SHA-256 the pairing code carried, for the link host —
// system roots play no part.
func pinnedTLS(sni, caSHA string, id *tls.Certificate) *tls.Config {
	cfg := &tls.Config{
		ServerName: sni,
		MinVersion: tls.VersionTLS13,
		// Verification is done by VerifyConnection against the pinned CA.
		InsecureSkipVerify: true, //nolint:gosec
		VerifyConnection: func(cs tls.ConnectionState) error {
			if err := verifyPinned(cs.PeerCertificates, sni, caSHA); err != nil {
				return &pinError{err: err}
			}
			return nil
		},
	}
	if id != nil {
		cfg.Certificates = []tls.Certificate{*id}
	}
	return cfg
}

func verifyPinned(chain []*x509.Certificate, sni, caSHA string) error {
	if len(chain) < 2 {
		return errors.New("relay sent no CA certificate")
	}
	var ca *x509.Certificate
	for _, c := range chain[1:] {
		if Fingerprint(c.Raw) == caSHA {
			ca = c
		}
	}
	if ca == nil {
		return errors.New("relay CA does not match the pairing code")
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	_, err := chain[0].Verify(x509.VerifyOptions{DNSName: sni, Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	return err
}

// pinError is a relay certificate that fails the pinned verification: a TLS
// failure, whatever the cause below.
type pinError struct{ err error }

func (e *pinError) Error() string { return "relay certificate: " + e.err.Error() }
func (e *pinError) Unwrap() error { return e.err }

// tlsCert builds a tls.Certificate from a pair, with extra chain certificates.
func tlsCert(p KeyPair, chain ...string) (tls.Certificate, error) {
	pemBlock := p.CertPEM
	for _, c := range chain {
		pemBlock += c
	}
	return tls.X509KeyPair([]byte(pemBlock), []byte(p.KeyPEM))
}

// category classifies a client-side failure for the page.
func category(err error) string {
	var dns *net.DNSError
	var alert tls.AlertError
	var verify *tls.CertificateVerificationError
	var rec tls.RecordHeaderError
	var pin *pinError
	switch {
	case errors.As(err, &pin):
		return CatTLS
	case errors.As(err, &dns):
		return CatDNS
	case errors.As(err, &alert), errors.As(err, &verify), errors.As(err, &rec):
		return CatTLS
	case strings.Contains(err.Error(), "tls:"):
		return CatTLS
	}
	return CatTCP
}

// acceptWithin accepts one stream or gives up after d.
func acceptWithin(mux *yamux.Session, d time.Duration) (*yamux.Stream, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return mux.AcceptStreamWithContext(ctx)
}
