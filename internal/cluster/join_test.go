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
