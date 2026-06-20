// Package webhook holds the pure, dependency-free helpers for auto-deploy
// webhooks: GitHub push HMAC verification, minimal push-payload parsing, and
// secret/token generation and comparison. No HTTP, no GitHub SDK.
package webhook

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
)

// VerifyHMAC reports whether sigHeader (an "X-Hub-Signature-256" value of the
// form "sha256=<hex>") is a valid HMAC-SHA256 of body keyed by secret.
func VerifyHMAC(secret string, body []byte, sigHeader string) bool {
	const prefix = "sha256="
	if secret == "" || !strings.HasPrefix(sigHeader, prefix) {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(sigHeader, prefix))
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hmac.Equal(want, m.Sum(nil))
}

// PushEvent is the minimal subset of a GitHub push payload we use.
type PushEvent struct {
	Ref     string `json:"ref"`
	Deleted bool   `json:"deleted"`
}

// Branch returns the branch name for a "refs/heads/<name>" ref, else "".
func (e PushEvent) Branch() string {
	const p = "refs/heads/"
	if strings.HasPrefix(e.Ref, p) {
		return strings.TrimPrefix(e.Ref, p)
	}
	return ""
}

// ParsePushEvent decodes a GitHub push payload.
func ParsePushEvent(body []byte) (PushEvent, error) {
	var e PushEvent
	if err := json.Unmarshal(body, &e); err != nil {
		return PushEvent{}, err
	}
	return e, nil
}

// NewSecret returns a fresh 32-byte random secret as 64 hex chars.
func NewSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never returns a short read
	return hex.EncodeToString(b)
}

// ConstantTimeEqual compares two strings without leaking length-independent timing.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

var tagRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidTag guards a user-supplied image tag from the deploy-hook ?tag= param.
func ValidTag(tag string) bool {
	return tag != "" && len(tag) <= 128 && tagRe.MatchString(tag)
}
