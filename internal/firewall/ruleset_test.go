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

// Every cluster address went into an `ip saddr` set, which is IPv4-only: one
// IPv6 node made nft reject the rule, the ruleset failed to load, and the
// dead-man switch reverted — a lockdown that silently never applied.
func TestBuildWorkerRulesetSplitsAddressFamilies(t *testing.T) {
	rs, err := BuildWorkerRuleset([]string{"10.0.0.1", "2001:db8::1"})
	if err != nil {
		t.Fatalf("BuildWorkerRuleset: %v", err)
	}
	if !strings.Contains(rs, "ip saddr { 10.0.0.1 }") {
		t.Errorf("IPv4 rule missing or malformed:\n%s", rs)
	}
	if !strings.Contains(rs, "ip6 saddr { 2001:db8::1 }") {
		t.Errorf("IPv6 rule missing or malformed:\n%s", rs)
	}
	for _, line := range strings.Split(rs, "\n") {
		if strings.Contains(line, "ip saddr") && !strings.Contains(line, "ip6 saddr") && strings.Contains(line, "2001:db8") {
			t.Errorf("IPv6 address placed in an IPv4 set: %q", line)
		}
	}
}

// A cluster with no IPv4 members must not emit an empty `ip saddr { }` set.
func TestBuildWorkerRulesetIPv6Only(t *testing.T) {
	rs, err := BuildWorkerRuleset([]string{"2001:db8::1"})
	if err != nil {
		t.Fatalf("BuildWorkerRuleset: %v", err)
	}
	if strings.Contains(rs, "ip saddr {") && !strings.Contains(rs, "ip6 saddr {") {
		t.Errorf("expected only ip6 rules:\n%s", rs)
	}
	if strings.Contains(rs, "saddr {  }") || strings.Contains(rs, "saddr { }") {
		t.Errorf("empty address set emitted:\n%s", rs)
	}
}
