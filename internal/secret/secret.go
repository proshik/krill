// Package secret provides optional encryption-at-rest for stored credentials
// (managed-DB passwords, registry passwords, destination keys). The key comes
// from KRILL_SECRET_KEY (any string, hashed to a 32-byte AES key). With no key
// configured, values are stored as plaintext (legacy) and a startup warning is
// logged. Encrypted values carry a version prefix, so legacy plaintext values
// still decrypt as-is — enabling encryption is backward compatible.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"strings"
)

const prefix = "enc:v1:"

// ErrUndecryptable reports a stored value that carries the encryption tag but
// cannot be opened: KRILL_SECRET_KEY is unset, was rotated, or the stored bytes
// are corrupt. Callers MUST NOT fall back to the stored form — it is ciphertext,
// not a credential, and using it silently corrupts whatever consumes it (an
// injected app env var, a database connection, an SSH key).
var ErrUndecryptable = errors.New("secret: value is encrypted but cannot be decrypted (missing, rotated, or wrong KRILL_SECRET_KEY)")

var gcm cipher.AEAD // nil when no key is configured

// Init configures the cipher from the key. An empty key disables encryption.
func Init(key string) {
	gcm = nil
	if key == "" {
		return
	}
	sum := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return
	}
	if g, err := cipher.NewGCM(block); err == nil {
		gcm = g
	}
}

// Enabled reports whether encryption is active.
func Enabled() bool { return gcm != nil }

// Enc encrypts s for storage. With no key (or empty s) it returns s unchanged.
func Enc(s string) string {
	if gcm == nil || s == "" {
		return s
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return s // fail to plaintext rather than lose the value
	}
	ct := gcm.Seal(nonce, nonce, []byte(s), nil)
	return prefix + base64.StdEncoding.EncodeToString(ct)
}

// Dec decrypts a stored value. Untagged (legacy plaintext) values pass through
// unchanged, so existing data keeps working when a key is later configured.
//
// A tagged value that cannot be opened returns ("", ErrUndecryptable) rather
// than the stored ciphertext: handing back the raw tagged string would let a
// bogus credential flow into a deploy, a connection, or an app's environment
// with no signal that anything went wrong.
func Dec(s string) (string, error) {
	if !strings.HasPrefix(s, prefix) {
		return s, nil
	}
	if gcm == nil {
		return "", ErrUndecryptable
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, prefix))
	if err != nil || len(raw) < gcm.NonceSize() {
		return "", ErrUndecryptable
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", ErrUndecryptable
	}
	return string(pt), nil
}
