package cluster

import (
	"strings"
	"testing"
	"time"
)

func TestJoinCommand(t *testing.T) {
	got := joinCommand("SWMTKN-1-abc", "10.0.0.1:2377")
	want := "docker swarm join --token SWMTKN-1-abc 10.0.0.1:2377"
	if got != want {
		t.Fatalf("joinCommand = %q, want %q", got, want)
	}
}

func TestHostKeyMatches(t *testing.T) {
	if !hostKeyMatches("", "AAAA") {
		t.Error("empty stored key must be accepted (accept-new)")
	}
	if !hostKeyMatches("AAAA", "AAAA") {
		t.Error("matching key must be accepted")
	}
	if hostKeyMatches("AAAA", "BBBB") {
		t.Error("mismatched key must be rejected")
	}
}

func TestDialVerifiedRequiresKey(t *testing.T) {
	// A malformed private key must fail fast before any network use.
	_, err := DialVerified(JoinSpec{Host: "127.0.0.1", User: "x", PrivateKey: []byte("not-a-key"), HostKey: "k"}, time.Second)
	if err == nil {
		t.Fatal("expected parse error for bad key")
	}
}

func TestDialVerifiedRejectsEmptyHostKey(t *testing.T) {
	// DialVerified is the strict variant: an empty HostKey must be rejected
	// immediately (before any network dial) to prevent silently accepting any key.
	_, err := DialVerified(JoinSpec{Host: "h", User: "u", PrivateKey: []byte("k"), HostKey: ""}, time.Second)
	if err == nil {
		t.Fatal("expected error for empty host key")
	}
	if !containsStr(err.Error(), "host key") {
		t.Fatalf("error should mention host key, got: %v", err)
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || strings.Contains(s, sub))
}
