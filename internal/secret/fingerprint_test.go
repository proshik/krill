package secret

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestFingerprintNoKeyIsPlainSHA256(t *testing.T) {
	Init("")
	a := Fingerprint("p", []byte("v"))
	if a != Fingerprint("p", []byte("v")) {
		t.Fatal("not deterministic")
	}
	if a == Fingerprint("q", []byte("v")) {
		t.Fatal("purpose is not part of the digest")
	}
	if FingerprintKeyID() != "sha256" {
		t.Fatalf("key id without a key = %q", FingerprintKeyID())
	}
	sum := sha256.Sum256([]byte("p\x00v"))
	if a != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest without a key = %q, want SHA-256 of purpose NUL data", a)
	}
}

func TestFingerprintKeyedIsStableAndKeyBound(t *testing.T) {
	defer Init("")
	Init("key-one")
	a, idA := Fingerprint("p", []byte("v")), FingerprintKeyID()
	Init("key-one")
	if Fingerprint("p", []byte("v")) != a || FingerprintKeyID() != idA {
		t.Fatal("the same key must derive the same subkey")
	}
	Init("key-two")
	if Fingerprint("p", []byte("v")) == a || FingerprintKeyID() == idA {
		t.Fatal("another key must give other digests and another key id")
	}
	Init("")
	if Fingerprint("p", []byte("v")) == a {
		t.Fatal("the keyed digest must differ from the plain one")
	}
	if len(idA) != 16 || idA == "sha256" {
		t.Fatalf("key id %q", idA)
	}
}
