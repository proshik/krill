package api

import "testing"

func TestGenerateTokenShape(t *testing.T) {
	plain, prefix, hash, err := GenerateToken()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(plain) <= len(TokenPrefix) {
		t.Fatalf("token too short: %q", plain)
	}
	if plain[:len(TokenPrefix)] != TokenPrefix {
		t.Fatalf("want %q prefix, got %q", TokenPrefix, plain)
	}
	if len(prefix) != 8 {
		t.Fatalf("want 8-char lookup prefix, got %q", prefix)
	}
	if prefix != PrefixOf(plain) {
		t.Fatalf("PrefixOf disagrees: %q vs %q", PrefixOf(plain), prefix)
	}
	if hash == plain {
		t.Fatal("hash must not equal the plaintext token")
	}
}

func TestGenerateTokenIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		plain, _, _, err := GenerateToken()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if seen[plain] {
			t.Fatal("duplicate token generated")
		}
		seen[plain] = true
	}
}

func TestTokenMatches(t *testing.T) {
	plain, _, hash, err := GenerateToken()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !TokenMatches(hash, plain) {
		t.Fatal("matching token rejected")
	}
	if TokenMatches(hash, plain+"x") {
		t.Fatal("altered token accepted")
	}
	if TokenMatches("", plain) {
		t.Fatal("empty hash accepted")
	}
}
