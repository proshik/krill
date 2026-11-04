//go:build integration

package docker

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"
)

// Exercise all three sidecar callers on disposable volumes, both through the
// empty-node local convention and through an explicitly verified manager ID.
func TestVolumeSidecarCleanupRoundtrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	eng, err := NewEngine("")
	if err != nil {
		t.Fatal(err)
	}
	e := eng.(*dockerEngine)
	defer e.cli.Close()
	info, err := e.cli.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Swarm.NodeID == "" {
		t.Fatal("Swarm manager required")
	}
	for _, node := range []string{"", info.Swarm.NodeID} {
		t.Run(node, func(t *testing.T) {
			name := fmt.Sprintf("krill-sidecar-cleanup-%d", time.Now().UnixNano())
			defer func() {
				if err := e.VolumeRemove(context.Background(), name); err != nil {
					t.Error(err)
				}
			}()
			var seed bytes.Buffer
			writeTar(t, &seed, map[string]string{"data.txt": "preserved"})
			if err := e.VolumeRestore(ctx, name, &seed, node); err != nil {
				t.Fatal(err)
			}
			if err := e.VolumeChown(ctx, name, 1000, 1000, node); err != nil {
				t.Fatal(err)
			}
			var archive bytes.Buffer
			if err := e.VolumeArchive(ctx, name, &archive, node); err != nil {
				t.Fatal(err)
			}
			files := tarFiles(t, &archive)
			if files["./data.txt"] != "preserved" && files["data.txt"] != "preserved" {
				t.Fatalf("roundtrip data: %v", files)
			}
		})
	}
}
