package cluster

import "testing"

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
	_, err := DialVerified(JoinSpec{Host: "127.0.0.1", User: "x", PrivateKey: []byte("not-a-key"), HostKey: "k"})
	if err == nil {
		t.Fatal("expected parse error for bad key")
	}
}
