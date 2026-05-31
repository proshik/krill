package auth

import "testing"

func TestHashAndCheck(t *testing.T) {
	h, err := HashPassword("secret")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if h == "secret" || h == "" {
		t.Fatal("hash looks wrong")
	}
	if !CheckPassword(h, "secret") {
		t.Error("CheckPassword should accept correct password")
	}
	if CheckPassword(h, "wrong") {
		t.Error("CheckPassword should reject wrong password")
	}
}
