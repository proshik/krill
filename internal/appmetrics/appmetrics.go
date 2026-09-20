// Package appmetrics holds the rules of Krill's app-metrics contract that more
// than one layer needs: the bearer token apps are scraped with, and what a
// metrics endpoint may look like. Pure — no database, no docker.
package appmetrics

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
)

const (
	DefaultTokenEnv = "KRILL_METRICS_TOKEN"
	DefaultPath     = "/metrics"
	MaxEndpoints    = 10
	// ContainerLabelTokenHash records, on the app's containers, which token
	// (and variable name) the running deployment was started with. Container
	// labels change only on a deploy — unlike service labels, which domain
	// edits rewrite in place — so it tells "running with the current token"
	// apart from "needs a redeploy".
	ContainerLabelTokenHash = "krill.metrics-token-hash"
)

// NewToken returns 32 random bytes, hex-encoded.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// TokenHash fingerprints the variable name and token together, so renaming
// the variable also reads as "not deployed yet". 8 bytes of sha256 tell
// nothing useful about a 32-byte random token.
func TokenHash(envName, token string) string {
	sum := sha256.Sum256([]byte(envName + "\x00" + token))
	return hex.EncodeToString(sum[:8])
}

func ValidPort(p int) bool { return p >= 1 && p <= 65535 }

// pathRe keeps a path safe inside both a Traefik rule (backticks) and an Alloy
// string (quotes, backslashes): unreserved URL characters and slashes only.
// A bare "/" is refused — hiding it would take the whole site off the domain.
var pathRe = regexp.MustCompile(`^/[A-Za-z0-9._~-][A-Za-z0-9._~/-]{0,127}$`)

// NormalizePath trims spaces and trailing slashes and validates the result.
func NormalizePath(p string) (string, bool) {
	p = strings.TrimRight(strings.TrimSpace(p), "/")
	if !pathRe.MatchString(p) || strings.Contains(p, "//") {
		return "", false
	}
	return p, true
}

var jobRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{0,64}$`)

// ValidJob accepts "" (the collector then uses the app's name).
func ValidJob(j string) bool { return jobRe.MatchString(j) }

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)

func ValidEnvName(n string) bool { return envNameRe.MatchString(n) }

// Endpoint is the part of a metrics endpoint the router rule cares about.
type Endpoint struct {
	Port int32
	Path string
}

// HiddenPaths returns, sorted and deduplicated, the paths of the endpoints
// served on the app's own HTTP port — the port Traefik routes the app's
// domains to, so those paths must be taken off every domain. An endpoint on
// any other port is not routed by Traefik and needs nothing.
func HiddenPaths(appPort int32, eps []Endpoint) []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range eps {
		if e.Port == appPort && !seen[e.Path] {
			seen[e.Path] = true
			out = append(out, e.Path)
		}
	}
	sort.Strings(out)
	return out
}
