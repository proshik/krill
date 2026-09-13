// Package panel puts the Krill UI itself behind the Traefik gateway, on a
// domain with a Let's Encrypt certificate.
//
// Krill runs as a host process, not as a Swarm service, so the swarm provider
// that routes to apps by service labels cannot see it. Instead the gateway
// polls Krill for the panel's routes through Traefik's HTTP provider
// (ProviderPath): Krill stays the single source of truth for the setting, which
// lives in the database, survives restarts and gateway reconciliation, and
// changes without redeploying Traefik.
//
// Two secrets keep that channel honest, both derived from one stored random
// value (see Tokens). The provider token authenticates Traefik's poll. The
// forwarded token is a request header the gateway adds to every request it
// proxies to the panel; it is how Krill tells a request that came through
// Traefik over HTTPS (whose X-Forwarded-For and X-Forwarded-Proto Traefik set
// itself) from one sent straight to the UI port, which may carry anything.
// They must differ: the forwarded token rides on every proxied request, so a
// single shared token would let anyone fetch the gateway's configuration
// through the gateway.
package panel

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ProviderPath is the endpoint Traefik's HTTP provider polls for the panel's
// dynamic configuration.
const ProviderPath = "/_krill/gateway/config"

// ProviderHeader carries the provider token on Traefik's poll.
const ProviderHeader = "X-Krill-Gateway-Provider"

// ForwardedHeader carries the forwarded token on every request the gateway
// proxies to the panel.
const ForwardedHeader = "X-Krill-Gateway"

// States of the panel domain.
const (
	StateOff     = "off"
	StatePending = "pending" // routed, but not yet proven by a request through it
	StateActive  = "active"  // an operator reached the panel through the domain over HTTPS
)

// routerPriority puts the panel's routers above any app router for the same
// host. Traefik's default priority is the rule's length, so a tenant that adds
// the panel's host to an app could otherwise tie with the panel and win the
// admin's login form. The ACME challenge router sits higher still.
const routerPriority = 1_000_000

// Tokens are the two secrets derived from the stored gateway secret.
type Tokens struct {
	Provider  string
	Forwarded string
}

// DeriveTokens derives both tokens from the stored secret. Deriving rather than
// storing them keeps one value to rotate and guarantees they never coincide.
func DeriveTokens(secret string) Tokens {
	return Tokens{Provider: derive(secret, "provider"), Forwarded: derive(secret, "forwarded")}
}

func derive(secret, label string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("krill-gateway/" + label))
	return hex.EncodeToString(m.Sum(nil))
}

// Matches reports, in constant time, whether got equals want. An empty want
// never matches, so an unwired token cannot be satisfied by an empty header.
func Matches(got, want string) bool {
	if want == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// ErrAdvertiseUnset means the gateway has no address to reach Krill at.
var ErrAdvertiseUnset = errors.New("KRILL_ADVERTISE_ADDR is not set")

// ErrListenLoopback means Krill listens on loopback only, which the gateway's
// container cannot reach.
var ErrListenLoopback = errors.New("KRILL_LISTEN_ADDR binds to loopback only")

// Upstream is the URL the gateway uses to reach Krill: the control plane's
// advertise address and the port Krill listens on. The gateway runs in a
// container, so "localhost" there is the container itself — the host has to be
// addressed by an address it actually answers on.
func Upstream(advertiseAddr, listenAddr string) (string, error) {
	adv := strings.TrimSpace(advertiseAddr)
	if adv == "" {
		return "", ErrAdvertiseUnset
	}
	host, portStr, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("parse KRILL_LISTEN_ADDR %q: %w", listenAddr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("KRILL_LISTEN_ADDR %q: invalid port %q", listenAddr, portStr)
	}
	if host == "localhost" {
		return "", ErrListenLoopback
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return "", ErrListenLoopback
	}
	return "http://" + net.JoinHostPort(adv, portStr), nil
}

var hostLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ErrInvalidHost is returned by NormalizeHost for anything that is not a plain
// DNS name.
var ErrInvalidHost = errors.New("not a valid domain name")

// NormalizeHost validates a panel domain and returns it in canonical form
// (lower case, no trailing dot). It accepts a bare DNS name only: no scheme,
// port, path, wildcard or IP literal — Let's Encrypt issues HTTP-01
// certificates for names, and the value ends up inside a Traefik rule, so
// anything looser is either useless or an injection.
func NormalizeHost(s string) (string, error) {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
	if h == "" || len(h) > 253 || net.ParseIP(h) != nil {
		return "", ErrInvalidHost
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return "", ErrInvalidHost
	}
	for _, l := range labels {
		if !hostLabel.MatchString(l) {
			return "", ErrInvalidHost
		}
	}
	// The top-level label of a real name is never all digits.
	if _, err := strconv.Atoi(labels[len(labels)-1]); err == nil {
		return "", ErrInvalidHost
	}
	return h, nil
}

// Settings is the part of the stored panel configuration the gateway routes by.
type Settings struct {
	Host       string
	State      string
	AllowedIPs []string // CIDR/IP entries restricting the browser UI; empty = anyone
}

// machinePaths are served without the IP allowlist. Each authenticates its
// callers on its own (webhook secrets, API bearer tokens), and those callers —
// GitHub, CI runners, agents — do not come from the operator's addresses.
var machinePaths = []string{"/webhooks/", "/api/", "/mcp"}

// DynamicConfig returns the Traefik dynamic configuration for the panel, in the
// shape the HTTP provider expects. With the domain off it is empty, which
// removes any panel route the gateway still holds.
//
// Pending and active domains are routed identically: pending only means Krill
// has not yet seen proof that the route works, and the proof is a request
// through it.
func DynamicConfig(s Settings, upstream string, forwardedToken string) map[string]any {
	if s.State == StateOff || s.Host == "" || upstream == "" {
		return map[string]any{}
	}
	rule := fmt.Sprintf("Host(`%s`)", s.Host)
	middlewares := map[string]any{
		"krill-panel-redirect": map[string]any{
			"redirectScheme": map[string]any{"scheme": "https"},
		},
		"krill-panel-forwarded": map[string]any{
			"headers": map[string]any{
				"customRequestHeaders": map[string]any{ForwardedHeader: forwardedToken},
			},
		},
	}
	uiMiddlewares := []string{"krill-panel-forwarded"}
	routers := map[string]any{
		// Plain HTTP only redirects. Traefik's ACME challenge router outranks it,
		// so certificate issuance still works.
		"krill-panel-http": map[string]any{
			"rule":        rule,
			"entryPoints": []string{"web"},
			"middlewares": []string{"krill-panel-redirect"},
			"service":     "krill-panel",
			"priority":    routerPriority,
		},
	}
	if len(s.AllowedIPs) > 0 {
		middlewares["krill-panel-ipallow"] = map[string]any{
			"ipAllowList": map[string]any{"sourceRange": s.AllowedIPs},
		}
		uiMiddlewares = []string{"krill-panel-ipallow", "krill-panel-forwarded"}
		prefixes := make([]string, 0, len(machinePaths))
		for _, p := range machinePaths {
			prefixes = append(prefixes, fmt.Sprintf("PathPrefix(`%s`)", p))
		}
		routers["krill-panel-machine"] = map[string]any{
			"rule":        rule + " && (" + strings.Join(prefixes, " || ") + ")",
			"entryPoints": []string{"websecure"},
			"middlewares": []string{"krill-panel-forwarded"},
			"service":     "krill-panel",
			"priority":    routerPriority + 1,
			"tls":         map[string]any{"certResolver": "le"},
		}
	}
	routers["krill-panel-https"] = map[string]any{
		"rule":        rule,
		"entryPoints": []string{"websecure"},
		"middlewares": uiMiddlewares,
		"service":     "krill-panel",
		"priority":    routerPriority,
		"tls":         map[string]any{"certResolver": "le"},
	}
	return map[string]any{
		"http": map[string]any{
			"routers":     routers,
			"middlewares": middlewares,
			"services": map[string]any{
				"krill-panel": map[string]any{
					"loadBalancer": map[string]any{
						"servers":        []map[string]any{{"url": upstream}},
						"passHostHeader": true,
					},
				},
			},
		},
	}
}

// ViaGateway reports whether r was proxied by the gateway: it carries the
// forwarded token, which only the gateway's panel middleware adds.
func ViaGateway(r *http.Request, forwardedToken string) bool {
	return Matches(r.Header.Get(ForwardedHeader), forwardedToken)
}

// SecureViaGateway reports whether r reached the gateway over HTTPS. Traefik
// strips a client-supplied X-Forwarded-Proto and sets its own, so the header
// is trustworthy exactly when the request came through the gateway.
func SecureViaGateway(r *http.Request, forwardedToken string) bool {
	return ViaGateway(r, forwardedToken) && r.Header.Get("X-Forwarded-Proto") == "https"
}

// RequestHost is r's host without a port, lower-cased.
func RequestHost(r *http.Request) string {
	h := r.Host
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	return strings.TrimSuffix(strings.ToLower(h), ".")
}

// CheckCertificate verifies that the gateway at addr serves host a certificate
// the system trusts. Until Let's Encrypt has issued one, Traefik answers with
// its self-signed default, which a browser lets an operator click through — so
// an operator's request alone does not prove the certificate is real.
//
// It says nothing about reachability: dialing the host's own address from the
// host goes through loopback and succeeds even when the outside world is cut
// off. The operator's request through the domain is the reachability proof;
// this only checks what that request cannot.
func CheckCertificate(ctx context.Context, addr, host string) error {
	d := tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 5 * time.Second},
		Config:    &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12},
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := d.DialContext(cctx, "tcp", net.JoinHostPort(addr, "443"))
	if err != nil {
		return err
	}
	return conn.Close()
}
