package i18n

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// keyCall finds a catalog key passed as a literal: to i18n.T / i18n.Tf, or to
// the server's flash helpers (flashErrT, flashErrErr, flashOK, ...).
var keyCall = regexp.MustCompile(`(?:i18n\.Tf?\(\s*[A-Za-z_.()]+,\s*|\.flash(?:ErrT|ErrErr|OK|OKf|ErrTf)\(\s*w,\s*r,\s*)"([a-z][a-z0-9_]*(?:\.[a-z0-9_]+)+)"`)

// Every key the code passes as a literal exists in both catalogs: removing an
// unused key must never take one that is still used.
func TestLiteralKeysExistInBothCatalogs(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	refs := map[string]string{} // key -> first file using it
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "docs", "bin", "tools":
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, "_templ.go") || !(strings.HasSuffix(name, ".go") || strings.HasSuffix(name, ".templ")) {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(b)
		for _, m := range keyCall.FindAllStringSubmatchIndex(src, -1) {
			// A literal followed by "+" is the prefix of a key built at run
			// time ("deploy.trigger_" + trigger), not a key.
			if strings.HasPrefix(strings.TrimLeft(src[m[1]:], " "), "+") {
				continue
			}
			key := src[m[2]:m[3]]
			if _, seen := refs[key]; !seen {
				refs[key] = path
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) < 500 {
		t.Fatalf("found only %d literal keys: the scan is broken", len(refs))
	}
	for key, path := range refs {
		for _, loc := range []string{"en", "ru"} {
			if _, ok := catalogs[loc][key]; !ok {
				t.Errorf("%s uses %q, missing from the %s catalog", path, key, loc)
			}
		}
	}
}
