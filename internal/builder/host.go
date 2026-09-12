package builder

import (
	"errors"
	"net"
	"net/url"
	"strings"
)

// NormalizeHost brings a stored credential host and a URL host to the same
// shape so they can be compared: lower case, no scheme, no path, port kept.
func NormalizeHost(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, "@"); i >= 0 { // user:pass@host
		s = s[i+1:]
	}
	return s
}

// HostOf returns the normalized host of an http(s) git URL.
func HostOf(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return "", errors.New("git_url has no host")
	}
	return NormalizeHost(u.Host), nil
}

// bareHost strips a ":port" suffix so the result can be handed to
// net.LookupIPAddr (via netguard.CheckHost), which errors on a "host:port"
// string — HostOf/NormalizeHost deliberately keep the port for credential-host
// comparison, so callers that need to resolve the host must strip it first.
// Returns s unchanged when it carries no port (including a bare IPv6 literal,
// which SplitHostPort also rejects without brackets).
func bareHost(s string) string {
	if h, _, err := net.SplitHostPort(s); err == nil {
		return h
	}
	return s
}
