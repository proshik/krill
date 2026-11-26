package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Gateway is what Krill knows about its own gateway (Traefik on the manager
// nodes, publishing 80/443 in host mode). The server implements it.
type Gateway interface {
	// Addresses are the IPs the gateway can be reached at as far as Krill
	// knows; empty when it does not know them.
	Addresses(ctx context.Context) ([]string, error)
	// DialAddr is the host a TLS check connects to: always Krill's own
	// gateway, never an address taken from DNS.
	DialAddr() string
}

// DomainStatusResult is the DNS and certificate state of an app's domains.
type DomainStatusResult struct {
	App string `json:"app"`
	// GatewayKnown: Krill knows its gateway's addresses, so each resolved
	// domain says whether it points there. The addresses themselves are not
	// returned: node addresses are operator data, not an organization's.
	GatewayKnown bool          `json:"gateway_known"`
	Domains      []DomainCheck `json:"domains"`
}

// DomainCheck is one domain's state.
type DomainCheck struct {
	Host    string   `json:"host"`
	Exposed bool     `json:"exposed"`
	TLS     bool     `json:"tls"`
	DNS     DNSCheck `json:"dns"`
	// PointsAtGateway: some address the host resolves to is one of the
	// gateway's. Absent when the gateway's addresses are unknown or DNS
	// failed.
	PointsAtGateway *bool `json:"points_at_gateway,omitempty"`
	// Certificate is what the gateway serves for the host; only for a TLS
	// domain.
	Certificate *CertCheck `json:"certificate,omitempty"`
}

// DNSCheck is what the control plane's resolver answers for a host.
type DNSCheck struct {
	Addresses []string `json:"addresses,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// CertCheck is the certificate the gateway presented for a host.
type CertCheck struct {
	Subject   string `json:"subject,omitempty"`
	Issuer    string `json:"issuer,omitempty"`
	NotBefore string `json:"not_before,omitempty"`
	NotAfter  string `json:"not_after,omitempty"`
	DaysLeft  int    `json:"days_left"`
	// Trusted: the chain verifies against the system roots for this host.
	Trusted bool `json:"trusted"`
	// VerifyError says why it is not trusted.
	VerifyError string `json:"verify_error,omitempty"`
	// DefaultCert: Traefik's own self-signed certificate — no certificate
	// was issued for the host (yet), or no router serves it.
	DefaultCert bool `json:"default_cert"`
	// Error: the handshake itself failed; the other fields are empty.
	Error string `json:"error,omitempty"`
}

// traefikDefaultCN is the subject of the certificate Traefik serves when it
// has none for a name.
const traefikDefaultCN = "TRAEFIK DEFAULT CERT"

const (
	domainLookupTimeout = 3 * time.Second
	domainTLSTimeout    = 5 * time.Second
	domainStatusBudget  = 15 * time.Second
	domainParallel      = 4
)

// domainProbe is how DomainStatus resolves names and fetches certificates;
// a field so tests can stand in for the network.
type domainProbe struct {
	lookup func(ctx context.Context, host string) ([]string, error)
	certs  func(ctx context.Context, addr, serverName string) ([]*x509.Certificate, error)
	roots  *x509.CertPool // nil: the system roots
	now    func() time.Time
}

func systemDomainProbe() domainProbe {
	return domainProbe{
		lookup: func(ctx context.Context, host string) ([]string, error) {
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			out := make([]string, 0, len(addrs))
			for _, a := range addrs {
				out = append(out, a.IP.String())
			}
			return out, nil
		},
		certs: func(ctx context.Context, addr, serverName string) ([]*x509.Certificate, error) {
			d := tls.Dialer{
				NetDialer: &net.Dialer{},
				// Verification is done separately (CertCheck.Trusted): the
				// point is to see whatever the gateway serves, the default
				// certificate included.
				Config: &tls.Config{ServerName: serverName, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}, //nolint:gosec // inspected, then verified below
			}
			conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(addr, "443"))
			if err != nil {
				return nil, err
			}
			defer conn.Close()
			tc, ok := conn.(*tls.Conn)
			if !ok {
				return nil, errors.New("not a TLS connection")
			}
			return tc.ConnectionState().PeerCertificates, nil
		},
		now: time.Now,
	}
}

// DomainStatus checks each domain of an app: what DNS resolves it to,
// whether that is Krill's gateway, and which certificate the gateway serves
// for it. Only the app's own hosts are looked up, and the only connection
// made goes to the gateway's address from configuration, with the host as
// SNI — never to an address taken from DNS.
func (s *Service) DomainStatus(ctx context.Context, id Identity, ref string) (DomainStatusResult, error) {
	app, err := s.resolveApp(ctx, id, ref)
	if err != nil {
		return DomainStatusResult{}, err
	}
	p, err := s.appPath(ctx, app)
	if err != nil {
		return DomainStatusResult{}, err
	}
	doms, err := s.q.ListDomainsByApplication(ctx, app.ID)
	if err != nil {
		return DomainStatusResult{}, fmt.Errorf("domain status: list domains: %w", err)
	}
	out := DomainStatusResult{App: p, Domains: make([]DomainCheck, len(doms))}
	ctx, cancel := context.WithTimeout(ctx, domainStatusBudget)
	defer cancel()

	gw := map[string]bool{}
	dialAddr := ""
	if s.gateway != nil {
		dialAddr = s.gateway.DialAddr()
		addrs, err := s.gateway.Addresses(ctx)
		if err != nil {
			slog.Warn("domain status: gateway addresses unavailable", "app_id", app.ID, "err", err)
		}
		for _, a := range addrs {
			if ip := net.ParseIP(a); ip != nil {
				gw[ip.String()] = true
			}
		}
		out.GatewayKnown = len(gw) > 0
	}

	sem := make(chan struct{}, domainParallel)
	var wg sync.WaitGroup
	for i, d := range doms {
		out.Domains[i] = DomainCheck{Host: d.Host, Exposed: d.Exposed, TLS: d.Tls}
		wg.Add(1)
		go func(c *DomainCheck) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				c.DNS.Error = "not checked: out of time"
				return
			}
			defer func() { <-sem }()
			s.checkDomain(ctx, c, gw, dialAddr)
		}(&out.Domains[i])
	}
	wg.Wait()
	return out, nil
}

func (s *Service) checkDomain(ctx context.Context, c *DomainCheck, gw map[string]bool, dialAddr string) {
	lctx, cancel := context.WithTimeout(ctx, domainLookupTimeout)
	addrs, err := s.domains.lookup(lctx, c.Host)
	cancel()
	if err != nil {
		c.DNS.Error = err.Error()
	} else {
		c.DNS.Addresses = addrs
		if len(gw) > 0 {
			hit := false
			for _, a := range addrs {
				if ip := net.ParseIP(a); ip != nil && gw[ip.String()] {
					hit = true
				}
			}
			c.PointsAtGateway = &hit
		}
	}
	if !c.TLS || dialAddr == "" {
		return
	}
	tctx, cancel := context.WithTimeout(ctx, domainTLSTimeout)
	defer cancel()
	chain, err := s.domains.certs(tctx, dialAddr, c.Host)
	if err != nil {
		c.Certificate = &CertCheck{Error: "TLS handshake with the gateway failed: " + handshakeError(err, dialAddr)}
		return
	}
	c.Certificate = s.inspectChain(chain, c.Host)
}

// inspectChain describes the leaf certificate and verifies the chain for
// host.
func (s *Service) inspectChain(chain []*x509.Certificate, host string) *CertCheck {
	if len(chain) == 0 {
		return &CertCheck{Error: "the gateway presented no certificate"}
	}
	leaf := chain[0]
	now := s.domains.now()
	cc := &CertCheck{
		Subject:     leaf.Subject.CommonName,
		Issuer:      issuerName(leaf),
		NotBefore:   formatTime(leaf.NotBefore),
		NotAfter:    formatTime(leaf.NotAfter),
		DaysLeft:    int(math.Floor(leaf.NotAfter.Sub(now).Hours() / 24)),
		DefaultCert: leaf.Subject.CommonName == traefikDefaultCN,
	}
	inter := x509.NewCertPool()
	for _, c := range chain[1:] {
		inter.AddCert(c)
	}
	_, err := leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: s.domains.roots, Intermediates: inter, CurrentTime: now})
	cc.Trusted = err == nil
	if err != nil {
		cc.VerifyError = err.Error()
	}
	return cc
}

// handshakeError describes a failed handshake in words of its own, without
// the address or host name dialed (the gateway's address is operator data)
// and without a resolver's address.
func handshakeError(err error, dialAddr string) string {
	var dnsErr *net.DNSError
	var op *net.OpError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.As(err, &dnsErr):
		return "the gateway's address does not resolve"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "the gateway is unreachable"
	case errors.As(err, &op):
		// A TLS-level failure (tls: …) carries no address; anything else is
		// not described further.
		if op.Op != "dial" && op.Err != nil {
			return scrub(op.Err.Error(), dialAddr)
		}
		return "could not connect"
	}
	return scrub(err.Error(), dialAddr)
}

// scrub removes the dialed address from a message, in case a library put it
// there.
func scrub(msg, dialAddr string) string {
	if dialAddr != "" {
		msg = strings.ReplaceAll(msg, dialAddr, "the gateway")
	}
	return msg
}

func issuerName(c *x509.Certificate) string {
	parts := []string{}
	if c.Issuer.CommonName != "" {
		parts = append(parts, c.Issuer.CommonName)
	}
	if len(c.Issuer.Organization) > 0 {
		parts = append(parts, strings.Join(c.Issuer.Organization, ", "))
	}
	return strings.Join(parts, " / ")
}
