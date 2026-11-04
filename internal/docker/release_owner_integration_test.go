//go:build integration

package docker

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

// Ownership preparation changes metadata only. A newer release's file survives,
// and the old non-root image can write again after restoring its owner setting.
func TestReleaseRestoredOwnerCanWrite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	eng, err := NewEngine("")
	if err != nil {
		t.Fatal(err)
	}
	e := eng.(*dockerEngine)
	defer e.cli.Close()
	name := fmt.Sprintf("krill-release-owner-%d", time.Now().UnixNano())
	defer e.VolumeRemove(context.Background(), name)
	var seed bytes.Buffer
	writeTar(t, &seed, map[string]string{"newer-release.txt": "keep-newer-data"})
	if err = e.VolumeRestore(ctx, name, &seed, ""); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []int{2000, 1000} {
		if err = e.VolumeChown(ctx, name, owner, owner, ""); err != nil {
			t.Fatal(err)
		}
		c, err := e.cli.ContainerCreate(ctx, &container.Config{Image: busyboxImage, User: "1000:1000", Cmd: []string{"sh", "-c", "test \"$(cat /data/newer-release.txt)\" = keep-newer-data && echo old-image-wrote >> /data/probe"}}, &container.HostConfig{NetworkMode: "none", Mounts: []mount.Mount{{Type: mount.TypeVolume, Source: name, Target: "/data"}}}, nil, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		cleanup := func() {
			if err := e.cli.ContainerRemove(context.Background(), c.ID, container.RemoveOptions{Force: true}); err != nil {
				t.Error(err)
			}
		}
		if err = e.cli.ContainerStart(ctx, c.ID, container.StartOptions{}); err != nil {
			cleanup()
			t.Fatal(err)
		}
		status, fail := e.cli.ContainerWait(ctx, c.ID, container.WaitConditionNotRunning)
		select {
		case result := <-status:
			if (result.StatusCode == 0) != (owner == 1000) {
				t.Errorf("owner %d exit %d", owner, result.StatusCode)
			}
		case err := <-fail:
			t.Error(err)
		}
		cleanup()
	}
}
