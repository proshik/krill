package auth

import (
	"crypto/rand"
	"encoding/hex"
)

// NewToken generates a random opaque session token (64 hex characters).
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
