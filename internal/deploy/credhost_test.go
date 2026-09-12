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

// Docker Hub answers to several names, and Docker itself advertises more than
// one of them. A private Hub image is usually written without a host, which
// resolves to docker.io, while the stored registry URL may name any alias: each
// must be accepted, with the image written hostless or with any alias too.
func TestCheckRegistryHostAcceptsDockerHubAliases(t *testing.T) {
	registries := []string{"docker.io", "https://index.docker.io/v1/", "index.docker.io", "registry-1.docker.io", "https://registry-1.docker.io"}
	images := []string{"acme/private-api", "nginx", "docker.io/acme/private-api", "index.docker.io/acme/private-api", "registry-1.docker.io/acme/private-api"}
	for _, reg := range registries {
		for _, img := range images {
			if err := checkRegistryHost(img, reg); err != nil {
				t.Errorf("image %q with registry %q must pass: %v", img, reg, err)
			}
		}
	}
	// The aliases are Hub's only: another registry still does not match a Hub image.
	if err := checkRegistryHost("acme/private-api", "https://ghcr.io"); !errors.Is(err, ErrCredentialHostMismatch) {
		t.Fatalf("a non-Hub registry must still be refused for a Hub image, got %v", err)
	}
	if err := checkRegistryHost("ghcr.io/acme/api", "index.docker.io"); !errors.Is(err, ErrCredentialHostMismatch) {
		t.Fatalf("a Hub registry must still be refused for a non-Hub image, got %v", err)
	}
}
