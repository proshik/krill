package secret

import (
	"errors"
	"testing"
)

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
		got, err := Dec(enc)
		if err != nil {
			t.Errorf("Dec(%q): unexpected error: %v", enc, err)
		}
		if got != s {
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
	got, err := Dec("hunter2")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if got != "hunter2" {
		t.Error("Dec should be a no-op for plaintext")
	}
}

// Legacy plaintext (no prefix) must pass through even after a key is set.
func TestLegacyPlaintextPassthrough(t *testing.T) {
	Init("a-test-key")
	defer Init("")
	got, err := Dec("legacy-plaintext")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if got != "legacy-plaintext" {
		t.Errorf("legacy plaintext mangled: %q", got)
	}
}

// A value encrypted with one key must NOT silently come back as ciphertext when
// read with another key — that hands the caller a bogus credential it would
// happily inject into an app or use to open a connection.
func TestWrongKeyReportsError(t *testing.T) {
	Init("key-one")
	enc := Enc("secret")
	defer Init("")

	Init("key-two")
	got, err := Dec(enc)
	if !errors.Is(err, ErrUndecryptable) {
		t.Errorf("wrong key: got err %v, want ErrUndecryptable", err)
	}
	if got != "" {
		t.Errorf("wrong key: returned %q, want empty (never the stored ciphertext)", got)
	}

	Init("key-one")
	got, err = Dec(enc)
	if err != nil {
		t.Errorf("right key: unexpected error: %v", err)
	}
	if got != "secret" {
		t.Errorf("right key: got %q, want %q", got, "secret")
	}
}

// An encrypted value read with encryption switched off (KRILL_SECRET_KEY unset
// after having been set) is undecryptable — it must not pass through as-is.
func TestEncryptedValueWithoutKeyReportsError(t *testing.T) {
	Init("a-test-key")
	enc := Enc("hunter2")
	Init("")
	got, err := Dec(enc)
	if !errors.Is(err, ErrUndecryptable) {
		t.Errorf("no key: got err %v, want ErrUndecryptable", err)
	}
	if got != "" {
		t.Errorf("no key: returned %q, want empty", got)
	}
}

// A tagged but malformed payload is a storage-corruption signal, not a password.
func TestCorruptCiphertextReportsError(t *testing.T) {
	Init("a-test-key")
	defer Init("")
	for _, bad := range []string{prefix + "not-valid-base64!!", prefix + "", prefix + "AAAA"} {
		got, err := Dec(bad)
		if !errors.Is(err, ErrUndecryptable) {
			t.Errorf("Dec(%q): got err %v, want ErrUndecryptable", bad, err)
		}
		if got != "" {
			t.Errorf("Dec(%q): returned %q, want empty", bad, got)
		}
	}
}
