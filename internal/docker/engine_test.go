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
