package templates

import "testing"

func TestStatusLabel(t *testing.T) {
	cases := map[string]string{
		"running":      "running",
		"node_down":    "node down",
		"node_removed": "node removed",
	}
	for in, want := range cases {
		if got := statusLabel(in); got != want {
			t.Errorf("statusLabel(%q)=%q want %q", in, got, want)
		}
	}
}
