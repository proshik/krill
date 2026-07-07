// Package volume holds app-volume validation (Stage A) and, later, the S3
// backup/restore service (Stage B).
package volume

import (
	"path/filepath"
	"regexp"
	"strconv"
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

// ValidationError is a user-facing validation error identified by an i18n key.
// Args are the printf args for i18n.Tf. Error() returns the key as a fallback.
type ValidationError struct {
	Key  string
	Args []any
}

func (e *ValidationError) Error() string { return e.Key }

func verr(key string, args ...any) *ValidationError { return &ValidationError{Key: key, Args: args} }

// ValidateAppVolume checks a volume name and mount path. name is a short slug
// (so docker.VolumeName is always safe); mountPath must be a clean absolute path
// that does not overlay a sensitive container directory. The returned error is a
// *ValidationError carrying an i18n key (shown via flash).
func ValidateAppVolume(name, mountPath string) error {
	if !nameRe.MatchString(name) {
		return verr("flash.err.vol_name")
	}
	p := mountPath
	if !strings.HasPrefix(p, "/") {
		return verr("flash.err.vol_mount_absolute")
	}
	if strings.ContainsAny(p, "` \t") {
		return verr("flash.err.vol_mount_spaces")
	}
	if p != filepath.Clean(p) {
		return verr("flash.err.vol_mount_clean")
	}
	for _, seg := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if strings.HasPrefix(seg, "-") {
			return verr("flash.err.vol_mount_segment_dash")
		}
	}
	if forbiddenExact[p] {
		return verr("flash.err.vol_mount_system", p)
	}
	for _, t := range forbiddenTrees {
		if p == t || strings.HasPrefix(p, t+"/") {
			return verr("flash.err.vol_mount_system", p)
		}
	}
	return nil
}

// ParseOwner validates an optional volume-owner string. Accepts "uid:gid" or a
// bare "uid" (which means uid:uid). Empty input is valid and yields normalized
// "" (no chown). uid and gid must be integers in 0..65535. Returns the parsed
// ids and the normalized "uid:gid" string. The error is a *ValidationError.
func ParseOwner(s string) (uid, gid int, normalized string, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, "", nil
	}
	uidStr, gidStr := s, s
	if i := strings.IndexByte(s, ':'); i >= 0 {
		uidStr, gidStr = s[:i], s[i+1:]
	}
	uid, err = parseIDField(uidStr, "UID")
	if err != nil {
		return 0, 0, "", err
	}
	gid, err = parseIDField(gidStr, "GID")
	if err != nil {
		return 0, 0, "", err
	}
	return uid, gid, strconv.Itoa(uid) + ":" + strconv.Itoa(gid), nil
}

func parseIDField(s, field string) (int, error) {
	n, convErr := strconv.Atoi(strings.TrimSpace(s))
	if convErr != nil {
		return 0, verr("flash.err.vol_owner_number", field)
	}
	if n < 0 || n > 65535 {
		return 0, verr("flash.err.vol_owner_range", field)
	}
	return n, nil
}
