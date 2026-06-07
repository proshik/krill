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
	"io"
	"strings"
)

const prefix = "enc:v1:"

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
func Dec(s string) string {
	if !strings.HasPrefix(s, prefix) || gcm == nil {
		return s
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, prefix))
	if err != nil || len(raw) < gcm.NonceSize() {
		return s
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return s
	}
	return string(pt)
}
