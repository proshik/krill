package deploy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/docker"
)

func TestPredeploySecretStream(t *testing.T) {
	for size := 1; size <= 24; size++ {
		var out bytes.Buffer
		w := newSecretWriter(&out, map[string]string{"TOKEN": "secret-value", "PREFIX": "secret", "MULTILINE": "line1\nline2"})
		input := "before secret-value / line1\nline2 after secret-value"
		for len(input) > 0 {
			n := min(size, len(input))
			if _, err := w.Write([]byte(input[:n])); err != nil {
				t.Fatal(err)
			}
			input = input[n:]
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		if got := out.String(); got != "before [redacted] / [redacted] after [redacted]" {
			t.Fatalf("chunk %d: %q", size, got)
		}
	}
}

type predeployEngine struct {
	*digestMockEngine
	calls   []docker.PredeploySpec
	err     error
	localID string
	chowns  int
}

func (e *predeployEngine) LocalImageID(context.Context, string) (string, error) {
	return e.localID, nil
}
func (e *predeployEngine) CleanupPredeploy(context.Context) error { return nil }
func (e *predeployEngine) RunPredeploy(_ context.Context, s docker.PredeploySpec, w io.Writer) error {
	if len(e.deployed) != 0 || e.chowns != 0 {
		return errors.New("service mutated before predeploy")
	}
	e.calls = append(e.calls, s)
	io.WriteString(w, "secret-")
	io.WriteString(w, "value\n")
	return e.err
}
func (e *predeployEngine) VolumeChown(context.Context, string, int, int, string) error {
	e.chowns++
	return nil
}

func TestPredeployGatesServiceMutation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fail, skip bool
	}{{"success", false, false}, {"nonzero", true, false}, {"system", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			a := imageApp()
			a.PredeployCommand = "migrate"
			a.Env = map[string]string{"TOKEN": "secret-value"}
			a.Network = "org-network"
			a.Mounts = []docker.MountSpec{{Source: "data", Owner: "1000:1000"}}
			e := &predeployEngine{digestMockEngine: &digestMockEngine{digest: "repo@sha256:abc"}}
			if tc.fail {
				e.err = errors.New("predeploy exited with status 7")
			}
			st := newFakeStore(a)
			d := newDeployer(e, &mockBuilder{}, st)
			d.SetResourceDefaults(512, 100)
			id := int64(0)
			if tc.skip {
				id = d.EnqueueSystem(context.Background(), a.ID)
			} else {
				id = d.Enqueue(a.ID, TriggerManual)
			}
			j := <-d.queue
			d.run(context.Background(), j)
			if tc.skip {
				if len(e.calls) != 0 {
					t.Fatal("system command ran")
				}
			} else {
				if len(e.calls) != 1 {
					t.Fatal("command did not run exactly once")
				}
				c := e.calls[0]
				if c.Network != "org-network" || c.Env["TOKEN"] != "secret-value" || c.MemoryLimitBytes != 512 || c.Image != "repo@sha256:abc" {
					t.Fatalf("wrong predeploy spec: %+v", c)
				}
			}
			if tc.fail && !tc.skip {
				if len(e.deployed) != 0 || e.chowns != 0 || st.deploys[id] != "error" {
					t.Fatal("failed command mutated service or volumes")
				}
			} else if len(e.deployed) != 1 || e.chowns != 1 {
				t.Fatal("successful command did not deploy")
			}
		})
	}
}

func TestPredeployRequiresImmutableResolution(t *testing.T) {
	a := imageApp()
	a.PredeployCommand = "migrate"
	e := &predeployEngine{digestMockEngine: &digestMockEngine{digestErr: errors.New("registry offline")}}
	d := newDeployer(e, &mockBuilder{}, newFakeStore(a))
	d.run(context.Background(), job{deployID: 1, appID: a.ID})
	if len(e.calls) != 0 || len(e.deployed) != 0 {
		t.Fatal("mutable tag executed")
	}
}
func TestPredeployMasksEngineError(t *testing.T) {
	a := imageApp()
	a.PredeployCommand = "private-command"
	a.Env = map[string]string{"TOKEN": "secret-value"}
	e := &predeployEngine{digestMockEngine: &digestMockEngine{}, err: errors.New("private-command failed with secret-value")}
	d := newDeployer(e, &mockBuilder{}, newFakeStore(a))
	var out bytes.Buffer
	err := d.predeploy(context.Background(), a, 1, "repo@sha256:abc", &out)
	if err == nil || strings.Contains(err.Error(), "secret-value") || strings.Contains(err.Error(), "private-command") || strings.Contains(out.String(), "secret-value") {
		t.Fatal("secret leaked")
	}
}
