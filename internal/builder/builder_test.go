package builder

import (
	"reflect"
	"testing"
)

func TestCloneArgs(t *testing.T) {
	got := cloneArgs("https://github.com/x/y.git", "main", "/tmp/ctx")
	want := []string{"clone", "--branch", "main", "--depth", "1", "https://github.com/x/y.git", "/tmp/ctx"}
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
