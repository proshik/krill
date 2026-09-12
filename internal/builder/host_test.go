package builder

import "testing"

func TestNormalizeHost(t *testing.T) {
	cases := map[string]string{
		"GitHub.com":           "github.com",
		"https://github.com":   "github.com",
		"https://github.com/":  "github.com",
		"git.example.com:8443": "git.example.com:8443",
		"  github.com  ":       "github.com",
	}
	for in, want := range cases {
		if got := NormalizeHost(in); got != want {
			t.Fatalf("NormalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHostOf(t *testing.T) {
	got, err := HostOf("https://github.com/proshik/krill.git")
	if err != nil || got != "github.com" {
		t.Fatalf("HostOf = %q, %v; want github.com, nil", got, err)
	}
	if _, err := HostOf(":://bad"); err == nil {
		t.Fatal("HostOf must reject a malformed URL")
	}
}

// TestBareHost covers the netguard.CheckHost input: HostOf/NormalizeHost keep
// the port (needed to compare against a stored credential host), but
// net.LookupIPAddr — which netguard.CheckHost calls — errors on a "host:port"
// string. A self-hosted git server on a non-standard port must still resolve.
func TestBareHost(t *testing.T) {
	cases := map[string]string{
		"git.example.com:8443": "git.example.com",
		"github.com":           "github.com",
		"localhost:5000":       "localhost",
		"127.0.0.1:5000":       "127.0.0.1",
	}
	for in, want := range cases {
		if got := bareHost(in); got != want {
			t.Fatalf("bareHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestGitURLHostReachesNetguardAsBareHost drives the actual path netguard
// sees: HostOf(gitURL) followed by bareHost(...), the same two calls Build
// makes before netguard.CheckHost. A host:port git_url must come out as a
// bare, resolvable host.
func TestGitURLHostReachesNetguardAsBareHost(t *testing.T) {
	host, err := HostOf("https://git.example.com:8443/org/repo.git")
	if err != nil {
		t.Fatalf("HostOf: %v", err)
	}
	if got := bareHost(host); got != "git.example.com" {
		t.Fatalf("bareHost(HostOf(...)) = %q, want %q", got, "git.example.com")
	}
}
