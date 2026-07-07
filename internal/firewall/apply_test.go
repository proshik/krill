package firewall

import (
	"context"
	"strings"
	"testing"
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
