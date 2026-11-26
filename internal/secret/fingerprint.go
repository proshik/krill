package secret

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// fpKey is the subkey Fingerprint signs with: derived from KRILL_SECRET_KEY by
// HMAC under a fixed label, so it is independent of the AES key Enc uses. nil
// when no key is configured.
var fpKey []byte

// fingerprintLabel names the subkey; changing it changes every fingerprint.
const fingerprintLabel = "krill fingerprint subkey v1"

// noKeyID is FingerprintKeyID without a key.
const noKeyID = "sha256"

func initFingerprint(aesKey []byte) {
	fpKey = nil
	if aesKey == nil {
		return
	}
	m := hmac.New(sha256.New, aesKey)
	m.Write([]byte(fingerprintLabel))
	fpKey = m.Sum(nil)
}

// Fingerprint returns a hex digest of data for purpose, so two values can be
// compared without storing either. With KRILL_SECRET_KEY set it is
// HMAC-SHA256 under a subkey of that key: a digest of a short secret (an env
// value) cannot be checked against guesses without the key. Without a key it
// is a plain SHA-256: the inputs are stored in plaintext then anyway.
func Fingerprint(purpose string, data []byte) string {
	msg := make([]byte, 0, len(purpose)+1+len(data))
	msg = append(append(append(msg, purpose...), 0), data...)
	if fpKey == nil {
		sum := sha256.Sum256(msg)
		return hex.EncodeToString(sum[:])
	}
	m := hmac.New(sha256.New, fpKey)
	m.Write(msg)
	return hex.EncodeToString(m.Sum(nil))
}

// FingerprintKeyID names the key Fingerprint uses ("sha256" without one), so
// a digest made under another key (or none) is known to be incomparable
// rather than read as a change. It reveals nothing about the key.
func FingerprintKeyID() string {
	if fpKey == nil {
		return noKeyID
	}
	m := hmac.New(sha256.New, fpKey)
	m.Write([]byte("key id"))
	return hex.EncodeToString(m.Sum(nil))[:16]
}
