package link

import (
	"errors"
	"fmt"
	"net"
	"strconv"

	"github.com/proshik/krill/internal/panel"
)

// Names of the link's objects in the gateway's dynamic configuration.
const (
	gatewayRouter    = "krill-link"
	gatewayService   = "krill-link"
	gatewayTransport = "krill-link-gateway"
)

// ErrLinkListenLoopback means KRILL_LINK_LISTEN_ADDR binds to loopback only,
// which the gateway's container cannot reach.
var ErrLinkListenLoopback = errors.New("KRILL_LINK_LISTEN_ADDR binds to loopback only")

// ListenPort is the port of KRILL_LINK_LISTEN_ADDR.
func ListenPort(linkListenAddr string) (int, error) {
	_, portStr, err := net.SplitHostPort(linkListenAddr)
	if err != nil {
		return 0, fmt.Errorf("parse KRILL_LINK_LISTEN_ADDR %q: %w", linkListenAddr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("KRILL_LINK_LISTEN_ADDR %q: invalid port %q", linkListenAddr, portStr)
	}
	return port, nil
}

// Upstream is the address the gateway forwards the link host to:
// KRILL_ADVERTISE_ADDR and the port of KRILL_LINK_LISTEN_ADDR. A relay needs
// what the panel domain needs — a working HTTP provider (panel.Upstream: an
// advertise address, a non-loopback UI listener) — and a link listener the
// gateway's container can reach. The panel domain itself need not be on.
func Upstream(advertiseAddr, listenAddr, linkListenAddr string) (string, error) {
	if _, err := panel.Upstream(advertiseAddr, listenAddr); err != nil {
		return "", err
	}
	host, _, err := net.SplitHostPort(linkListenAddr)
	if err != nil {
		return "", fmt.Errorf("parse KRILL_LINK_LISTEN_ADDR %q: %w", linkListenAddr, err)
	}
	port, err := ListenPort(linkListenAddr)
	if err != nil {
		return "", err
	}
	if host == "localhost" {
		return "", ErrLinkListenLoopback
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return "", ErrLinkListenLoopback
	}
	return net.JoinHostPort(advertiseAddr, strconv.Itoa(port)), nil
}

// GatewayConfig is the relay's tcp section of the gateway's dynamic
// configuration: a passthrough router for the link host on websecure, to the
// link listener through a transport that sends a PROXY v2 header and then
// opens TLS with the gateway's client certificate (inline: the HTTP provider's
// answer is behind the provider token). No middlewares: TLS stays end to end
// between the client and Krill.
func GatewayConfig(linkHost, upstream string, local LocalMaterial) map[string]any {
	return map[string]any{
		"routers": map[string]any{
			gatewayRouter: map[string]any{
				"rule":        fmt.Sprintf("HostSNI(`%s`)", linkHost),
				"entryPoints": []string{"websecure"},
				"service":     gatewayService,
				"tls":         map[string]any{"passthrough": true},
			},
		},
		"services": map[string]any{
			gatewayService: map[string]any{
				"loadBalancer": map[string]any{
					"servers":          []map[string]any{{"address": upstream, "tls": true}},
					"serversTransport": gatewayTransport,
				},
			},
		},
		"serversTransports": map[string]any{
			gatewayTransport: map[string]any{
				"proxyProtocol": map[string]any{"version": 2},
				"tls": map[string]any{
					"serverName":   ListenerServerName,
					"rootCAs":      []string{local.Listener.CertPEM},
					"certificates": []map[string]any{{"certFile": local.Gateway.CertPEM, "keyFile": local.Gateway.KeyPEM}},
				},
			},
		},
	}
}

// Merge adds the link's tcp section to the panel's configuration. The panel's
// is "{}" while the panel domain is off; its HTTP routers are left as they are.
func Merge(panelCfg map[string]any, tcp map[string]any) map[string]any {
	if tcp == nil {
		return panelCfg
	}
	out := make(map[string]any, len(panelCfg)+1)
	for k, v := range panelCfg {
		out[k] = v
	}
	out["tcp"] = tcp
	return out
}
