package docker

import "testing"

func TestImageHost(t *testing.T) {
	cases := map[string]string{
		"nginx:alpine":                  "docker.io",
		"library/nginx":                 "docker.io",
		"ghcr.io/proshik/krill:v1":      "ghcr.io",
		"registry.example.com:5000/a/b": "registry.example.com:5000",
		"localhost:5000/app":            "localhost:5000",
	}
	for in, want := range cases {
		if got := ImageHost(in); got != want {
			t.Fatalf("ImageHost(%q) = %q, want %q", in, got, want)
		}
	}
}
