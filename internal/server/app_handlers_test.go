package server

import (
	"reflect"
	"testing"
)

func TestIsSlug(t *testing.T) {
	ok := []string{"web", "my-app", "a1", "x-y-z"}
	bad := []string{"", "Web", "my_app", "a b", "café"}
	for _, s := range ok {
		if !isSlug(s) {
			t.Errorf("isSlug(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if isSlug(s) {
			t.Errorf("isSlug(%q) = true, want false", s)
		}
	}
}

func TestParseEnv(t *testing.T) {
	got := parseEnv("FOO=bar\n  BAZ = qux \n\nINVALID\nK=v=w")
	want := map[string]string{"FOO": "bar", "BAZ": "qux", "K": "v=w"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseEnv = %#v, want %#v", got, want)
	}
}
