// Package api holds the agent-facing operations shared by the REST and MCP
// adapters: identity resolution, tenancy checks and the twelve operations.
package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// TokenPrefix marks a Krill personal access token so a leaked string is
// recognizable in logs and secret scanners.
const TokenPrefix = "krill_pat_"

// lookupPrefixLen is how many characters of the token are stored in clear to
// narrow the lookup before the constant-time hash compare.
const lookupPrefixLen = 8

// GenerateToken mints a new token, returning the plaintext (shown to the user
// exactly once), the lookup prefix, and the hash to persist.
func GenerateToken() (plain, prefix, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", "", err
	}
	// base64url without padding: URL/header safe, denser than hex.
	plain = TokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return plain, PrefixOf(plain), HashToken(plain), nil
}

// HashToken hashes a token for storage. SHA-256 rather than bcrypt on purpose:
// the token is 32 random bytes, so there is nothing to brute-force, and bcrypt
// would add ~100ms to every API call — agents make dozens back to back.
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// TokenMatches reports whether plain hashes to hash, in constant time.
func TokenMatches(hash, plain string) bool {
	if hash == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hash), []byte(HashToken(plain))) == 1
}

// PrefixOf returns the stored lookup prefix for a token: the marker plus the
// first lookupPrefixLen characters of the random part.
func PrefixOf(plain string) string {
	body := strings.TrimPrefix(plain, TokenPrefix)
	if len(body) < lookupPrefixLen {
		return body
	}
	return body[:lookupPrefixLen]
}
