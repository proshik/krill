package builder

import (
	"reflect"
	"strings"
	"testing"
)

func TestSanitizeGitURL(t *testing.T) {
	cases := map[string]string{
		"https://user:token@github.com/x/y.git":         "https://github.com/x/y.git",
		"https://x-access-token:ghp_abc@github.com/o/r":  "https://github.com/o/r",
		"https://github.com/x/y.git":                     "https://github.com/x/y.git",
		"git@github.com:x/y.git":                         "git@github.com:x/y.git", // scp-style, nothing to strip
	}
	for in, want := range cases {
		if got := sanitizeGitURL(in); got != want {
			t.Errorf("sanitizeGitURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCloneArgs(t *testing.T) {
	got := cloneArgs("https://github.com/x/y.git", "main", "/tmp/ctx")
	want := []string{"clone", "--branch", "main", "--depth", "1", "--", "https://github.com/x/y.git", "/tmp/ctx"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cloneArgs = %v, want %v", got, want)
	}
}

func TestBuildArgs(t *testing.T) {
	got := buildArgs("krill-7:42", "/tmp/ctx/sub/Dockerfile", "/tmp/ctx/sub", false, nil, nil)
	want := []string{"build", "-t", "krill-7:42", "-f", "/tmp/ctx/sub/Dockerfile", "/tmp/ctx/sub"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildArgs = %v, want %v", got, want)
	}
}

func TestBuildArgsNoCache(t *testing.T) {
	got := buildArgs("krill-7:42", "/tmp/ctx/sub/Dockerfile", "/tmp/ctx/sub", true, nil, nil)
	want := []string{"build", "--no-cache", "-t", "krill-7:42", "-f", "/tmp/ctx/sub/Dockerfile", "/tmp/ctx/sub"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildArgs no-cache = %v, want %v", got, want)
	}
}

func TestCloneURLWithAuth(t *testing.T) {
	got, err := cloneURLWithAuth("https://github.com/me/private.git", &GitAuth{Username: "x-access-token", Token: "ghp_abc"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://x-access-token:ghp_abc@github.com/me/private.git" {
		t.Fatalf("got %q", got)
	}
	if san := sanitizeGitURL(got); san != "https://github.com/me/private.git" {
		t.Fatalf("token leaked into sanitized URL: %q", san)
	}
	if _, err := cloneURLWithAuth("http://h/r.git", &GitAuth{Username: "u", Token: "t"}); err == nil {
		t.Fatal("expected error for non-https auth clone")
	}
	if got, _ := cloneURLWithAuth("https://github.com/x/y.git", nil); got != "https://github.com/x/y.git" {
		t.Fatalf("nil auth changed url: %q", got)
	}
}

func TestBuildArgv(t *testing.T) {
	argv := buildArgs("krill-7:1", "/d/Dockerfile", "/d", false,
		map[string]string{"VERSION": "1.2", "ENV": "prod"},
		map[string]string{"NPM_TOKEN": "/tmp/s/NPM_TOKEN"})
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--build-arg ENV=prod --build-arg VERSION=1.2") {
		t.Fatalf("build-args argv = %q", joined)
	}
	if !strings.Contains(joined, "--secret id=NPM_TOKEN,src=/tmp/s/NPM_TOKEN") {
		t.Fatalf("secret argv = %q", joined)
	}
	if argv[0] != "build" || argv[len(argv)-1] != "/d" {
		t.Fatalf("argv shape wrong: %v", argv)
	}
}

func TestValidBuildKey(t *testing.T) {
	for _, ok := range []string{"NPM_TOKEN", "_X", "A1"} {
		if !validBuildKey(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "1A", "a-b", "a b", "a=b", "a,b"} {
		if validBuildKey(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestContextDir(t *testing.T) {
	// Dockerfile at the repo root → context = root
	if d := contextDir("/tmp/ctx", "Dockerfile"); d != "/tmp/ctx" {
		t.Errorf("contextDir root = %q", d)
	}
	// Dockerfile in a subdirectory → context = that subdirectory
	if d := contextDir("/tmp/ctx", "sub/Dockerfile"); d != "/tmp/ctx/sub" {
		t.Errorf("contextDir sub = %q", d)
	}
}

func TestValidateBuildRequest(t *testing.T) {
	ok := BuildRequest{GitURL: "https://github.com/x/y.git", GitBranch: "main", DockerfilePath: "Dockerfile"}
	if err := ValidateBuildRequest(ok); err != nil {
		t.Errorf("valid request rejected: %v", err)
	}
	if err := ValidateBuildRequest(BuildRequest{GitURL: "https://github.com/x/y.git", DockerfilePath: "sub/Dockerfile"}); err != nil {
		t.Errorf("relative subdir dockerfile rejected: %v", err)
	}
	bad := []BuildRequest{
		{GitURL: "ext::sh -c id"},                                       // non-http scheme
		{GitURL: "--upload-pack=touch /tmp/x"},                          // arg-injection / no scheme
		{GitURL: "file:///etc/passwd"},                                  // file scheme
		{GitURL: "ssh://git@h/x.git"},                                   // ssh scheme
		{GitURL: "https://h/x.git", GitBranch: "-evil"},                 // dashed branch
		{GitURL: "https://h/x.git", DockerfilePath: "../../etc/passwd"}, // traversal
		{GitURL: "https://h/x.git", DockerfilePath: "/etc/passwd"},      // absolute
	}
	for i, r := range bad {
		if err := ValidateBuildRequest(r); err == nil {
			t.Errorf("bad request #%d accepted: %+v", i, r)
		}
	}
}

func TestCloneArgsHasSeparator(t *testing.T) {
	got := cloneArgs("https://h/x.git", "main", "/tmp/ctx")
	// "--" must appear before the URL operand
	sepIdx, urlIdx := -1, -1
	for i, a := range got {
		if a == "--" {
			sepIdx = i
		}
		if a == "https://h/x.git" {
			urlIdx = i
		}
	}
	if sepIdx == -1 || urlIdx == -1 || sepIdx > urlIdx {
		t.Errorf("cloneArgs must place -- before url operand: %v", got)
	}
}
