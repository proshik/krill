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
	got, dups := parseEnv("FOO=bar\n  BAZ = qux \n\nINVALID\nK=v=w\nFOO=again")
	want := map[string]string{"FOO": "again", "BAZ": "qux", "K": "v=w"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseEnv = %#v, want %#v", got, want)
	}
	if len(dups) != 1 || dups[0] != "FOO" {
		t.Errorf("parseEnv dups = %#v, want [FOO]", dups)
	}
}
