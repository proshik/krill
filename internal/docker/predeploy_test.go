package docker

import "testing"

func TestChoosePredeployNode(t *testing.T) {
	nodes := []SwarmNode{{ID: "worker-b", Role: "worker", State: "ready", Availability: "active"}, {ID: "manager", Role: "manager", State: "ready", Availability: "active"}, {ID: "worker-a", Role: "worker", State: "ready", Availability: "active"}, {ID: "drained", Role: "worker", State: "ready", Availability: "drain"}}
	for _, tc := range []struct {
		mode     string
		selected []string
		want     string
	}{{"any", nil, "manager"}, {"pin", []string{"worker-b", "worker-a"}, "worker-a"}, {"global", []string{"worker-b"}, "worker-b"}, {"pin", []string{"drained"}, ""}, {"global", nil, ""}} {
		got, err := ChoosePredeployNode(nodes, tc.mode, tc.selected)
		if got != tc.want || (err != nil) != (tc.want == "") {
			t.Fatalf("%s: got %q err %v", tc.mode, got, err)
		}
	}
}
