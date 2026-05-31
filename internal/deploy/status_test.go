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
		{"running", docker.ServiceState{Found: true, Running: 1, Desired: 1}, "deploying", "running"},
		{"converging", docker.ServiceState{Found: true, Running: 0, Desired: 1}, "deploying", "deploying"},
		{"zero desired", docker.ServiceState{Found: true, Running: 0, Desired: 0}, "idle", "idle"},
	}
	for _, tc := range tests {
		if got := DeriveStatus(tc.state, tc.db); got != tc.want {
			t.Errorf("%s: DeriveStatus=%q want %q", tc.name, got, tc.want)
		}
	}
}
