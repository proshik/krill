// Package volume holds app-volume validation (Stage A) and, later, the S3
// backup/restore service (Stage B).
package volume

import (
	"errors"
	"path/filepath"
	"regexp"
	"strings"
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// forbiddenExact: mount paths that must never be overlaid by a volume.
var forbiddenExact = map[string]bool{
	"/": true, "/etc": true, "/root": true, "/boot": true,
	"/run": true, "/var/run": true, "/proc": true, "/sys": true, "/dev": true,
}

// forbiddenTrees: paths under which mounting is always rejected.
var forbiddenTrees = []string{"/proc", "/sys", "/dev"}

// ValidateAppVolume checks a volume name and mount path. name is a short slug
// (so docker.VolumeName is always safe); mountPath must be a clean absolute path
// that does not overlay a sensitive container directory. The returned error is
// user-facing (shown via flash).
func ValidateAppVolume(name, mountPath string) error {
	if !nameRe.MatchString(name) {
		return errors.New("name must match ^[a-z0-9][a-z0-9-]{0,31}$ (lowercase letters, digits, dashes; up to 32 chars)")
	}
	p := mountPath
	if !strings.HasPrefix(p, "/") {
		return errors.New("mount path must be absolute (start with /)")
	}
	if strings.ContainsAny(p, "` \t") {
		return errors.New("mount path must not contain spaces, tabs or backticks")
	}
	if p != filepath.Clean(p) {
		return errors.New("mount path must be clean (no .., ., trailing or double slashes)")
	}
	for _, seg := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if strings.HasPrefix(seg, "-") {
			return errors.New("mount path segments must not start with '-'")
		}
	}
	if forbiddenExact[p] {
		return errors.New("mount path overlays a system directory: " + p)
	}
	for _, t := range forbiddenTrees {
		if p == t || strings.HasPrefix(p, t+"/") {
			return errors.New("mount path overlays a system directory: " + p)
		}
	}
	return nil
}
