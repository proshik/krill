package auth

import "testing"

func TestNewTokenUnique(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	b, _ := NewToken()
	if a == b {
		t.Error("tokens must be unique")
	}
	if len(a) < 32 {
		t.Errorf("token too short: %d", len(a))
	}
}
