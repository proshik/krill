package server

import (
	"context"
	"testing"
)

// A resource's backup summary looks at every configuration, not only the
// oldest one.
func TestBackupSummaryAcrossConfigs(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		cfgs []backupConfig
		want string
	}{
		{"a newer failure behind an older success", []backupConfig{{Enabled: true, Status: "ok", Ran: true}, {Enabled: true, Status: "error", Ran: true}}, "err"},
		{"one paused, another running fine", []backupConfig{{Enabled: false}, {Enabled: true, Status: "ok", Ran: true}}, "ok"},
		{"all paused", []backupConfig{{Enabled: false, Status: "ok", Ran: true}, {Enabled: false}}, "idle"},
		{"a paused failure does not fail the resource", []backupConfig{{Enabled: false, Status: "error", Ran: true}, {Enabled: true, Status: "ok", Ran: true}}, "ok"},
		{"running but never ran", []backupConfig{{Enabled: true}}, "idle"},
	} {
		if kind, _ := backupSummary(ctx, c.cfgs); kind != c.want {
			t.Errorf("%s: got %s, want %s", c.name, kind, c.want)
		}
	}
}
