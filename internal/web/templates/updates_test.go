package templates

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestUpdateStateLine(t *testing.T) {
	cases := []struct {
		key      string
		want     string
		wantLink bool
	}{
		{"update.state.on", "will be backed up first.", false},
		{"update.state.off", "is not backed up before updates", true},
		{"update.state.paused", "Krill backups are paused: this update will not be preceded by a backup.", false},
	}
	for _, tc := range cases {
		var buf bytes.Buffer
		if err := updateStateLine(7, tc.key).Render(context.Background(), &buf); err != nil {
			t.Fatalf("%s: render: %v", tc.key, err)
		}
		body := buf.String()
		if !strings.Contains(body, tc.want) {
			t.Errorf("%s: line %q does not contain %q", tc.key, body, tc.want)
		}
		if got := strings.Contains(body, `href="/orgs/7/krill-backups"`); got != tc.wantLink {
			t.Errorf("%s: link to the Krill backups page = %v, want %v (%q)", tc.key, got, tc.wantLink, body)
		}
	}
}
