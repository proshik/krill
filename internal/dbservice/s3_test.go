package dbservice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// argvEngine fails every exec the way docker.Engine.Exec reports a non-zero
// exit: with the whole command line in the error.
type argvEngine struct {
	*mockEngine
	stdout string
}

func (e argvEngine) Exec(_ context.Context, _ string, cmd []string, _ []string, _ io.Reader, out io.Writer) error {
	io.WriteString(out, e.stdout)
	return fmt.Errorf("exec %v exited 1: boom", cmd)
}

// A failing `mc admin user add` must not carry the new secret key into the
// error a handler flashes and logs.
func TestMCFailureNeverCarriesTheSecret(t *testing.T) {
	inst := Instance{AppName: "krill-minio-x", Superuser: "root", SuperuserPassword: "root-pw-123"}
	for _, stdout := range []string{"", `{"status":"error","error":{"message":"Unable to add user","cause":{"message":"x"}}}`} {
		svc := New(argvEngine{mockEngine: newMockEngine(), stdout: stdout}, nil, nil, "krill-net")
		err := svc.AddPrincipal(context.Background(), inst, "penpot", "krill-penpot-a1", "very-secret-key")
		if err == nil {
			t.Fatal("want an error")
		}
		if strings.Contains(err.Error(), "very-secret-key") || strings.Contains(err.Error(), "root-pw-123") {
			t.Fatalf("secret leaked into the error: %v", err)
		}
	}
}

func TestLastMCResult(t *testing.T) {
	out := "noise\n{\"status\":\"success\"}\n{\"status\":\"error\",\"error\":{\"message\":\"Unable\",\"cause\":{\"message\":\"no user\",\"error\":{\"Code\":\"XMinioAdminNoSuchUser\"}}}}\n"
	r, ok := lastMCResult([]byte(out))
	if !ok || r.Status != "error" || r.Error.Cause.Error.Code != "XMinioAdminNoSuchUser" {
		t.Fatalf("got %+v %v", r, ok)
	}
	if _, ok := lastMCResult([]byte("OCI runtime exec failed: exec: \"mc\": executable file not found")); ok {
		t.Fatal("non-JSON output has no result")
	}
	svc := New(argvEngine{mockEngine: newMockEngine(), stdout: "OCI runtime exec failed: exec: \"mc\": executable file not found in $PATH"}, nil, nil, "krill-net")
	if err := svc.RemoveBucket(context.Background(), Instance{AppName: "x"}, "penpot"); !errors.Is(err, ErrMCMissing) {
		t.Fatalf("missing mc: got %v", err)
	}
}

func TestValidBucketName(t *testing.T) {
	for name, ok := range map[string]bool{"penpot": true, "penpot-assets": true, "ab": false, "Penpot": false, "a.b": false, "-ab": false, "ab-": false, "a--b": false} {
		if ValidBucketName(name) != ok {
			t.Errorf("%q: want %v", name, ok)
		}
	}
}
