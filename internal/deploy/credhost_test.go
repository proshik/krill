package deploy

import (
	"errors"
	"testing"
)

func TestCheckGitCredentialHost(t *testing.T) {
	if err := checkGitCredentialHost("https://github.com/a/b.git", "GitHub.com"); err != nil {
		t.Fatalf("matching host must pass: %v", err)
	}
	err := checkGitCredentialHost("https://evil.example/a/b.git", "github.com")
	if !errors.Is(err, ErrCredentialHostMismatch) {
		t.Fatalf("want ErrCredentialHostMismatch, got %v", err)
	}
	// A stored host that normalizes to empty (e.g. "https://" with nothing
	// after the scheme) must fail CLOSED, not be treated as "no host to
	// check" — otherwise a credential with an unusable stored host would be
	// handed to any git_url.
	if err := checkGitCredentialHost("https://evil.example/a/b.git", "https://"); !errors.Is(err, ErrCredentialHostMismatch) {
		t.Fatalf("empty stored host must fail closed, got %v", err)
	}
}

func TestCheckRegistryHost(t *testing.T) {
	if err := checkRegistryHost("ghcr.io/proshik/krill", "https://ghcr.io"); err != nil {
		t.Fatalf("matching host must pass: %v", err)
	}
	if err := checkRegistryHost("nginx:alpine", "https://ghcr.io"); !errors.Is(err, ErrCredentialHostMismatch) {
		t.Fatalf("want ErrCredentialHostMismatch, got %v", err)
	}
	// Same fail-closed requirement for a stored registry_url that normalizes
	// to empty (e.g. "https://" or "/") — this is the exploitable path:
	// createRegistry only rejected an empty raw string, so such a value could
	// reach storage and, unchecked, would hand the registry password to
	// whatever host the image names.
	if err := checkRegistryHost("ghcr.io/proshik/krill", "https://"); !errors.Is(err, ErrCredentialHostMismatch) {
		t.Fatalf("empty stored registry host must fail closed, got %v", err)
	}
}
