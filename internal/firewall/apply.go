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
// table and schedules its restore. One script, so the check and the snapshot
// cannot be split by another Apply. It prints "armed <epoch>" or
// "pending <epoch>" — the epoch being when the switch fires, or "-" if unknown.
func armScript(savePath string) string {
	return fmt.Sprintf(`set -e
unit=%[1]s
prev=%[2]s
at="$prev.at"
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
		if cerr := Confirm(ctx, r); cerr != nil {
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
// is still reachable and swarm-healthy.
func Confirm(ctx context.Context, r Runner) error {
	_, err := r.Run(ctx, "", fmt.Sprintf("systemctl stop %s.timer 2>/dev/null; systemctl reset-failed %s 2>/dev/null; true", revertUnit, revertUnit))
	return err
}

// Open removes the lockdown table (and cancels any pending revert).
func Open(ctx context.Context, r Runner) error {
	_, err := r.Run(ctx, "", fmt.Sprintf("systemctl stop %s.timer 2>/dev/null; nft delete table inet krill 2>/dev/null; true", revertUnit))
	return err
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
	cl, err := cluster.DialVerified(rr.Spec, rr.Timeout)
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
