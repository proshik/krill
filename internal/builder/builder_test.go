package builder

import (
	"reflect"
	"testing"
)

func TestCloneArgs(t *testing.T) {
	got := cloneArgs("https://github.com/x/y.git", "main", "/tmp/ctx")
	want := []string{"clone", "--branch", "main", "--depth", "1", "--", "https://github.com/x/y.git", "/tmp/ctx"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cloneArgs = %v, want %v", got, want)
	}
}

func TestBuildArgs(t *testing.T) {
	got := buildArgs("krill-7:42", "/tmp/ctx/sub/Dockerfile", "/tmp/ctx/sub")
	want := []string{"build", "-t", "krill-7:42", "-f", "/tmp/ctx/sub/Dockerfile", "/tmp/ctx/sub"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildArgs = %v, want %v", got, want)
	}
}

func TestContextDir(t *testing.T) {
	// Dockerfile в корне репо → контекст = корень
	if d := contextDir("/tmp/ctx", "Dockerfile"); d != "/tmp/ctx" {
		t.Errorf("contextDir root = %q", d)
	}
	// Dockerfile в подкаталоге → контекст = этот подкаталог
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
