package deploy

import (
	"testing"

	"github.com/proshik/krill/internal/docker"
)

func TestDeriveStatus(t *testing.T) {
	tests := []struct {
		name  string
		state docker.ServiceState
		db    string
		want  string
	}{
		{"not found falls back to db", docker.ServiceState{Found: false}, "idle", "idle"},
		{"not found keeps a stored error", docker.ServiceState{Found: false}, "error", "error"},
		{"running", docker.ServiceState{Found: true, Running: 1, Desired: 1}, "deploying", "running"},
		{"converging", docker.ServiceState{Found: true, Running: 0, Desired: 1}, "deploying", "deploying"},
		{"zero desired", docker.ServiceState{Found: true, Running: 0, Desired: 0}, "idle", "idle"},

		// A service scaled to zero is stopped, whatever the stored column says.
		// Reading the column here let a stop performed through a surface that
		// does not write it (the agent API) report "running" for an app with
		// 0/0 replicas, while the same stop from the UI reported "idle".
		{"zero desired overrides a stale running", docker.ServiceState{Found: true, Running: 0, Desired: 0}, "running", "idle"},
		{"zero desired overrides a stale deploying", docker.ServiceState{Found: true, Running: 0, Desired: 0}, "deploying", "idle"},
	}
	for _, tc := range tests {
		if got := DeriveStatus(tc.state, tc.db); got != tc.want {
			t.Errorf("%s: DeriveStatus=%q want %q", tc.name, got, tc.want)
		}
	}
}
