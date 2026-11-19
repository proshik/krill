package metrics_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/proshik/krill/internal/metrics"
)

func TestParseDF(t *testing.T) {
	// GNU df -P -k: root and a separate Docker filesystem, plus a mount with a
	// space in its path; busybox prints the same columns.
	out := `Filesystem     1024-blocks     Used Available Capacity Mounted on
/dev/vda1         51290592 43597004   7677204      86% /
/dev/vdb1        103080224  1048576 101080224       2% /var/lib/docker
/dev/vdc1             1000      500       500      50% /mnt/my disk
`
	got := metrics.ParseDF(out)
	want := []metrics.DiskUsage{
		{Mount: "/", Filesystem: "/dev/vda1", Total: 51290592 * 1024, Used: 43597004 * 1024, Avail: 7677204 * 1024},
		{Mount: "/var/lib/docker", Filesystem: "/dev/vdb1", Total: 103080224 * 1024, Used: 1048576 * 1024, Avail: 101080224 * 1024},
		{Mount: "/mnt/my disk", Filesystem: "/dev/vdc1", Total: 1000 * 1024, Used: 500 * 1024, Avail: 500 * 1024},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseDF =\n%+v\nwant\n%+v", got, want)
	}
	// df prints 86% for this line (85.02 rounded up); so must Krill.
	if p := got[0].UsedPct(); p != 86 {
		t.Errorf("UsedPct = %.2f, want df's 86%%", p)
	}
	if p := got[1].UsedPct(); p != 2 {
		t.Errorf("UsedPct = %.2f, want df's 2%%", p)
	}
}

// /var/lib/docker on the root filesystem: df prints the same mount twice,
// and the node has one filesystem, not two.
func TestParseDFCollapsesSameMount(t *testing.T) {
	out := `Filesystem 1024-blocks Used Available Capacity Mounted on
overlay 100 50 50 50% /
overlay 100 50 50 50% /
`
	if got := metrics.ParseDF(out); len(got) != 1 || got[0].Mount != "/" {
		t.Fatalf("ParseDF = %+v, want one entry for /", got)
	}
}

func TestParseDFSkipsGarbage(t *testing.T) {
	if got := metrics.ParseDF("df: /nope: No such file or directory\n\nrandom words here x y z\n"); len(got) != 0 {
		t.Fatalf("ParseDF(garbage) = %+v, want none", got)
	}
}

func TestDiskViaDF(t *testing.T) {
	var argv []string
	run := func(_ context.Context, a []string) ([]byte, error) {
		argv = a
		// df exits 1 for the missing second path but still prints the root.
		return []byte("Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sda1 10 5 5 50% /\n"), errors.New("exit status 1")
	}
	got, err := metrics.DiskViaDF(context.Background(), run, []string{"/", "/missing"})
	if err != nil || len(got) != 1 {
		t.Fatalf("DiskViaDF = %+v, %v; want the root despite df's exit status", got, err)
	}
	if !reflect.DeepEqual(argv, []string{"df", "-P", "-k", "/", "/missing"}) {
		t.Fatalf("argv = %v", argv)
	}
	if _, err := metrics.DiskViaDF(context.Background(), func(context.Context, []string) ([]byte, error) {
		return nil, errors.New("sh: df: not found")
	}, []string{"/"}); err == nil {
		t.Fatal("DiskViaDF with no output must fail")
	}
}

func TestShellQuote(t *testing.T) {
	if got := metrics.ShellQuote("/var/lib/it's"); got != `'/var/lib/it'\''s'` {
		t.Fatalf("ShellQuote = %s", got)
	}
}
