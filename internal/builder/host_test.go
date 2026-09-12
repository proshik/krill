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
