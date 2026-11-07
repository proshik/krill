//go:build realsystemd

package firewall

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// dockerRunner runs commands in a container booted with systemd as PID 1.
type dockerRunner struct{ ctr string }

func (d dockerRunner) Run(ctx context.Context, stdin, cmd string) (string, error) {
	c := exec.CommandContext(ctx, "docker", "exec", "-i", d.ctr, "sh", "-c", cmd)
	if stdin != "" {
		c.Stdin = strings.NewReader(stdin)
	}
	out, err := c.CombinedOutput()
	return string(out), err
}

func rulesetWith(marker string) string {
	return snapshotPrologue + "table inet krill {\n\tchain " + marker + " {\n\t}\n\tchain input {\n\t\ttype filter hook input priority 0; policy accept;\n\t}\n}\n"
}

// TestConfirmProtocolOnRealSystemd runs Apply/Confirm/Open against real
// systemd, nft and flock: what systemctl reports for a transient timer and
// its service, and how systemd-run passes the revert script its argument,
// cannot be stubbed faithfully. It needs a disposable privileged container
// booted with systemd:
//
//	FROM debian:trixie-slim   # or bookworm-slim (systemd 252)
//	RUN apt-get update && apt-get install -y --no-install-recommends systemd systemd-sysv nftables util-linux
//	STOPSIGNAL SIGRTMIN+3
//	CMD ["/lib/systemd/systemd"]
//
//	docker run -d --name krill-fwtest --privileged --cgroupns=host \
//	  -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock <image>
//	KRILL_FWTEST_CONTAINER=krill-fwtest [KRILL_FWTEST_ELAPSE=1] \
//	  go test -tags=realsystemd ./internal/firewall -run TestConfirmProtocolOnRealSystemd -v
//
// KRILL_FWTEST_ELAPSE also waits for the real timer (about two minutes) and
// checks it fires on time.
func TestConfirmProtocolOnRealSystemd(t *testing.T) {
	ctr := os.Getenv("KRILL_FWTEST_CONTAINER")
	if ctr == "" {
		t.Skip("KRILL_FWTEST_CONTAINER not set")
	}
	r := dockerRunner{ctr}
	ctx := context.Background()
	sh := func(cmd string) string {
		t.Helper()
		out, _ := r.Run(ctx, "", cmd)
		return strings.TrimSpace(out)
	}
	live := func() string {
		return sh(`nft list table inet krill 2>/dev/null | sed -n 's/^[[:space:]]*chain \(krill_rs_[0-9a-f]*\).*/\1/p'`)
	}
	units := func() string {
		return sh(`for u in krill-fw-revert.timer krill-fw-revert.service; do printf '%s: ' $u; systemctl show -p LoadState,ActiveState,SubState --value $u | tr '\n' ' '; echo; done`)
	}
	persisted := func() string {
		return sh(`sed -n 's/^[[:space:]]*chain \(krill_rs_[0-9a-f]*\).*/\1/p' /etc/krill/firewall.nft 2>/dev/null`)
	}
	reset := func() {
		t.Helper()
		if err := Open(ctx, r); err != nil {
			t.Fatalf("open: %v", err)
		}
		sh(`rm -f /run/krill-fw-prev.nft* /usr/local/sbin/nft; systemctl reset-failed krill-fw-revert.timer krill-fw-revert.service 2>/dev/null; true`)
		if out, err := r.Run(ctx, rulesetWith("krill_rs_aaaa"), "nft -f -"); err != nil {
			t.Fatalf("seed: %v %s", err, out)
		}
	}

	t.Run("confirm a waiting switch", func(t *testing.T) {
		reset()
		if err := Apply(ctx, r, rulesetWith("krill_rs_bbbb")); err != nil {
			t.Fatalf("apply: %v", err)
		}
		t.Logf("after apply:\n%s", units())
		if err := Confirm(ctx, r); err != nil {
			t.Fatalf("confirm: %v", err)
		}
		t.Logf("after confirm:\n%s", units())
		if got := live(); got != "krill_rs_bbbb" {
			t.Fatalf("live %q", got)
		}
		if got := persisted(); got != "krill_rs_bbbb" {
			t.Fatalf("persisted %q", got)
		}
		if sh(`systemctl is-active krill-fw-revert.timer`) == "active" {
			t.Fatal("timer still active after confirm")
		}
		if sh(`test -e /run/krill-fw-prev.nft.confirmed && echo yes`) != "yes" {
			t.Fatal("no confirmed marker")
		}
		// The revert running anyway (a firing that raced the stop) must change nothing.
		sh(`sh -c '` + revertShell + `' /run/krill-fw-prev.nft`)
		if got := live(); got != "krill_rs_bbbb" {
			t.Fatalf("a revert after confirm restored %q", got)
		}
	})

	t.Run("confirm while the revert runs", func(t *testing.T) {
		reset()
		if err := Apply(ctx, r, rulesetWith("krill_rs_cccc")); err != nil {
			t.Fatalf("apply: %v", err)
		}
		// nft is slow only inside systemd services, so the revert holds the
		// lock for a while; Apply/Confirm through docker exec are not slowed.
		sh(`printf '#!/bin/sh\n[ -n "$INVOCATION_ID" ] && sleep 6\nexec /usr/sbin/nft "$@"\n' > /usr/local/sbin/nft; chmod +x /usr/local/sbin/nft`)
		sh(`systemctl start --no-block krill-fw-revert.service`)
		for i := 0; i < 50 && sh(`systemctl is-active krill-fw-revert.service`) != "active"; i++ {
			time.Sleep(100 * time.Millisecond)
		}
		t.Logf("revert running:\n%s", units())
		start := time.Now()
		err := Confirm(ctx, r)
		t.Logf("confirm returned after %s: %v\n%s", time.Since(start).Round(100*time.Millisecond), err, units())
		if !errors.Is(err, ErrRevertFired) {
			t.Fatalf("confirm during a running revert: %v", err)
		}
		for i := 0; i < 100 && sh(`systemctl is-active krill-fw-revert.service`) == "active"; i++ {
			time.Sleep(100 * time.Millisecond)
		}
		if got := live(); got != "krill_rs_aaaa" {
			t.Fatalf("revert did not restore the previous table: %q", got)
		}
		if got := persisted(); got == "krill_rs_cccc" {
			t.Fatal("the reverted change was persisted")
		}
	})

	t.Run("timer really elapses", func(t *testing.T) {
		if os.Getenv("KRILL_FWTEST_ELAPSE") == "" {
			t.Skip("KRILL_FWTEST_ELAPSE not set (waits the full revert delay)")
		}
		reset()
		if err := Apply(ctx, r, rulesetWith("krill_rs_dddd")); err != nil {
			t.Fatalf("apply: %v", err)
		}
		t.Logf("armed:\n%s", units())
		if got := sh(`systemctl show -p AccuracyUSec --value krill-fw-revert.timer`); got != "1s" {
			t.Fatalf("timer accuracy %q, want 1s", got)
		}
		time.Sleep(RevertDelay + 5*time.Second)
		t.Logf("after the delay:\n%s", units())
		// It fired on time: the revert is done and the transient units are gone.
		if sh(`systemctl is-active --quiet krill-fw-revert.timer krill-fw-revert.service && echo armed`) == "armed" {
			t.Fatalf("the switch has not fired %s after arming", RevertDelay+5*time.Second)
		}
		if got := live(); got != "krill_rs_aaaa" {
			t.Fatalf("the real timer did not restore the previous table: %q", got)
		}
		if err := Confirm(ctx, r); !errors.Is(err, ErrRevertFired) {
			t.Fatalf("confirm after the timer fired: %v", err)
		}
		if got := persisted(); got == "krill_rs_dddd" {
			t.Fatal("the reverted change was persisted")
		}
		// A fired switch must not block the next Apply.
		if err := Apply(ctx, r, rulesetWith("krill_rs_eeee")); err != nil {
			t.Fatalf("apply after a fired switch: %v", err)
		}
		if err := Confirm(ctx, r); err != nil {
			t.Fatalf("confirm the next apply: %v", err)
		}
		if got := live(); got != "krill_rs_eeee" {
			t.Fatalf("live %q", got)
		}
	})
}
