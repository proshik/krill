package docker

import "testing"

func TestServiceName(t *testing.T) {
	if got := ServiceName(7); got != "krill-7" {
		t.Errorf("ServiceName(7) = %q, want krill-7", got)
	}
}

func TestBuildImageTag(t *testing.T) {
	if got := BuildImageTag(7, 42); got != "krill-7:42" {
		t.Errorf("BuildImageTag(7,42) = %q, want krill-7:42", got)
	}
}

func TestUpdateSettled(t *testing.T) {
	for state, want := range map[string]bool{
		"": true, "completed": true, "rollback_completed": true,
		"updating": false, "paused": false, "rollback_started": false, "rollback_paused": false,
	} {
		if got := UpdateSettled(state); got != want {
			t.Errorf("UpdateSettled(%q) = %v, want %v", state, got, want)
		}
	}
}
