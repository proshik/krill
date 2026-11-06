package firewall

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/proshik/krill/internal/cluster"
)

const (
	revertSavePath = "/run/krill-fw-prev.nft"
	revertUnit     = "krill-fw-revert"
	revertDelaySec = 120
)

// RevertDelay is how long after Apply the dead-man switch fires unless
// Confirm cancels it.
const RevertDelay = revertDelaySec * time.Second

// snapshotPrologue opens every snapshot, so that restoring one replaces the
// table instead of adding its rules to whatever the table holds by then — the
// same full-replace preamble buildRuleset writes. A node with no table yet
// snapshots as the prologue alone, which restores "no table".
const snapshotPrologue = "table inet krill { }\ndelete table inet krill\n"

// ErrNftMissing is returned by Status when the node has no `nft` binary: the
// lockdown cannot be applied there, and reporting the table as merely absent
// ("open") would hide that.
var ErrNftMissing = errors.New("firewall: nft is not installed on the node")

// RevertArmedError is returned by Apply when an earlier change on the node is
// still awaiting confirmation, i.e. its dead-man switch is armed. Apply then
// changes nothing: taking a new snapshot would overwrite the one the armed
// switch restores with the unconfirmed ruleset, and the switch would put the
// unconfirmed state back instead of the last good one.
type RevertArmedError struct {
	// At is when the armed switch fires; zero if the node did not say.
	At time.Time
}

func (e *RevertArmedError) Error() string {
	if e.At.IsZero() {
		return "firewall: an earlier change is still awaiting confirmation"
	}
	return "firewall: an earlier change is still awaiting confirmation; it reverts at " + e.At.UTC().Format(time.RFC3339)
}

// Runner runs a command on one node, feeding stdin, returning stdout+stderr.
type Runner interface {
	Run(ctx context.Context, stdin, cmd string) (string, error)
}

// armScript refuses while a switch is armed (the timer is waiting, or it fired
// and its `nft -f` is still running), and otherwise snapshots the current
// table and schedules its restore. It prints "armed <epoch>" or
// "pending <epoch>" — the epoch being when the switch fires, or "-" if unknown.
//
// The check, the snapshot and the arming run under an flock, so two Applies
// cannot both pass the check: the second then snapshots after the first has
// already applied its ruleset, and would save that over the good snapshot.
// The lock is taken on a fixed fd, which busybox's flock understands as well
// as util-linux's; a node without flock at all still gets the rest.
func armScript(savePath string) string {
	return fmt.Sprintf(`set -e
unit=%[1]s
prev=%[2]s
at="$prev.at"
exec 9>"$prev.lock"
if command -v flock >/dev/null 2>&1; then flock 9; fi
if systemctl is-active --quiet "$unit.timer" "$unit.service"; then
	echo "pending $(cat "$at" 2>/dev/null || echo -)"
	exit 0
fi
# A fired switch whose nft failed leaves the unit behind in "failed", and
# systemd-run refuses to reuse the name until it is reset.
systemctl reset-failed "$unit.timer" "$unit.service" >/dev/null 2>&1 || true
{ printf '%[3]s'; nft list table inet krill 2>/dev/null || true; } > "$prev.tmp"
mv "$prev.tmp" "$prev"
echo $(( $(date +%%s) + %[4]d )) > "$at"
systemd-run --on-active=%[4]d --unit="$unit" nft -f "$prev"
echo "armed $(cat "$at")"
`, revertUnit, savePath, strings.ReplaceAll(snapshotPrologue, "\n", `\n`), revertDelaySec)
}

// Apply installs the ruleset with a dead-man switch: it first snapshots the
// current table and schedules an auto-revert in revertDelaySec, THEN applies the
// new ruleset. If the caller cannot reach the node afterwards, the revert fires
// and restores access. On success the caller must invoke Confirm to cancel it.
//
// While an earlier Apply on the node is unconfirmed, Apply returns a
// *RevertArmedError and touches nothing.
func Apply(ctx context.Context, r Runner, ruleset string) error {
	out, err := r.Run(ctx, "", armScript(revertSavePath))
	if err != nil {
		return fmt.Errorf("schedule revert: %w: %s", err, strings.TrimSpace(out))
	}
	state, at := parseArmOutput(out)
	switch state {
	case "pending":
		return &RevertArmedError{At: at}
	case "armed":
	default:
		return fmt.Errorf("schedule revert: unexpected output %q", strings.TrimSpace(out))
	}
	// Apply the new ruleset via stdin.
	if _, err := r.Run(ctx, ruleset, "nft -f -"); err != nil {
		// `nft -f` is one transaction: a rejected ruleset changed nothing, so
		// the armed switch would only block the next attempt for its full delay.
		if cerr := disarm(ctx, r); cerr != nil {
			return fmt.Errorf("apply ruleset: %w (disarming the revert also failed: %v)", err, cerr)
		}
		return fmt.Errorf("apply ruleset: %w", err)
	}
	return nil
}

// parseArmOutput reads armScript's last line: its state word and, when the
// node reported one, the time the switch fires.
func parseArmOutput(out string) (string, time.Time) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) == 0 {
		return "", time.Time{}
	}
	var at time.Time
	if len(fields) > 1 {
		if sec, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
			at = time.Unix(sec, 0)
		}
	}
	return fields[0], at
}

// Confirm cancels the pending auto-revert after the caller has verified the node
// is still reachable and swarm-healthy, and persists the now-proven table so
// the node loads it again at boot.
func Confirm(ctx context.Context, r Runner) error {
	if err := disarm(ctx, r); err != nil {
		return err
	}
	out, err := r.Run(ctx, "", persistScript(persistPath, bootUnitDir+"/"+bootUnit))
	if err != nil {
		return fmt.Errorf("persist ruleset: %w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// disarm cancels the pending auto-revert without persisting anything: for a
// ruleset nft rejected, which changed nothing.
func disarm(ctx context.Context, r Runner) error {
	_, err := r.Run(ctx, "", fmt.Sprintf("systemctl stop %s.timer 2>/dev/null; systemctl reset-failed %s 2>/dev/null; true", revertUnit, revertUnit))
	return err
}

// bootUnitText is the unit that reloads the persisted ruleset at boot. It runs
// before the network comes up, like the distribution's own nftables.service,
// and after it and firewalld: both may flush the whole ruleset when they
// start, which would remove a table loaded earlier. It has no ExecStop, so
// stopping the unit never opens the node.
func bootUnitText(nft, file string) string {
	return fmt.Sprintf(`[Unit]
Description=Krill node firewall (inet krill)
Wants=network-pre.target
Before=network-pre.target shutdown.target
After=nftables.service firewalld.service
Conflicts=shutdown.target
DefaultDependencies=no

[Service]
Type=oneshot
RemainAfterExit=yes
StandardInput=null
ExecStart=%s -f %s

[Install]
WantedBy=sysinit.target
`, nft, file)
}

// persistScript writes the loaded table to persistPath — prefixed with the
// same full-replace prologue as a snapshot, so loading it replaces whatever
// table exists — and enables the boot unit; with no table loaded it removes
// both, so a node that is open stays open after a reboot. The file is written
// next to its final place and renamed, so a failure never leaves a truncated
// ruleset for the next boot.
func persistScript(file, unit string) string {
	return fmt.Sprintf(`set -e
f=%[1]s
mkdir -p "$(dirname "$f")"
if nft list table inet krill > "$f.list" 2>/dev/null; then
	{ printf '%[2]s'; cat "$f.list"; } > "$f.new"
	rm -f "$f.list"
	chmod 600 "$f.new"
	mv "$f.new" "$f"
	nft=$(command -v nft)
	cat > %[3]s.new <<'UNIT'
%[4]sUNIT
	sed "s#__NFT__#$nft#" %[3]s.new > %[3]s.tmp && mv %[3]s.tmp %[3]s.new
	if ! cmp -s %[3]s.new %[3]s 2>/dev/null; then mv %[3]s.new %[3]s; systemctl daemon-reload; else rm -f %[3]s.new; fi
	systemctl enable %[5]s >/dev/null 2>&1
	echo persisted
else
	rm -f "$f.list" "$f"
	systemctl disable %[5]s >/dev/null 2>&1 || true
	echo removed
fi
`, file, strings.ReplaceAll(snapshotPrologue, "\n", `\n`), unit, bootUnitText("__NFT__", file), bootUnit)
}

// Open removes the lockdown table, cancels any pending revert and removes the
// persisted ruleset, so the node also stays open after a reboot.
func Open(ctx context.Context, r Runner) error {
	_, err := r.Run(ctx, "", fmt.Sprintf("systemctl stop %s.timer 2>/dev/null; nft delete table inet krill 2>/dev/null; %s", revertUnit, unpersistScript()))
	return err
}

// unpersistScript removes the persisted ruleset and disables its boot unit.
func unpersistScript() string {
	return fmt.Sprintf("rm -f %s; systemctl disable %s >/dev/null 2>&1; true", persistPath, bootUnit)
}

// Status reports whether the lockdown table is present on the node.
func Status(ctx context.Context, r Runner) (bool, error) {
	out, err := r.Run(ctx, "", "command -v nft >/dev/null 2>&1 || { echo nft-missing; exit 0; }; nft list table inet krill >/dev/null 2>&1 && echo yes || echo no")
	if err != nil {
		return false, err
	}
	if strings.Contains(out, "nft-missing") {
		return false, ErrNftMissing
	}
	return strings.Contains(out, "yes"), nil
}

// GatewayOnly reports whether the node's live table admits some port only from
// GatewayInterface — the rule BuildManagerRuleset writes for gatewayPorts, i.e.
// the control plane's UI port closed to the world. A node with no table
// reports false. It reads the host, not what Krill last stored, so callers can
// notice the two disagreeing.
func GatewayOnly(ctx context.Context, r Runner) (bool, error) {
	out, err := r.Run(ctx, "", "nft list table inet krill 2>/dev/null || true")
	if err != nil {
		return false, err
	}
	return strings.Contains(out, fmt.Sprintf("iifname %q", GatewayInterface)), nil
}

// RevertPending reports whether an Apply is still awaiting Confirm, i.e. the
// dead-man switch is armed and will restore the previous ruleset when it fires.
func RevertPending(ctx context.Context, r Runner) (bool, error) {
	out, err := r.Run(ctx, "", fmt.Sprintf("systemctl is-active --quiet %s.timer && echo yes || echo no", revertUnit))
	if err != nil {
		return false, err
	}
	return strings.Contains(out, "yes"), nil
}

// LocalRunner runs commands on this host through `sh -c`. It exists for the
// control plane, which has no cluster_nodes row and no SSH credentials to
// itself. The dead-man switch Apply schedules is a separate transient systemd
// unit, so it still fires if this process dies in the meantime.
type LocalRunner struct {
	Timeout time.Duration // per command; <= 0 means only ctx bounds it
}

func (lr LocalRunner) Run(ctx context.Context, stdin, cmd string) (string, error) {
	if lr.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, lr.Timeout)
		defer cancel()
	}
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	if stdin != "" {
		c.Stdin = strings.NewReader(stdin)
	}
	// A background grandchild holding the output pipe open must not keep Run
	// blocked past the deadline.
	c.WaitDelay = 2 * time.Second
	out, err := c.CombinedOutput()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return string(out), ctxErr
	}
	return string(out), err
}

// RealRunner runs commands over SSH using the cluster dial helper.
type RealRunner struct {
	Spec    cluster.JoinSpec // Host/Port/User/PrivateKey/HostKey of the node
	Timeout time.Duration
}

func (rr RealRunner) Run(ctx context.Context, stdin, cmd string) (string, error) {
	cl, err := cluster.DialVerifiedContext(ctx, rr.Spec, rr.Timeout)
	if err != nil {
		return "", err
	}
	defer cl.Close()
	sess, err := cl.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	if stdin != "" {
		sess.Stdin = bytes.NewBufferString(stdin)
	}
	return runCtx(ctx, sess.Close, func() (string, error) {
		out, err := sess.CombinedOutput(cmd)
		return string(out), err
	})
}

// runCtx runs work in a goroutine and returns its result, unless ctx is
// canceled first — in which case it calls closeSess (to unblock the
// still-running work, e.g. by closing the underlying SSH session so a
// blocked remote command errors out) and returns ctx.Err(). Extracted from
// RealRunner.Run so the deadline/cancel behavior is unit-testable without a
// real SSH session.
func runCtx(ctx context.Context, closeSess func() error, work func() (string, error)) (string, error) {
	type res struct {
		out string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		out, err := work()
		ch <- res{out, err}
	}()
	select {
	case <-ctx.Done():
		if closeSess != nil {
			_ = closeSess()
		}
		return "", ctx.Err()
	case r := <-ch:
		return r.out, r.err
	}
}
