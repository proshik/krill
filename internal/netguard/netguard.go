// Package netguard blocks outbound connections to private / loopback / link-local
// destinations (SSRF egress control) unless private egress is explicitly allowed.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// ErrBlocked is returned when a destination resolves to a blocked address.
var ErrBlocked = errors.New("netguard: destination address is not permitted")

// cgnat is RFC6598 Carrier-Grade NAT space (100.64.0.0/10), used by some cloud
// environments for internal endpoints. Go's net.IP.IsPrivate() does not cover it.
var cgnat = mustCIDR("100.64.0.0/10")

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// IsBlockedIP reports whether ip is a non-public address that must be refused
// when allowPrivate is false: loopback, private (RFC1918 / ULA fc00::/7),
// link-local (169.254/fe80, incl. cloud metadata), CGNAT (100.64.0.0/10), or
// unspecified.
func IsBlockedIP(ip net.IP, allowPrivate bool) bool {
	if allowPrivate {
		return false // egress explicitly permitted everywhere
	}
	if ip == nil {
		return true // unknown/unparseable -> fail closed
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || cgnat.Contains(ip)
}

// resolveAndCheck resolves host and returns the permitted resolved IPs, or an
// error if any resolved IP is blocked (fail-closed against DNS rebinding).
func resolveAndCheck(ctx context.Context, host string, allowPrivate bool) ([]net.IPAddr, error) {
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if IsBlockedIP(ip.IP, allowPrivate) {
			return nil, fmt.Errorf("%w: %s -> %s", ErrBlocked, host, ip.IP)
		}
	}
	return ips, nil
}

// CheckHost resolves host and errors if it maps to any blocked address.
func CheckHost(ctx context.Context, host string, allowPrivate bool) error {
	if allowPrivate {
		return nil
	}
	_, err := resolveAndCheck(ctx, host, allowPrivate)
	return err
}

// DialContext returns a dialer that resolves the host, refuses blocked IPs, and
// dials the checked IP directly (so the connection can't be rebound to a blocked
// address between check and dial). When allowPrivate is true there is nothing to
// guard against, so it delegates straight to the base dialer's normal
// Happy-Eyeballs / RFC6555 dual-stack fallback instead of pinning to one IP.
func DialContext(allowPrivate bool) func(ctx context.Context, network, addr string) (net.Conn, error) {
	base := &net.Dialer{Timeout: 15 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if allowPrivate {
			// No guard needed: fall through to the stdlib dialer's full
			// Happy-Eyeballs / RFC6555 dual-stack fallback instead of
			// re-resolving and pinning to a single (possibly unreachable) IP.
			return base.DialContext(ctx, network, addr)
		}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if ip := net.ParseIP(host); ip != nil {
			if IsBlockedIP(ip, allowPrivate) {
				return nil, fmt.Errorf("%w: %s", ErrBlocked, addr)
			}
			return base.DialContext(ctx, network, addr)
		}
		ips, err := resolveAndCheck(ctx, host, allowPrivate)
		if err != nil {
			return nil, err
		}
		return base.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}
}

// HTTPClient returns an http.Client whose transport enforces the egress guard.
func HTTPClient(allowPrivate bool) *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext:         DialContext(allowPrivate),
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
}
