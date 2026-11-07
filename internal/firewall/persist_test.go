package firewall

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildCarriesDeterministicMarker(t *testing.T) {
	a, err := Build(Rules{ClusterIPs: []string{"10.0.0.2", "10.0.0.1"}, PublicTCP: []int{443, 80}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Build(Rules{ClusterIPs: []string{"10.0.0.1", "10.0.0.2", "10.0.0.1"}, PublicTCP: []int{80, 443}})
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("the same cluster in another order must give the same ruleset:\n%s\n%s", a, b)
	}
	id := RulesetID(a)
	if !strings.HasPrefix(id, markerPrefix) || len(id) != len(markerPrefix)+16 {
		t.Fatalf("marker = %q", id)
	}
	c, _ := Build(Rules{ClusterIPs: []string{"10.0.0.1", "10.0.0.3"}, PublicTCP: []int{80, 443}})
	if RulesetID(c) == id {
		t.Fatal("a different ruleset must carry a different marker")
	}
	// nft lists the empty chain back in its own layout; the marker must still
	// be found there.
	listing := "table inet krill {\n\tchain " + id + " {\n\t}\n\n\tchain input {\n\t}\n}\n"
	if got := RulesetID(listing); got != id {
		t.Fatalf("marker from a listing = %q, want %q", got, id)
	}
	if RulesetID("table inet krill {\n\tchain input {\n\t}\n}\n") != "" {
		t.Fatal("a table without a marker has no id")
	}
}

func TestBuildWireGuard(t *testing.T) {
	rs, err := Build(Rules{ClusterIPs: []string{"10.8.0.1", "10.8.0.2"}, WireGuardUDP: []int{51820, 51820}, ClusterIface: "wg0"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"udp dport { 51820 } accept",
		`iifname "wg0" ip saddr { 10.8.0.1, 10.8.0.2 } tcp dport { 2377, 7946 } accept`,
		`iifname "wg0" ip saddr { 10.8.0.1, 10.8.0.2 } udp dport { 4789, 7946 } accept`,
	} {
		if !strings.Contains(rs, want) {
			t.Fatalf("missing %q:\n%s", want, rs)
		}
	}
	// The WireGuard port must be accepted before the drop policy can bite and
	// independently of the source address: a peer's endpoint is not a cluster IP.
	if strings.Contains(rs, "saddr { 10.8.0.1, 10.8.0.2 } udp dport { 51820") {
		t.Fatalf("WireGuard must not be limited to cluster addresses:\n%s", rs)
	}
	if _, err := Build(Rules{ClusterIPs: []string{"10.8.0.1"}, ClusterIface: `wg0" accept; #`}); err == nil {
		t.Fatal("an interface name that leaves the nft string must be refused")
	}
	if _, err := Build(Rules{ClusterIPs: []string{"10.8.0.1"}, WireGuardUDP: []int{70000}}); err == nil {
		t.Fatal("an out-of-range port must be refused")
	}
}

const probeOutput = `nft yes
table yes
live krill_rs_0123456789abcdef
pending no
persisted yes
persisted-id krill_rs_fedcba9876543210
unit yes
iface wg0 10.8.0.2/24
mtu lo 65536
mtu eth0 1500
mtu wg0 1420
mtu veth1a2b 1450
wg wg0
wgtool yes
wgport wg0 51820
`

func TestParseProbe(t *testing.T) {
	h := parseProbe(probeOutput)
	if !h.Nft || !h.Table || h.Pending || !h.Persisted || !h.UnitEnabled {
		t.Fatalf("flags: %+v", h)
	}
	if h.LiveID != "krill_rs_0123456789abcdef" || h.PersistedID != "krill_rs_fedcba9876543210" {
		t.Fatalf("ids: %+v", h)
	}
	if h.SwarmIface == nil || h.SwarmIface.Name != "wg0" || h.SwarmIface.MTU != 1420 || !h.SwarmIface.WireGuard || h.SwarmIface.Net.String() != "10.8.0.0/24" {
		t.Fatalf("swarm iface: %+v", h.SwarmIface)
	}
	if !h.WireGuardPortsKnown || len(h.WireGuardPorts) != 1 || h.WireGuardPorts[0] != 51820 {
		t.Fatalf("wg ports: %+v", h)
	}
}

func TestHostRules(t *testing.T) {
	h := parseProbe(probeOutput)
	ips := func(s ...string) []net.IP {
		var out []net.IP
		for _, v := range s {
			out = append(out, net.ParseIP(v))
		}
		return out
	}
	r, err := h.Rules(Rules{ClusterIPs: []string{"10.8.0.1"}}, ips("10.8.0.1", "10.8.0.2", "10.8.0.3"))
	if err != nil {
		t.Fatal(err)
	}
	if r.ClusterIface != "wg0" || len(r.WireGuardUDP) != 1 {
		t.Fatalf("all peers on the WireGuard subnet: %+v", r)
	}
	// One node reached over the public interface: binding swarm traffic to wg0
	// would cut it off.
	r, err = h.Rules(Rules{ClusterIPs: []string{"10.8.0.1"}}, ips("10.8.0.1", "203.0.113.7"))
	if err != nil {
		t.Fatal(err)
	}
	if r.ClusterIface != "" || len(r.WireGuardUDP) != 1 {
		t.Fatalf("mixed cluster: %+v", r)
	}
	noTool := parseProbe("nft yes\niface wg0 10.8.0.2/24\nmtu wg0 1420\nwg wg0\n")
	if _, err := noTool.Rules(Rules{ClusterIPs: []string{"10.8.0.1"}}, nil); !errors.Is(err, ErrWireGuardPortsUnknown) {
		t.Fatalf("WireGuard without readable ports must refuse, got %v", err)
	}
	plain := parseProbe("nft yes\niface eth0 192.0.2.10/24\nmtu eth0 1500\n")
	r, err = plain.Rules(Rules{ClusterIPs: []string{"192.0.2.10"}}, ips("192.0.2.10", "192.0.2.11"))
	if err != nil || r.ClusterIface != "" || len(r.WireGuardUDP) != 0 {
		t.Fatalf("a node without WireGuard keeps the plain rules: %+v %v", r, err)
	}
}

// persistNode stubs nft/systemctl for persistScript: "table" in dir is what
// `nft list table` prints, and every systemctl call is appended to "calls".
func persistNode(t *testing.T) (dir string, run func() string) {
	t.Helper()
	dir = t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stubs := map[string]string{
		"nft":       `[ "$1" = list ] && [ -e "$STATE/table" ] && exec cat "$STATE/table"; exit 1`,
		"systemctl": `echo "$@" >> "$STATE/calls"`,
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("STATE", dir)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	file := filepath.Join(dir, "etc", "firewall.nft")
	unit := filepath.Join(dir, "krill-firewall.service")
	return dir, func() string {
		out, err := LocalRunner{Timeout: 10 * time.Second}.Run(context.Background(), "", persistScript(file, unit))
		if err != nil {
			t.Fatalf("persist: %v: %s", err, out)
		}
		return out
	}
}

func TestPersistScriptWritesRulesetAndUnit(t *testing.T) {
	dir, run := persistNode(t)
	table := "table inet krill {\n\tchain krill_rs_0123456789abcdef {\n\t}\n}\n"
	os.WriteFile(filepath.Join(dir, "table"), []byte(table), 0o644)
	if out := run(); !strings.Contains(out, "persisted") {
		t.Fatalf("out: %q", out)
	}
	if got := readFile(t, filepath.Join(dir, "etc", "firewall.nft")); got != snapshotPrologue+table {
		t.Fatalf("persisted ruleset must replace the table on load:\n%s", got)
	}
	unit := readFile(t, filepath.Join(dir, "krill-firewall.service"))
	nft := filepath.Join(dir, "bin", "nft")
	if !strings.Contains(unit, "ExecStart="+nft+" -f "+filepath.Join(dir, "etc", "firewall.nft")) {
		t.Fatalf("unit must load the persisted file with the node's nft:\n%s", unit)
	}
	if strings.Contains(unit, "ExecStop") || !strings.Contains(unit, "After=nftables.service") {
		t.Fatalf("unit:\n%s", unit)
	}
	calls := readFile(t, filepath.Join(dir, "calls"))
	if !strings.Contains(calls, "daemon-reload") || !strings.Contains(calls, "enable krill-firewall.service") {
		t.Fatalf("systemctl calls: %s", calls)
	}
	// A second confirmation with the same unit must not reload systemd.
	os.Remove(filepath.Join(dir, "calls"))
	run()
	if calls := readFile(t, filepath.Join(dir, "calls")); strings.Contains(calls, "daemon-reload") {
		t.Fatalf("unchanged unit reloaded systemd: %s", calls)
	}
}

func TestPersistScriptRemovesWhenOpen(t *testing.T) {
	dir, run := persistNode(t)
	os.WriteFile(filepath.Join(dir, "table"), []byte("table inet krill {\n}\n"), 0o644)
	run()
	os.Remove(filepath.Join(dir, "table"))
	if out := run(); !strings.Contains(out, "removed") {
		t.Fatalf("out: %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "etc", "firewall.nft")); !os.IsNotExist(err) {
		t.Fatalf("a node with no table must not reload one at boot: %v", err)
	}
	if calls := readFile(t, filepath.Join(dir, "calls")); !strings.Contains(calls, "disable krill-firewall.service") {
		t.Fatalf("systemctl calls: %s", calls)
	}
}

// The probe script runs against stubbed ip/wg/nft/systemctl and must report
// what parseProbe expects.
func TestProbeScriptOnStubbedNode(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o755)
	file := filepath.Join(dir, "firewall.nft")
	os.WriteFile(file, []byte(snapshotPrologue+"table inet krill {\n\tchain krill_rs_fedcba9876543210 {\n\t}\n}\n"), 0o600)
	stubs := map[string]string{
		"nft": `[ "$1" = list ] && printf 'table inet krill {\n\tchain krill_rs_0123456789abcdef {\n\t}\n}\n'`,
		"systemctl": `case "$1" in
is-active) exit 3 ;;
is-enabled) exit 0 ;;
esac`,
		"ip": `case "$*" in
"-o addr show") printf '1: lo    inet 127.0.0.1/8 scope host lo\n4: wg0    inet 10.8.0.2/24 scope global wg0\n' ;;
"-o link show") printf '1: lo: <LOOPBACK,UP> mtu 65536 qdisc noqueue\n2: eth0: <UP> mtu 1500 qdisc fq\n4: wg0: <POINTOPOINT,UP> mtu 1420 qdisc noqueue\n9: veth1a2b@if8: <UP> mtu 1450 qdisc noqueue\n' ;;
"-o link show type wireguard") printf '4: wg0: <POINTOPOINT,UP> mtu 1420 qdisc noqueue\n' ;;
esac`,
		"wg": `printf 'wg0\t51820\n'`,
	}
	for name, body := range stubs {
		os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := LocalRunner{Timeout: 10 * time.Second}.Run(context.Background(), "", probeScript("10.8.0.2", file))
	if err != nil {
		t.Fatalf("probe: %v: %s", err, out)
	}
	h := parseProbe(out)
	if !h.Table || h.LiveID != "krill_rs_0123456789abcdef" || h.PersistedID != "krill_rs_fedcba9876543210" || !h.UnitEnabled || h.Pending {
		t.Fatalf("state: %+v\n%s", h, out)
	}
	if h.SwarmIface == nil || h.SwarmIface.Name != "wg0" || h.SwarmIface.MTU != 1420 || !h.SwarmIface.WireGuard {
		t.Fatalf("iface: %+v\n%s", h.SwarmIface, out)
	}
	if len(h.WireGuardPorts) != 1 || h.WireGuardPorts[0] != 51820 {
		t.Fatalf("ports: %+v\n%s", h, out)
	}
}

// confirmRunner answers commands by prefix and records them.
type confirmRunner struct {
	disarm   string
	persistE error
	cmds     []string
}

func (s *confirmRunner) Run(_ context.Context, _, cmd string) (string, error) {
	s.cmds = append(s.cmds, cmd)
	switch {
	case strings.HasPrefix(cmd, "# krill-fw-disarm"):
		return s.disarm, nil
	case strings.HasPrefix(cmd, "set -e\nf="):
		return "", s.persistE
	}
	return "", nil
}

// A confirmation that arrives after the revert fired must not persist: the
// live table is the previous one, or is being restored to it.
func TestConfirmRefusesAfterTheRevertFired(t *testing.T) {
	r := &confirmRunner{disarm: "expired\n"}
	if err := Confirm(context.Background(), r); !errors.Is(err, ErrRevertFired) {
		t.Fatalf("want ErrRevertFired, got %v", err)
	}
	for _, c := range r.cmds {
		if strings.HasPrefix(c, "set -e\nf=") {
			t.Fatal("a fired revert must not be followed by persisting")
		}
	}
}

// Once disarmed the change is in force: failing to persist it must not make
// the caller treat it as unconfirmed.
func TestConfirmSucceedsWhenOnlyPersistingFails(t *testing.T) {
	r := &confirmRunner{disarm: "disarmed\n", persistE: errors.New("read-only file system")}
	if err := Confirm(context.Background(), r); err != nil {
		t.Fatalf("confirm must succeed once disarmed: %v", err)
	}
	if len(r.cmds) != 2 {
		t.Fatalf("want disarm then persist, got %d commands", len(r.cmds))
	}
}

// The confirmation protocol against a stubbed systemd: a confirmation of a
// waiting switch leaves the marker that makes the revert a no-op; a running
// or finished revert makes it "expired"; arming again clears the marker.
func TestConfirmAndRevertShareOneProtocol(t *testing.T) {
	run := func(dir, script string, args ...string) string {
		t.Helper()
		cmd := append([]string{"-c", script}, args...)
		out, err := exec.Command("sh", cmd...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }

	dir, arm := stubNode(t)
	prev := filepath.Join(dir, "prev.nft")
	if out, err := arm(); err != nil || !strings.Contains(out, "armed ") {
		t.Fatalf("arm: %q %v", out, err)
	}
	if got := run(dir, disarmIfArmedScript(prev)); got != "disarmed" || !exists(prev+".confirmed") {
		t.Fatalf("waiting switch: %q, marker %v", got, exists(prev+".confirmed"))
	}
	// The timer fires anyway (it was already due): the revert must do nothing.
	run(dir, revertShell, prev)
	if exists(filepath.Join(dir, "restored")) {
		t.Fatal("a confirmed change was reverted")
	}

	// Arming again clears the marker, so the new switch really reverts.
	if out, err := arm(); err != nil || !strings.Contains(out, "armed ") || exists(prev+".confirmed") {
		t.Fatalf("re-arm: %q %v marker %v", out, err, exists(prev+".confirmed"))
	}
	run(dir, revertShell, prev)
	if !exists(filepath.Join(dir, "restored")) {
		t.Fatal("an unconfirmed change must be reverted")
	}

	// A revert already running: the confirmation is too late.
	dir2, arm2 := stubNode(t)
	prev2 := filepath.Join(dir2, "prev.nft")
	arm2()
	os.WriteFile(filepath.Join(dir2, "running"), nil, 0o644)
	if got := run(dir2, disarmIfArmedScript(prev2)); got != "expired" || exists(prev2+".confirmed") {
		t.Fatalf("running revert: %q", got)
	}
	// Nothing armed at all.
	dir3, _ := stubNode(t)
	if got := run(dir3, disarmIfArmedScript(filepath.Join(dir3, "prev.nft"))); got != "expired" {
		t.Fatalf("nothing armed: %q", got)
	}
}

// A userspace WireGuard (wireguard-go) is no "type wireguard" link; wg still
// lists it, and its port must be admitted.
func TestParseProbeUserspaceWireGuard(t *testing.T) {
	h := parseProbe("nft yes\niface utun3 10.8.0.2/24\nmtu utun3 1420\nwgtool yes\nwg utun3\nwgport utun3 51820\n")
	if h.SwarmIface == nil || !h.SwarmIface.WireGuard || len(h.WireGuardPorts) != 1 {
		t.Fatalf("%+v %+v", h, h.SwarmIface)
	}
}
