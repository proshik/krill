package buildinfo

import "testing"

// TestString_ReleaseVersion pins the linker-stamped path: when Version has
// been set to anything other than the "dev" fallback, String() returns it
// verbatim with no build-info fallback logic involved.
func TestString_ReleaseVersion(t *testing.T) {
	original := Version
	defer func() { Version = original }()

	Version = "v0.1.0"
	if got := String(); got != "v0.1.0" {
		t.Errorf("String() = %q, want %q", got, "v0.1.0")
	}
}

func TestIsRelease(t *testing.T) {
	tests := []struct {
		name string
		v    string
		want bool
	}{
		{"release tag", "v0.1.0", true},
		{"release tag with patch bump", "v1.2.3", true},
		{"dev fallback", "dev", false},
		{"prerelease tag", "v0.2.0-rc1", false},
		{"dev plus commit", "dev+abc", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsRelease(tt.v); got != tt.want {
				t.Errorf("IsRelease(%q) = %v, want %v", tt.v, got, tt.want)
			}
		})
	}
}
