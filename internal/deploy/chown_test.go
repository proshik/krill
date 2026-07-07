package deploy

import "testing"

func TestChownNodes(t *testing.T) {
	cases := []struct {
		mode  string
		nodes []string
		want  []string
	}{
		{"pin", []string{"n1", "n2"}, []string{"n1", "n2"}},
		{"global", []string{"n1", "n2"}, []string{"n1", "n2"}},
		{"any", nil, []string{""}},
		{"global", nil, []string{""}},
		{"pin", nil, []string{""}},
	}
	for _, c := range cases {
		got := chownNodes(App{PlacementMode: c.mode, PlacementNodes: c.nodes})
		if len(got) != len(c.want) {
			t.Fatalf("chownNodes(%s,%v) = %v, want %v", c.mode, c.nodes, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("chownNodes(%s,%v) = %v, want %v", c.mode, c.nodes, got, c.want)
			}
		}
	}
}
