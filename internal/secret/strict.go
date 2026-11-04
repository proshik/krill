package secret

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"strings"
)

// Seal requires encryption and binds ciphertext to the caller's record identity.
// Unlike Enc, it never stores plaintext when encryption or entropy is unavailable.
// Existing credential storage retains its legacy behavior.
func Seal(value, identity string) (string, error) {
	return seal(value, identity, rand.Reader)
}

const strictPrefix = "enc:bound:v1:"

func seal(value, identity string, entropy io.Reader) (string, error) {
	if gcm == nil {
		return "", errors.New("secret: encryption key required")
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(entropy, nonce); err != nil {
		return "", errors.New("secret: encryption nonce unavailable")
	}
	return strictPrefix + base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(value), []byte(identity))), nil
}

// Open rejects plaintext, corrupt data, and ciphertext belonging to another
// record. Identity must be reconstructed from trusted row IDs, not the payload.
func Open(value, identity string) (string, error) {
	if gcm == nil || !strings.HasPrefix(value, strictPrefix) {
		return "", ErrUndecryptable
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, strictPrefix))
	if err != nil || len(raw) < gcm.NonceSize() {
		return "", ErrUndecryptable
	}
	nonce := raw[:gcm.NonceSize()]
	plain, err := gcm.Open(nil, nonce, raw[gcm.NonceSize():], []byte(identity))
	if err != nil {
		return "", ErrUndecryptable
	}
	return string(plain), nil
}
