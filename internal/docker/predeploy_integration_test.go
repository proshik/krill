//go:build integration

package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
)

func TestPredeployRealContainer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	eng, err := NewEngine("")
	if err != nil {
		t.Fatal(err)
	}
	e := eng.(*dockerEngine)
	defer e.cli.Close()
	appID := time.Now().UnixNano()
	name := fmt.Sprintf("krill-predeploy-test-%d", appID)
	if _, err := e.NetworkEnsure(ctx, name); err != nil {
		t.Fatal(err)
	}
	defer e.NetworkRemove(context.Background(), name)
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	dockerfile := "FROM traefik:v3.6.1\nUSER 65534\nWORKDIR /tmp\nENTRYPOINT [\"false\"]\nHEALTHCHECK CMD false\n"
	if err := tw.WriteHeader(&tar.Header{Name: "Dockerfile", Mode: 0600, Size: int64(len(dockerfile))}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte(dockerfile))
	tw.Close()
	built, err := e.cli.ImageBuild(ctx, &archive, build.ImageBuildOptions{Tags: []string{name}, Remove: true})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, built.Body)
	built.Body.Close()
	id, err := e.LocalImageID(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	defer e.cli.ImageRemove(context.Background(), name, image.RemoveOptions{Force: true})
	if err := e.ValidateRelease(ctx, ServiceSpec{Image: id, Network: name}, nil); err != nil {
		t.Fatalf("valid release rejected: %v", err)
	}
	if err := e.ValidateRelease(ctx, ServiceSpec{Image: id, Network: name, Mounts: []MountSpec{{Type: "volume", Source: name + "-missing", Target: "/data"}}}, nil); err == nil {
		t.Fatal("missing volume accepted")
	}
	if _, err := e.cli.VolumeInspect(ctx, name+"-missing"); err == nil {
		t.Fatal("validation created a missing volume")
	}
	if err := e.ValidateRelease(ctx, ServiceSpec{Image: "sha256:" + strings.Repeat("f", 64), Network: name}, nil); err == nil {
		t.Fatal("missing local image accepted")
	}
	if err := e.ValidateRelease(ctx, ServiceSpec{Image: id, Network: name}, []string{"missing-node"}); err == nil {
		t.Fatal("missing node accepted")
	}
	for i, tc := range []struct {
		name, command string
		deadline      time.Duration
		failure       bool
	}{{"success", "id -u; pwd; printf '%s' \"$VALUE\"", time.Minute, false}, {"nonzero", "echo failed >&2; exit 7", time.Minute, true}, {"cancel", "echo started; sleep 60", 2 * time.Second, true}} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			cctx, stop := context.WithTimeout(ctx, tc.deadline)
			defer stop()
			err := e.RunPredeploy(cctx, PredeploySpec{AppID: appID, DeploymentID: int64(i + 1), Image: id, Command: tc.command, Network: name, Env: map[string]string{"VALUE": "image-env-ok"}, MemoryLimitBytes: 64 << 20, NanoCPUs: 100000000}, &out)
			if (err != nil) != tc.failure {
				t.Fatalf("err=%v output=%s", err, out.String())
			}
			if !tc.failure && (!strings.Contains(out.String(), "65534\n/tmp\nimage-env-ok")) {
				t.Fatalf("image defaults/entrypoint/env not preserved: %s", out.String())
			}
			list, err := e.cli.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("name", fmt.Sprintf("krill-predeploy-%d-%d", appID, i+1)))})
			if err != nil || len(list) != 0 {
				t.Fatalf("container leaked: %v %v", list, err)
			}
		})
	}
}
