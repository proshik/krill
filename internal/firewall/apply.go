package firewall

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/proshik/krill/internal/cluster"
)

const (
	revertSavePath = "/run/krill-fw-prev.nft"
	revertUnit     = "krill-fw-revert"
	revertDelaySec = 120
)

// Runner runs a remote command on one node, feeding stdin, returning stdout+stderr.
type Runner interface {
	Run(ctx context.Context, stdin, cmd string) (string, error)
}

// Apply installs the ruleset with a dead-man switch: it first snapshots the
// current table and schedules an auto-revert in revertDelaySec, THEN applies the
// new ruleset. If the caller cannot reach the node afterwards, the revert fires
// and restores access. On success the caller must invoke Confirm to cancel it.
func Apply(ctx context.Context, r Runner, ruleset string) error {
	// Snapshot current table (or a delete stub if it doesn't exist yet).
	save := fmt.Sprintf("nft list table inet krill > %s 2>/dev/null || echo 'delete table inet krill' > %s", revertSavePath, revertSavePath)
	if _, err := r.Run(ctx, "", save); err != nil {
		return fmt.Errorf("snapshot ruleset: %w", err)
	}
	// Schedule the auto-revert.
	sched := fmt.Sprintf("systemd-run --on-active=%d --unit=%s nft -f %s", revertDelaySec, revertUnit, revertSavePath)
	if _, err := r.Run(ctx, "", sched); err != nil {
		return fmt.Errorf("schedule revert: %w", err)
	}
	// Apply the new ruleset via stdin.
	if _, err := r.Run(ctx, ruleset, "nft -f -"); err != nil {
		return fmt.Errorf("apply ruleset: %w", err)
	}
	return nil
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
	out, err := r.Run(ctx, "", "nft list table inet krill >/dev/null 2>&1 && echo yes || echo no")
	if err != nil {
		return false, err
	}
	return bytes.Contains([]byte(out), []byte("yes")), nil
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
