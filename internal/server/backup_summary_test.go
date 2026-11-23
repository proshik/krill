package server

import (
	"context"
	"testing"
	"time"
)

// A resource's backup summary is its newest run's result, across every
// configuration, paused ones included; paused only when all are.
func TestBackupSummaryAcrossConfigs(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		cfgs []backupConfig
		want string
	}{
		{"a newer failure behind an older success", []backupConfig{{Enabled: true, Status: "ok", RanAt: t0}, {Enabled: true, Status: "error", RanAt: t0.Add(time.Hour)}}, "err"},
		{"an older failure, a newer success", []backupConfig{{Enabled: true, Status: "error", RanAt: t0}, {Enabled: true, Status: "ok", RanAt: t0.Add(time.Hour)}}, "ok"},
		{"a manual run failed on a paused config, newest", []backupConfig{{Enabled: false, Status: "error", RanAt: t0.Add(time.Hour)}, {Enabled: true, Status: "ok", RanAt: t0}}, "err"},
		{"one paused, another running fine", []backupConfig{{Enabled: false}, {Enabled: true, Status: "ok", RanAt: t0}}, "ok"},
		{"all paused", []backupConfig{{Enabled: false, Status: "ok", RanAt: t0}, {Enabled: false}}, "idle"},
		{"running but never ran", []backupConfig{{Enabled: true}}, "idle"},
	} {
		if kind, _ := backupSummary(ctx, c.cfgs); kind != c.want {
			t.Errorf("%s: got %s, want %s", c.name, kind, c.want)
		}
	}
}
