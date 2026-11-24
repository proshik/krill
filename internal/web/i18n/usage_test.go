package i18n

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// keyCall finds a catalog key passed as a literal: to i18n.T / i18n.Tf, or to
// the server's flash helpers (flashErrT, flashErrErr, flashOK, ...).
// pluralCall matches i18n.Tn, whose key names a set of plural forms.
var pluralCall = regexp.MustCompile(`i18n\.Tn\(\s*[A-Za-z_.()]+,\s*"([a-z][a-z0-9_]*(?:\.[a-z0-9_]+)+)"`)

var keyCall = regexp.MustCompile(`(?:i18n\.Tf?\(\s*[A-Za-z_.()]+,\s*|\.flash(?:ErrT|ErrErr|OK|OKf|ErrTf)\(\s*w,\s*r,\s*)"([a-z][a-z0-9_]*(?:\.[a-z0-9_]+)+)"`)

// Every key the code passes as a literal exists in both catalogs: removing an
// unused key must never take one that is still used.
func TestLiteralKeysExistInBothCatalogs(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	refs := map[string]string{} // key -> first file using it
	files, err := sourceFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		name := filepath.Base(path)
		if strings.HasSuffix(name, "_templ.go") || !(strings.HasSuffix(name, ".go") || strings.HasSuffix(name, ".templ")) {
			continue
		}
		b, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue // tracked, deleted in the working tree
		}
		if err != nil {
			t.Fatal(err)
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
		for _, m := range pluralCall.FindAllStringSubmatch(src, -1) {
			for _, form := range []string{"one", "few", "many"} {
				if _, seen := refs[m[1]+"."+form]; !seen {
					refs[m[1]+"."+form] = path
				}
			}
		}
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

// sourceFiles lists the repository's own files: tracked ones and new ones git
// does not ignore. A checkout also holds ignored trees — old acceptance
// sources under .superpowers/, build output under bin/, other tools' scratch
// directories — whose code may use keys this tree no longer has, and that
// made the test fail in the main checkout. Without git, the tree is walked
// instead, skipping hidden directories and the usual output ones.
func sourceFiles(root string) ([]string, error) {
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", "*.go", "*.templ").Output()
	if err == nil {
		var files []string
		for _, f := range bytes.Split(out, []byte{0}) {
			if len(f) > 0 {
				files = append(files, filepath.Join(root, string(f)))
			}
		}
		return files, nil
	}
	var files []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || skipDirs[d.Name()]) {
				return filepath.SkipDir
			}
			return nil
		}
		files = append(files, path)
		return nil
	})
	return files, err
}

var skipDirs = map[string]bool{"node_modules": true, "bin": true, "dist": true, "tools": true}
