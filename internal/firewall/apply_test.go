package firewall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRunner records commands and answers the arm script as an idle node would.
type fakeRunner struct {
	cmds   []string
	armOut string // arm script output; "" means "armed <now+delay>"
	nftErr error  // returned by `nft -f -`
}

func (f *fakeRunner) Run(ctx context.Context, stdin, cmd string) (string, error) {
	f.cmds = append(f.cmds, cmd)
	switch {
	case strings.Contains(cmd, "systemd-run"):
		if f.armOut != "" {
			return f.armOut, nil
		}
		return fmt.Sprintf("armed %d\n", time.Now().Unix()+revertDelaySec), nil
	case cmd == "nft -f -":
		return "", f.nftErr
	}
	return "", nil
}

func TestApplySchedulesRevertBeforeApplying(t *testing.T) {
	f := &fakeRunner{}
	if err := Apply(context.Background(), f, "table inet krill {}"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	joined := strings.Join(f.cmds, "\n")
	saveIdx := strings.Index(joined, "> \"$prev.tmp\"")           // prev saved
	revertIdx := strings.Index(joined, "systemd-run --on-active") // revert scheduled
	applyIdx := strings.LastIndex(joined, "nft -f -")             // new ruleset applied via stdin
	if !(saveIdx >= 0 && revertIdx > saveIdx && applyIdx > revertIdx) {
		t.Fatalf("order wrong: save=%d revert=%d apply=%d\n%s", saveIdx, revertIdx, applyIdx, joined)
	}
}

// A second Apply while the first is unconfirmed must not reach the ruleset:
// the armed switch still has to restore the state from before the first one.
func TestApplyRefusesWhileRevertArmed(t *testing.T) {
	at := time.Now().Add(90 * time.Second).Truncate(time.Second)
	f := &fakeRunner{armOut: fmt.Sprintf("pending %d\n", at.Unix())}
	err := Apply(context.Background(), f, "table inet krill {}")
	var armed *RevertArmedError
	if !errors.As(err, &armed) {
		t.Fatalf("want RevertArmedError, got %v", err)
	}
	if !armed.At.Equal(at) {
		t.Fatalf("revert time = %v, want %v", armed.At, at)
	}
	for _, c := range f.cmds {
		if c == "nft -f -" {
			t.Fatal("a refused Apply must not apply the ruleset")
		}
	}
}

// nft -f is one transaction, so a rejected ruleset changed nothing and the
// switch it armed is disarmed rather than left to block the next attempt.
func TestApplyDisarmsWhenRulesetRejected(t *testing.T) {
	f := &fakeRunner{nftErr: errors.New("syntax error")}
	if err := Apply(context.Background(), f, "garbage"); err == nil {
		t.Fatal("want an error")
	}
	if last := f.cmds[len(f.cmds)-1]; !strings.Contains(last, "systemctl stop "+revertUnit+".timer") {
		t.Fatalf("want the revert disarmed last, got %q", last)
	}
}

// stubNode puts fake systemctl/systemd-run/nft on PATH for LocalRunner, backed
// by files in dir: "armed" exists while the timer waits, "table" is what
// `nft list table` prints (absent = no table), "fail-run" makes systemd-run fail.
func stubNode(t *testing.T) (dir string, run func() (string, error)) {
	t.Helper()
	dir = t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stubs := map[string]string{
		"systemctl": `case "$1" in
is-active) [ -e "$STATE/armed" ] ;;
*) exit 0 ;;
esac`,
		"systemd-run": `[ -e "$STATE/fail-run" ] && { echo "Unit krill-fw-revert.timer was already loaded" >&2; exit 1; }
echo "$@" > "$STATE/sched"; touch "$STATE/armed"; echo "Running timer as unit: krill-fw-revert.timer" >&2`,
		"nft": `[ "$1" = list ] && [ -e "$STATE/table" ] && exec cat "$STATE/table"; exit 1`,
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("STATE", dir)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	save := filepath.Join(dir, "prev.nft")
	return dir, func() (string, error) {
		return LocalRunner{Timeout: 10 * time.Second}.Run(context.Background(), "", armScript(save))
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The snapshot restores by replacing the table: restored with plain `nft -f`,
// a bare `nft list table` would add its rules to the table's current ones.
func TestArmScriptSnapshotReplacesTable(t *testing.T) {
	dir, run := stubNode(t)
	table := "table inet krill {\n\tchain input {\n\t\ttcp dport 22 accept\n\t}\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "table"), []byte(table), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run()
	if err != nil {
		t.Fatalf("arm: %v: %s", err, out)
	}
	state, at := parseArmOutput(out)
	if state != "armed" || time.Until(at) < 100*time.Second || time.Until(at) > 130*time.Second {
		t.Fatalf("want armed ~2 min out, got %q", out)
	}
	if got := readFile(t, filepath.Join(dir, "prev.nft")); got != snapshotPrologue+table {
		t.Fatalf("snapshot:\n%s", got)
	}
	if sched := readFile(t, filepath.Join(dir, "sched")); !strings.Contains(sched, "--unit=krill-fw-revert") || !strings.Contains(sched, filepath.Join(dir, "prev.nft")) {
		t.Fatalf("revert not scheduled against the snapshot: %s", sched)
	}
}

// With no table the snapshot restores "no table", not a failing bare delete.
func TestArmScriptSnapshotOfNoTable(t *testing.T) {
	dir, run := stubNode(t)
	if out, err := run(); err != nil {
		t.Fatalf("arm: %v: %s", err, out)
	}
	if got := readFile(t, filepath.Join(dir, "prev.nft")); got != snapshotPrologue {
		t.Fatalf("snapshot: %q", got)
	}
}

// The production incident: a second close while the first was unconfirmed
// replaced the snapshot with the unconfirmed ruleset before systemd-run
// refused the duplicate unit, so the switch restored the change it guarded.
func TestArmScriptKeepsSnapshotWhileArmed(t *testing.T) {
	dir, run := stubNode(t)
	os.WriteFile(filepath.Join(dir, "table"), []byte("table inet krill { # open\n}\n"), 0o644)
	first, err := run()
	if err != nil {
		t.Fatalf("arm: %v: %s", err, first)
	}
	_, firstAt := parseArmOutput(first)
	snapshot := readFile(t, filepath.Join(dir, "prev.nft"))

	os.WriteFile(filepath.Join(dir, "table"), []byte("table inet krill { # closed\n}\n"), 0o644)
	second, err := run()
	if err != nil {
		t.Fatalf("second arm: %v: %s", err, second)
	}
	state, at := parseArmOutput(second)
	if state != "pending" || !at.Equal(firstAt) {
		t.Fatalf("want pending at %v, got %q", firstAt, second)
	}
	if got := readFile(t, filepath.Join(dir, "prev.nft")); got != snapshot {
		t.Fatalf("the armed snapshot was overwritten:\n%s", got)
	}
}

// A systemd-run failure is an error, not a silently unguarded apply.
func TestArmScriptFailsWhenScheduleFails(t *testing.T) {
	dir, run := stubNode(t)
	os.WriteFile(filepath.Join(dir, "fail-run"), nil, 0o644)
	out, err := run()
	if err == nil {
		t.Fatalf("want an error, got %q", out)
	}
	if !strings.Contains(out, "already loaded") {
		t.Fatalf("systemd-run's own message must reach the caller: %q", out)
	}
}

func TestConfirmCancelsRevert(t *testing.T) {
	f := &fakeRunner{}
	if err := Confirm(context.Background(), f); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if !strings.Contains(strings.Join(f.cmds, "\n"), "krill-fw-revert") {
		t.Fatalf("confirm must stop the revert unit: %v", f.cmds)
	}
}

// TestRunCtxReturnsPromptlyOnCancel exercises the select-based deadline logic
// that RealRunner.Run wires around sess.CombinedOutput. RealRunner itself
// needs a real SSH session, so this drives the extracted runCtx helper
// directly: work blocks until closeSess is called (mimicking an SSH session
// whose blocked remote command only unblocks when the session is closed),
// and a canceled ctx must call closeSess and return ctx.Err() without
// waiting for work to finish.
func TestRunCtxReturnsPromptlyOnCancel(t *testing.T) {
	closed := make(chan struct{})
	unblock := make(chan struct{})
	closeSess := func() error {
		close(closed)
		return nil
	}
	work := func() (string, error) {
		<-unblock // never sent — simulates a hung remote command
		return "should not get here", nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled: Run must not wait on work

	done := make(chan struct{})
	var out string
	var err error
	go func() {
		out, err = runCtx(ctx, closeSess, work)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runCtx did not return promptly on ctx cancel")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if out != "" {
		t.Fatalf("out = %q, want empty on cancel", out)
	}
	select {
	case <-closed:
	default:
		t.Fatal("closeSess was not called on ctx cancel")
	}
	close(unblock) // let the leaked goroutine exit
}

// TestRunCtxReturnsWorkResultOnSuccess is the happy-path sibling: when work
// finishes before ctx is done, its result passes through unchanged and
// closeSess is never invoked.
func TestRunCtxReturnsWorkResultOnSuccess(t *testing.T) {
	closeCalled := false
	closeSess := func() error {
		closeCalled = true
		return nil
	}
	work := func() (string, error) { return "output", nil }

	out, err := runCtx(context.Background(), closeSess, work)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if out != "output" {
		t.Fatalf("out = %q, want %q", out, "output")
	}
	if closeCalled {
		t.Fatal("closeSess must not be called on success")
	}
}

type scriptedRunner struct{ out string }

func (s scriptedRunner) Run(ctx context.Context, stdin, cmd string) (string, error) {
	return s.out, nil
}

// A host without nft used to report "no" — shown as an open firewall — when
// the lockdown could not be applied there at all.
func TestStatusReportsMissingNft(t *testing.T) {
	if _, err := Status(context.Background(), scriptedRunner{out: "nft-missing\n"}); !errors.Is(err, ErrNftMissing) {
		t.Fatalf("want ErrNftMissing, got %v", err)
	}
	locked, err := Status(context.Background(), scriptedRunner{out: "yes\n"})
	if err != nil || !locked {
		t.Fatalf("want locked, got %v %v", locked, err)
	}
	locked, err = Status(context.Background(), scriptedRunner{out: "no\n"})
	if err != nil || locked {
		t.Fatalf("want open, got %v %v", locked, err)
	}
}

func TestLocalRunnerFeedsStdinAndReturnsOutput(t *testing.T) {
	out, err := LocalRunner{Timeout: 5 * time.Second}.Run(context.Background(), "table inet krill {}\n", "cat")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != "table inet krill {}\n" {
		t.Fatalf("stdin not passed through: %q", out)
	}
}

func TestLocalRunnerReportsNonZeroExit(t *testing.T) {
	out, err := LocalRunner{Timeout: 5 * time.Second}.Run(context.Background(), "", "echo boom >&2; exit 3")
	if err == nil {
		t.Fatal("non-zero exit must be an error")
	}
	if !strings.Contains(out, "boom") {
		t.Fatalf("stderr must be captured: %q", out)
	}
}

func TestLocalRunnerHonorsTimeout(t *testing.T) {
	start := time.Now()
	_, err := LocalRunner{Timeout: 200 * time.Millisecond}.Run(context.Background(), "", "sleep 10")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("timeout not enforced: took %v", time.Since(start))
	}
}

// GatewayOnly reads the rule as `nft list` prints it back.
func TestGatewayOnly(t *testing.T) {
	listed := "table inet krill {\n\tchain input {\n\t\ttcp dport { 80, 443 } accept\n\t\tiifname \"docker_gwbridge\" tcp dport 8080 accept\n\t}\n}\n"
	if got, err := GatewayOnly(context.Background(), scriptedRunner{out: listed}); err != nil || !got {
		t.Fatalf("want gateway-only, got %v %v", got, err)
	}
	open := "table inet krill {\n\tchain input {\n\t\ttcp dport { 80, 443, 8080 } accept\n\t}\n}\n"
	if got, err := GatewayOnly(context.Background(), scriptedRunner{out: open}); err != nil || got {
		t.Fatalf("want public, got %v %v", got, err)
	}
	if got, _ := GatewayOnly(context.Background(), scriptedRunner{out: ""}); got {
		t.Fatal("no table is not gateway-only")
	}
}

// Two Applies racing on one node: the arm script must serialize them, so the
// loser sees the winner's switch armed instead of snapshotting after it.
func TestArmScriptSerializesConcurrentApplies(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock not installed")
	}
	dir, run := stubNode(t)
	// Widen the window between the check and systemd-run.
	os.WriteFile(filepath.Join(dir, "bin", "nft"), []byte("#!/bin/sh\nsleep 0.3; [ \"$1\" = list ] && [ -e \"$STATE/table\" ] && exec cat \"$STATE/table\"; exit 1\n"), 0o755)
	outs := make(chan string, 2)
	for i := 0; i < 2; i++ {
		go func() {
			out, err := run()
			if err != nil {
				out = "error " + err.Error() + ": " + out
			}
			outs <- out
		}()
	}
	var armed, pending int
	for i := 0; i < 2; i++ {
		switch state, _ := parseArmOutput(<-outs); state {
		case "armed":
			armed++
		case "pending":
			pending++
		}
	}
	if armed != 1 || pending != 1 {
		t.Fatalf("want one armed and one pending, got armed=%d pending=%d", armed, pending)
	}
}
