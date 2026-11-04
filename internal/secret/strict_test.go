package secret

import (
	"errors"
	"strings"
	"testing"
)

func TestBoundEncryptionFailsClosed(t *testing.T) {
	Init("snapshot-key")
	t.Cleanup(func() { Init("") })
	value, err := Seal("private-env-value", "app:1/deploy:2")
	if err != nil || strings.Contains(value, "private-env-value") {
		t.Fatalf("seal failed: %v", err)
	}
	if got, err := Open(value, "app:1/deploy:2"); err != nil || got != "private-env-value" {
		t.Fatalf("roundtrip: %v", err)
	}
	for _, tc := range []struct{ cipher, identity string }{{value, "app:3/deploy:2"}, {"private-env-value", "app:1/deploy:2"}, {value + "corrupt", "app:1/deploy:2"}} {
		if _, err := Open(tc.cipher, tc.identity); !errors.Is(err, ErrUndecryptable) {
			t.Fatal("invalid payload accepted")
		}
	}
	if got, err := seal("secret", "identity", strings.NewReader("")); err == nil || got != "" {
		t.Fatal("entropy failure exposed plaintext")
	}
	Init("different-key")
	if _, err := Open(value, "app:1/deploy:2"); err == nil {
		t.Fatal("wrong key accepted")
	}
	Init("")
	if got, err := Seal("secret", "identity"); err == nil || got != "" {
		t.Fatal("missing key exposed plaintext")
	}
	if _, err := Open(value, "app:1/deploy:2"); err == nil {
		t.Fatal("missing key accepted")
	}
}
