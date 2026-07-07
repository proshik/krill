package firewall

import (
	"strings"
	"testing"
)

func TestBuildWorkerRuleset(t *testing.T) {
	if _, err := BuildWorkerRuleset(nil); err == nil {
		t.Fatal("empty cluster IPs must error (would strand the worker off swarm)")
	}
	rs, err := BuildWorkerRuleset([]string{"10.0.0.1", "10.0.0.2"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// SSH accept must appear before the drop policy line so applying never cuts SSH.
	sshIdx := strings.Index(rs, "tcp dport 22 accept")
	dropIdx := strings.Index(rs, "policy drop")
	if sshIdx < 0 || dropIdx < 0 || sshIdx < dropIdx {
		t.Fatalf("ssh-accept must be present and after the chain header; ssh=%d drop=%d\n%s", sshIdx, dropIdx, rs)
	}
	for _, want := range []string{
		"table inet krill",
		"ct state established,related accept",
		`iif "lo" accept`,
		"10.0.0.1, 10.0.0.2",
		"tcp dport { 2377, 7946 } accept",
		"udp dport { 4789, 7946 } accept",
	} {
		if !strings.Contains(rs, want) {
			t.Fatalf("ruleset missing %q\n%s", want, rs)
		}
	}
}
