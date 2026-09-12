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

// With no service ports the manager ruleset must be the worker ruleset byte for
// byte — the refactor into buildRuleset must not shift anything.
func TestBuildManagerRulesetNoPortsEqualsWorker(t *testing.T) {
	ips := []string{"10.0.0.1", "2001:db8::1"}
	w, err := BuildWorkerRuleset(ips)
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	for _, ports := range [][]int{nil, {}, {22}} {
		m, err := BuildManagerRuleset(ips, ports)
		if err != nil {
			t.Fatalf("manager %v: %v", ports, err)
		}
		if m != w {
			t.Fatalf("manager ruleset with ports %v differs from worker:\n--- worker\n%s--- manager\n%s", ports, w, m)
		}
	}
}

func TestBuildManagerRulesetServicePorts(t *testing.T) {
	rs, err := BuildManagerRuleset([]string{"10.0.0.1"}, []int{8080, 443, 22, 80, 443})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.Contains(rs, "tcp dport { 80, 443, 8080 } accept") {
		t.Fatalf("service ports missing, unsorted or not deduplicated:\n%s", rs)
	}
	if n := strings.Count(rs, "22"); n != 1 {
		t.Fatalf("port 22 must appear exactly once (the anti-lockout rule), got %d:\n%s", n, rs)
	}
	// Service ports are accepted from anywhere, but must still sit after SSH and
	// inside the drop-policy chain.
	sshIdx := strings.Index(rs, "tcp dport 22 accept")
	svcIdx := strings.Index(rs, "tcp dport { 80, 443, 8080 }")
	if sshIdx < 0 || svcIdx < sshIdx {
		t.Fatalf("service ports must follow the SSH rule; ssh=%d svc=%d\n%s", sshIdx, svcIdx, rs)
	}
	for _, want := range []string{"tcp dport { 2377, 7946 } accept", "udp dport { 4789, 7946 } accept", "policy drop"} {
		if !strings.Contains(rs, want) {
			t.Fatalf("manager ruleset lost %q\n%s", want, rs)
		}
	}
}

func TestBuildManagerRulesetDeterministic(t *testing.T) {
	a, err := BuildManagerRuleset([]string{"10.0.0.1"}, []int{8080, 80, 443})
	if err != nil {
		t.Fatalf("build a: %v", err)
	}
	b, err := BuildManagerRuleset([]string{"10.0.0.1"}, []int{443, 8080, 80})
	if err != nil {
		t.Fatalf("build b: %v", err)
	}
	if a != b {
		t.Fatalf("port order leaked into the ruleset:\n%s\n%s", a, b)
	}
}

func TestBuildManagerRulesetRejectsOutOfRangePort(t *testing.T) {
	for _, p := range []int{0, -1, 65536} {
		if _, err := BuildManagerRuleset([]string{"10.0.0.1"}, []int{80, p}); err == nil {
			t.Fatalf("port %d must be rejected", p)
		}
	}
}
