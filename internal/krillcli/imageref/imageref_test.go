package imageref_test

import (
	"strings"
	"testing"

	"github.com/proshik/krill/internal/krillcli/imageref"
	"github.com/proshik/krill/internal/webhook"
)

func TestValidTag(t *testing.T) {
	tests := []struct {
		name string
		tag  string
		want bool
	}{
		{"plain", "v1.2.3", true},
		{"latest", "latest", true},
		{"underscore first", "_build", true},
		{"digits", "20260906", true},
		{"branch and sha", "main-a1b2c3d4e5f6", true},
		{"dot inside", "v1.2.3-rc.1", true},
		{"exactly 128", strings.Repeat("a", 128), true},

		{"empty", "", false},
		{"129", strings.Repeat("a", 129), false},
		// Docker forbids these leading characters even though Krill's own
		// rule would take them — the whole reason this is an intersection.
		{"leading dot", ".wip", false},
		{"leading dash", "-hotfix", false},
		{"slash", "acme/bot", false},
		{"colon", "bot:v1", false},
		{"full reference", "ghcr.io/acme/bot:v1", false},
		{"trailing space", "v1 ", false},
		{"inner space", "v1 2", false},
		{"unicode", "версия", false},
		{"at sign", "sha256@abc", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := imageref.ValidTag(tc.tag); got != tc.want {
				t.Fatalf("ValidTag(%q) = %v, want %v", tc.tag, got, tc.want)
			}
		})
	}
}

// TestValidTagIsSubsetOfServerRule is the property that keeps the two rules
// from drifting: everything this package accepts, the server must accept too.
// If it ever failed, the CLI would build and push an image and only then
// learn the deploy call rejects the tag — after the expensive part.
//
// The converse deliberately does NOT hold: the server accepts ".wip", docker
// does not, so the CLI is the stricter of the two by design.
func TestValidTagIsSubsetOfServerRule(t *testing.T) {
	corpus := []string{
		"", "latest", "v1.2.3", "_x", ".wip", "-hotfix", "a/b", "a:b", "a b",
		"версия", strings.Repeat("a", 127), strings.Repeat("a", 128), strings.Repeat("a", 129),
		"main-a1b2c3d4e5f6", "20260906-dirty-141530", "sha256@abc", "..", "--", "__",
	}
	for _, tag := range corpus {
		if imageref.ValidTag(tag) && !webhook.ValidTag(tag) {
			t.Fatalf("CLI accepts %q but the server rejects it: a build would be wasted", tag)
		}
	}
}

func TestNormalizeRepoAndSameRepo(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		same bool
	}{
		{"identical", "ghcr.io/acme/bot", "ghcr.io/acme/bot", true},
		{"tag on one side", "ghcr.io/acme/bot", "ghcr.io/acme/bot:v1", true},
		{"digest on one side", "ghcr.io/acme/bot", "ghcr.io/acme/bot@sha256:" + strings.Repeat("a", 64), true},
		{"surrounding space", " ghcr.io/acme/bot ", "ghcr.io/acme/bot", true},

		// Docker Hub's implicit namespaces — the cases a string compare gets wrong.
		{"hub short vs library", "nginx", "docker.io/library/nginx", true},
		{"hub short vs bare library", "nginx", "library/nginx", true},
		{"hub index alias", "index.docker.io/acme/bot", "acme/bot", true},

		{"different repo", "ghcr.io/acme/bot", "ghcr.io/acme/api", false},
		{"different registry", "ghcr.io/acme/bot", "docker.io/acme/bot", false},
		// Case matters: docker repository paths are lowercase, and an
		// uppercase one is not a different spelling of the same repo, it is
		// invalid — so it must not silently compare equal.
		{"uppercase is not a spelling", "ghcr.io/Acme/Bot", "ghcr.io/acme/bot", false},
		{"both unparseable", "!!!", "!!!", false},
		{"empty", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := imageref.SameRepo(tc.a, tc.b); got != tc.same {
				t.Fatalf("SameRepo(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.same)
			}
		})
	}
}

func TestJoinKeepsTheFamiliarSpelling(t *testing.T) {
	tests := []struct {
		repo, tag, want string
	}{
		{"ghcr.io/acme/bot", "v1", "ghcr.io/acme/bot:v1"},
		{"nginx", "alpine", "nginx:alpine"},
		{"docker.io/library/nginx", "alpine", "nginx:alpine"},
		{"acme/bot", "v1", "acme/bot:v1"},
		{"ghcr.io/acme/bot:old", "v2", "ghcr.io/acme/bot:v2"},
	}
	for _, tc := range tests {
		t.Run(tc.repo+":"+tc.tag, func(t *testing.T) {
			got, err := imageref.Join(tc.repo, tc.tag)
			if err != nil {
				t.Fatalf("Join: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Join(%q, %q) = %q, want %q", tc.repo, tc.tag, got, tc.want)
			}
		})
	}
}

func TestJoinRejectsBadInput(t *testing.T) {
	if _, err := imageref.Join("ghcr.io/acme/bot", ".wip"); err == nil {
		t.Fatal("want an error for a tag docker will refuse")
	}
	if _, err := imageref.Join("!!!", "v1"); err == nil {
		t.Fatal("want an error for an unparseable repository")
	}
}
