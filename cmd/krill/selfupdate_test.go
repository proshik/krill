package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
)

// TestMain silences slog for the whole package: updateBusy.reason logs a
// warning on a query error, and the tests want deterministic, quiet output.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// fakeInFlight is a fake backup.Service/volume.VolumeService for the
// InFlight() int part updateBusy needs.
type fakeInFlight struct{ n int }

func (f fakeInFlight) InFlight() int { return f.n }

// countFn builds a fake CountRunningDeployments/CountMigratingDBInstances.
func countFn(n int64, err error) func(context.Context) (int64, error) {
	return func(context.Context) (int64, error) { return n, err }
}

func TestUpdateBusyReason(t *testing.T) {
	errBoom := errors.New("boom")

	idle := func() updateBusy {
		return updateBusy{
			runningDeploys:     countFn(0, nil),
			migratingInstances: countFn(0, nil),
			backups:            fakeInFlight{0},
			volumes:            fakeInFlight{0},
		}
	}

	tests := []struct {
		name string
		b    updateBusy
		want string
	}{
		{
			name: "all idle",
			b:    idle(),
			want: "",
		},
		{
			name: "deploy running",
			b: func() updateBusy {
				b := idle()
				b.runningDeploys = countFn(1, nil)
				return b
			}(),
			want: "deploy",
		},
		{
			name: "backup in flight",
			b: func() updateBusy {
				b := idle()
				b.backups = fakeInFlight{1}
				return b
			}(),
			want: "backup",
		},
		{
			name: "volume backup in flight",
			b: func() updateBusy {
				b := idle()
				b.volumes = fakeInFlight{1}
				return b
			}(),
			want: "volume_backup",
		},
		{
			name: "db instance migrating",
			b: func() updateBusy {
				b := idle()
				b.migratingInstances = countFn(1, nil)
				return b
			}(),
			want: "db_migration",
		},
		{
			name: "deploy and volume both busy: deploy wins (first in order)",
			b: func() updateBusy {
				b := idle()
				b.runningDeploys = countFn(1, nil)
				b.volumes = fakeInFlight{1}
				return b
			}(),
			want: "deploy",
		},
		{
			name: "backup and migrating both busy: backup wins (first in order)",
			b: func() updateBusy {
				b := idle()
				b.backups = fakeInFlight{1}
				b.migratingInstances = countFn(1, nil)
				return b
			}(),
			want: "backup",
		},
		{
			name: "running-deploys query error counts as busy",
			b: func() updateBusy {
				b := idle()
				b.runningDeploys = countFn(0, errBoom)
				return b
			}(),
			want: "deploy",
		},
		{
			name: "migrating-instances query error counts as busy",
			b: func() updateBusy {
				b := idle()
				b.migratingInstances = countFn(0, errBoom)
				return b
			}(),
			want: "db_migration",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.b.reason(context.Background()); got != tt.want {
				t.Errorf("reason() = %q, want %q", got, tt.want)
			}
		})
	}
}
