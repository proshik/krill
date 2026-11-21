package accept

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func repoInstallDoc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "install.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCheckRepoInstallDoc(t *testing.T) {
	if err := Check(repoInstallDoc(t), Runbook, Covered); err != nil {
		t.Fatal(err)
	}
}

func TestCheckMissingText(t *testing.T) {
	doc := strings.Replace(repoInstallDoc(t), "rm -f /run/krill-update-pending", "rm -f /run/krill-update-pending-x", 1)
	err := Check(doc, Runbook, Covered)
	if err == nil || !strings.Contains(err.Error(), "B. Failed upgrade on the same host: b.cancel") {
		t.Fatalf("Check = %v, want b.cancel reported", err)
	}
}

func TestCheckUnlistedLine(t *testing.T) {
	doc := strings.Replace(repoInstallDoc(t), "   rm -f /run/krill-update-pending\n", "   rm -f /run/krill-update-pending\n   touch /tmp/new-step\n", 1)
	err := Check(doc, Runbook, Covered)
	if err == nil || !strings.Contains(err.Error(), "unlisted command line: touch /tmp/new-step") {
		t.Fatalf("Check = %v, want the new line reported", err)
	}
}

const continuationDoc = "## Sec\n\n1. Do it:\n   ```sh\n   do-thing --a \\\n     --b   # done\n   ```\n2. Then:\n   ```sql\n   UPDATE x SET y = 1;\n   ```\n\n## Next\n"

func TestCheckContinuationLines(t *testing.T) {
	both := []Command{
		{ID: "x.do", Section: "Sec", Text: "do-thing --a \\\n--b   # done"},
		{ID: "x.sql", Section: "Sec", Text: "UPDATE x SET y = 1;"},
	}
	if err := Check(continuationDoc, both, []string{"Sec"}); err != nil {
		t.Fatalf("continued command not matched: %v", err)
	}
	err := Check(continuationDoc, both[:1], []string{"Sec"})
	if err == nil || !strings.Contains(err.Error(), "unlisted command line: UPDATE x SET y = 1;") || strings.Contains(err.Error(), "--b") {
		t.Fatalf("Check = %v, want only the SQL line reported", err)
	}
}

func TestSectionIgnoresHashInFence(t *testing.T) {
	doc := "### One\n```sh\n# a comment, not a heading\necho hi\n```\nafter\n### Two\nother\n"
	body, err := Section(doc, "One")
	if err != nil || !strings.Contains(body, "after") || strings.Contains(body, "other") {
		t.Fatalf("Section = %q, %v", body, err)
	}
	if _, err := Section(doc, "Three"); err == nil {
		t.Fatal("missing section accepted")
	}
}

func TestLookupUnknownPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Lookup of an unknown id did not panic")
		}
	}()
	Lookup("no.such.command")
}

func TestEveryNonVerbatimHasWhy(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Runbook {
		if seen[c.ID] {
			t.Errorf("duplicate id %s", c.ID)
		}
		seen[c.ID] = true
		if c.Mode != Verbatim && c.Why == "" {
			t.Errorf("%s is %s without a reason", c.ID, c.Mode)
		}
	}
}

func TestFill(t *testing.T) {
	got := Fill(Lookup("dump.verify").Text, map[string]string{"<object>": "obj.sql.gz.age"})
	if !strings.Contains(got, "krill-backup-key.txt obj.sql.gz.age |") || strings.Contains(got, "<object>") {
		t.Fatalf("Fill = %q", got)
	}
}
