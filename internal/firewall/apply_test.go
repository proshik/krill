package firewall

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct{ cmds []string }

func (f *fakeRunner) Run(ctx context.Context, stdin, cmd string) (string, error) {
	f.cmds = append(f.cmds, cmd)
	return "", nil
}

func TestApplySchedulesRevertBeforeApplying(t *testing.T) {
	f := &fakeRunner{}
	if err := Apply(context.Background(), f, "table inet krill {}"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	joined := strings.Join(f.cmds, "\n")
	saveIdx := strings.Index(joined, revertSavePath)   // prev saved
	revertIdx := strings.Index(joined, "systemd-run")  // revert scheduled
	applyIdx := strings.LastIndex(joined, "nft -f -")  // new ruleset applied via stdin
	if !(saveIdx >= 0 && revertIdx > saveIdx && applyIdx > revertIdx) {
		t.Fatalf("order wrong: save=%d revert=%d apply=%d\n%s", saveIdx, revertIdx, applyIdx, joined)
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
