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
}

func TestCheckRegistryHost(t *testing.T) {
	if err := checkRegistryHost("ghcr.io/proshik/krill", "https://ghcr.io"); err != nil {
		t.Fatalf("matching host must pass: %v", err)
	}
	if err := checkRegistryHost("nginx:alpine", "https://ghcr.io"); !errors.Is(err, ErrCredentialHostMismatch) {
		t.Fatalf("want ErrCredentialHostMismatch, got %v", err)
	}
}
