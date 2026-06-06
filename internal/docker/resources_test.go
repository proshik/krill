package docker

import "testing"

func TestParseMemoryBytes(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		err  bool
	}{
		{"", 0, false},
		{"256m", 256 * 1024 * 1024, false},
		{"1g", 1024 * 1024 * 1024, false},
		{"abc", 0, true},
		{"0", 0, true},
	}
	for _, c := range cases {
		got, err := ParseMemoryBytes(c.in)
		if (err != nil) != c.err {
			t.Fatalf("%q: err=%v want err=%v", c.in, err, c.err)
		}
		if !c.err && got != c.want {
			t.Errorf("%q: got %d want %d", c.in, got, c.want)
		}
	}
}

func TestParseNanoCPUs(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		err  bool
	}{
		{"", 0, false},
		{"0.5", 500_000_000, false},
		{"2", 2_000_000_000, false},
		{"x", 0, true},
		{"0", 0, true},
	}
	for _, c := range cases {
		got, err := ParseNanoCPUs(c.in)
		if (err != nil) != c.err {
			t.Fatalf("%q: err=%v want err=%v", c.in, err, c.err)
		}
		if !c.err && got != c.want {
			t.Errorf("%q: got %d want %d", c.in, got, c.want)
		}
	}
}

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"start-dev", []string{"start-dev"}},
		{"start --optimized", []string{"start", "--optimized"}},
		{"  a   b\tc ", []string{"a", "b", "c"}},
	}
	for _, c := range cases {
		got := SplitCommand(c.in)
		if len(got) != len(c.want) {
			t.Errorf("%q: got %v want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%q[%d]: got %q want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}
