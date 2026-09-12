package builder

import (
	"errors"
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
