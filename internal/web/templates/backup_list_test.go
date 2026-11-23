package templates

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/proshik/krill/internal/backup"
)

// Two archives of the same minute and size must not look alike: the list
// shows each one's time to the second and its file name, and each restore
// confirmation repeats them.
func TestBackupListTellsArchivesApart(t *testing.T) {
	at := time.Date(2026, 11, 22, 3, 0, 0, 0, time.Local)
	objs := []backup.Object{
		{Key: "krill/shop/main/20261122T030005Z.sql.gz", Size: 1024, LastModified: at.Add(5 * time.Second)},
		{Key: "krill/shop/main/20261122T030041Z.sql.gz", Size: 1024, LastModified: at.Add(41 * time.Second)},
	}
	var buf bytes.Buffer
	if err := backupObjectList(objs, "", "/restore", "the database is replaced").Render(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	for _, want := range []string{"03:00:05", "03:00:41", "20261122T030005Z.sql.gz", "20261122T030041Z.sql.gz",
		"2026-11-22 03:00:05, 20261122T030005Z.sql.gz", "2026-11-22 03:00:41, 20261122T030041Z.sql.gz"} {
		if !strings.Contains(html, want) {
			t.Errorf("the list lacks %q", want)
		}
	}
}
