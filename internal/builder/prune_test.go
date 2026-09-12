package builder

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakePruneCLI struct {
	calls [][]string
	// knowsReserved is false for a CLI that predates --reserved-space.
	knowsReserved bool
	fail          bool
}

func (f *fakePruneCLI) run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	if !f.knowsReserved && contains(args, pruneFlagReserved) {
		return []byte("unknown flag: " + pruneFlagReserved + "\nSee 'docker builder prune --help'."), errors.New("exit status 125")
	}
	if f.fail {
		return []byte("Error response from daemon: boom"), errors.New("exit status 1")
	}
	return []byte("Total:\t0B"), nil
}

func contains(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}

// Docker CLI 28+ deprecated --keep-storage in favour of --reserved-space, so the
// new spelling is used whenever the CLI knows it.
func TestCachePruneUsesReservedSpace(t *testing.T) {
	cli := &fakePruneCLI{knowsReserved: true}
	p := &cachePruner{run: cli.run}
	if err := p.prune(context.Background(), "5gb"); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(cli.calls) != 1 || strings.Join(cli.calls[0], " ") != "builder prune --force --reserved-space 5gb" {
		t.Fatalf("calls = %v", cli.calls)
	}
}

// An older CLI rejects the new flag: prune falls back to --keep-storage, and
// remembers it, so later ticks do not fail first every time.
func TestCachePruneFallsBackToKeepStorageOnAnOlderCLI(t *testing.T) {
	cli := &fakePruneCLI{knowsReserved: false}
	p := &cachePruner{run: cli.run}
	for i := 0; i < 2; i++ {
		if err := p.prune(context.Background(), "5gb"); err != nil {
			t.Fatalf("prune %d: %v", i, err)
		}
	}
	want := []string{
		"builder prune --force --reserved-space 5gb",
		"builder prune --force --keep-storage 5gb",
		"builder prune --force --keep-storage 5gb",
	}
	if len(cli.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", cli.calls, want)
	}
	for i := range want {
		if got := strings.Join(cli.calls[i], " "); got != want[i] {
			t.Fatalf("call %d = %q, want %q", i, got, want[i])
		}
	}
}

// Any other failure is reported as is: it says nothing about the flag, so it
// must not flip the pruner onto the deprecated spelling.
func TestCachePruneDoesNotFallBackOnAnUnrelatedError(t *testing.T) {
	cli := &fakePruneCLI{knowsReserved: true, fail: true}
	p := &cachePruner{run: cli.run}
	if err := p.prune(context.Background(), "5gb"); err == nil {
		t.Fatal("a failed prune must be reported")
	}
	if len(cli.calls) != 1 || p.legacy {
		t.Fatalf("an unrelated error must not switch spellings, calls=%v legacy=%v", cli.calls, p.legacy)
	}
}
