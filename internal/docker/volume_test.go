package docker

import "testing"

func TestVolumeName(t *testing.T) {
	if got := VolumeName(3, "data"); got != "krill-vol-3-data" {
		t.Fatalf("VolumeName(3,\"data\") = %q, want krill-vol-3-data", got)
	}
	if got := VolumeName(42, "uploads"); got != "krill-vol-42-uploads" {
		t.Fatalf("VolumeName(42,\"uploads\") = %q, want krill-vol-42-uploads", got)
	}
}
