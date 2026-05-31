package auth

import (
	"crypto/rand"
	"encoding/hex"
)

// NewToken генерирует случайный opaque-токен сессии (64 hex-символа).
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
