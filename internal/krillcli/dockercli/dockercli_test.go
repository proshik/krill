package dockercli_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/krillcli/dockercli"
)

func TestBuildArgs(t *testing.T) {
	tests := []struct {
		name string
		spec dockercli.BuildSpec
		want []string
	}{
		{
			name: "minimal",
			spec: dockercli.BuildSpec{Ref: "ghcr.io/acme/bot:v1"},
			want: []string{"build", "-t", "ghcr.io/acme/bot:v1", "--", "."},
		},
		{
			name: "platform and dockerfile",
			spec: dockercli.BuildSpec{
				Ref: "ghcr.io/acme/bot:v1", Platform: "linux/amd64",
				Dockerfile: "docker/Dockerfile", Context: "svc/bot",
			},
			want: []string{
				"build", "--platform", "linux/amd64", "-t", "ghcr.io/acme/bot:v1",
				"-f", "docker/Dockerfile", "--", "svc/bot",
			},
		},
		{
			name: "no cache",
			spec: dockercli.BuildSpec{Ref: "r:t", NoCache: true},
			want: []string{"build", "--no-cache", "-t", "r:t", "--", "."},
		},
		{
			// Sorted, so --dry-run prints the same command twice running and
			// a test can assert one at all.
			name: "build args are sorted",
			spec: dockercli.BuildSpec{
				Ref:       "r:t",
				BuildArgs: map[string]string{"ZED": "3", "ALPHA": "1", "MID": "2"},
			},
			want: []string{
				"build", "-t", "r:t",
				"--build-arg", "ALPHA=1",
				"--build-arg", "MID=2",
				"--build-arg", "ZED=3",
				"--", ".",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dockercli.BuildArgs(tc.spec)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("BuildArgs\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestArgvTerminatesOptions guards against a caller-supplied value that
// begins with "-" being read as a flag.
func TestArgvTerminatesOptions(t *testing.T) {
	build := dockercli.BuildArgs(dockercli.BuildSpec{Ref: "r:t", Context: "-weird"})
	if build[len(build)-2] != "--" {
		t.Fatalf("build context must be preceded by --, got %q", build)
	}
	for _, argv := range [][]string{
		dockercli.PushArgs("-weird"),
		dockercli.SaveArgs("-weird"),
		dockercli.InspectIDArgs("-weird"),
		dockercli.ManifestInspectArgs("-weird"),
	} {
		if argv[len(argv)-2] != "--" {
			t.Fatalf("argv must terminate options before its operand, got %q", argv)
		}
	}
}

// TestSaveArgsTakesAnID documents the security-relevant half: saving by
// reference writes tags into the archive that docker load would apply on the
// server, letting an upload rename an image it does not own.
func TestSaveArgsTakesAnID(t *testing.T) {
	want := []string{"save", "--", "sha256:abc"}
	if got := dockercli.SaveArgs("sha256:abc"); !reflect.DeepEqual(got, want) {
		t.Fatalf("SaveArgs = %q, want %q", got, want)
	}
}

func TestHint(t *testing.T) {
	tests := []struct {
		name, stderr, wantSubstr string
	}{
		{"denied", "denied: requested access to the resource is denied", "docker login"},
		{"unauthorized", "unauthorized: authentication required", "docker login"},
		{"unknown repo", "name unknown: repository name not known to registry", "no such repository"},
		{"no such image", "Error response from daemon: No such image: ghcr.io/a/b:v1", "buildx"},
		{"exec format", "exec /app: exec format error", "image.platform"},
		{"platform", "no match for platform in manifest", "binfmt"},
		{"daemon down", "Cannot connect to the Docker daemon at unix:///var/run/docker.sock.", "colima start"},
		{"disk full", "write /var/lib/docker: no space left on device", "prune"},
		{"unknown", "something entirely new happened", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dockercli.Hint(tc.stderr)
			if tc.wantSubstr == "" {
				if got != "" {
					t.Fatalf("unrecognized failure should get no hint, got %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantSubstr) {
				t.Fatalf("Hint(%q) = %q, want it to mention %q", tc.stderr, got, tc.wantSubstr)
			}
		})
	}
}

func TestValidateBuildArgKey(t *testing.T) {
	for _, ok := range []string{"FOO", "foo_bar", "_x", "A1"} {
		if err := dockercli.ValidateBuildArgKey(ok); err != nil {
			t.Fatalf("ValidateBuildArgKey(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "1FOO", "foo-bar", "foo bar", "FOO=BAR"} {
		if err := dockercli.ValidateBuildArgKey(bad); err == nil {
			t.Fatalf("ValidateBuildArgKey(%q) = nil, want an error", bad)
		}
	}
}
