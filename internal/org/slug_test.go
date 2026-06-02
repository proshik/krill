package org

import "testing"

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Default":          "default",
		"My Project":       "my-project",
		"Staging 2!":       "staging-2",
		"  trim  me  ":     "trim-me",
		"a__b--c":          "a-b-c",
		"日本語":              "",   // non-ascii is dropped
		"Web 日本語":          "web",
		"UPPER":            "upper",
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSlugifyEmptyIsError(t *testing.T) {
	if Slugify("") != "" {
		t.Error("empty input must produce empty slug")
	}
	if Slugify("!!!") != "" {
		t.Error("all-invalid input must produce empty slug")
	}
}
