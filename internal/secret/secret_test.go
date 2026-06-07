package secret

import "testing"

func TestRoundTrip(t *testing.T) {
	Init("a-test-key")
	defer Init("")
	if !Enabled() {
		t.Fatal("expected enabled")
	}
	for _, s := range []string{"hunter2", "", "with=special:chars/and spaces"} {
		enc := Enc(s)
		if s != "" && enc == s {
			t.Errorf("Enc(%q) returned plaintext", s)
		}
		if got := Dec(enc); got != s {
			t.Errorf("round trip %q -> %q -> %q", s, enc, got)
		}
	}
}

func TestNoKeyIsPlaintext(t *testing.T) {
	Init("")
	if Enabled() {
		t.Fatal("expected disabled")
	}
	if Enc("hunter2") != "hunter2" {
		t.Error("Enc should be a no-op without a key")
	}
	if Dec("hunter2") != "hunter2" {
		t.Error("Dec should be a no-op for plaintext")
	}
}

// Legacy plaintext (no prefix) must pass through even after a key is set.
func TestLegacyPlaintextPassthrough(t *testing.T) {
	Init("a-test-key")
	defer Init("")
	if got := Dec("legacy-plaintext"); got != "legacy-plaintext" {
		t.Errorf("legacy plaintext mangled: %q", got)
	}
}

// A value encrypted with one key is unreadable with another (returns the stored
// form rather than garbage), and decryptable with the right key.
func TestWrongKey(t *testing.T) {
	Init("key-one")
	enc := Enc("secret")
	Init("key-two")
	if Dec(enc) == "secret" {
		t.Error("decrypted with the wrong key")
	}
	Init("key-one")
	if Dec(enc) != "secret" {
		t.Error("failed to decrypt with the right key")
	}
	Init("")
}
