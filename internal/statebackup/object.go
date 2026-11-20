package statebackup

import (
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/proshik/krill/internal/backup"
)

// PreUpdateKeep is how many pre-update dumps retention keeps.
const PreUpdateKeep = 5

const stampLayout = "2006-01-02T15-04-05Z"

var (
	scheduledRe = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d-\d\d-\d\dZ\.sql\.gz\.age$`)
	preUpdateRe = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d-\d\d-\d\dZ_v[0-9A-Za-z.\-]+_to_v[0-9A-Za-z.\-]+\.sql\.gz\.age$`)
	slashRuns   = regexp.MustCompile(`/+`)
)

// ScheduledDir is the directory holding scheduled dumps, with a trailing slash.
func ScheduledDir(prefix string) string {
	p := strings.Trim(slashRuns.ReplaceAllString(prefix, "/"), "/")
	if p == "" {
		return "krill-state/"
	}
	return p + "/krill-state/"
}

// PreUpdateDir is the directory holding pre-update dumps.
func PreUpdateDir(prefix string) string { return ScheduledDir(prefix) + "pre-update/" }

// ScheduledKey names a scheduled dump taken at t.
func ScheduledKey(prefix string, t time.Time) string {
	return ScheduledDir(prefix) + t.UTC().Format(stampLayout) + ".sql.gz.age"
}

// PreUpdateKey names a dump taken before updating from one release to another.
func PreUpdateKey(prefix string, t time.Time, from, to string) string {
	return PreUpdateDir(prefix) + t.UTC().Format(stampLayout) + "_" + from + "_to_" + to + ".sql.gz.age"
}

// Prunable returns the objects retention may delete: only keys directly in dir
// whose remainder matches that directory's exact pattern, newest first by key
// name, everything after the first keep (at least one is always kept).
func Prunable(objs []backup.Object, dir string, preUpdate bool, keep int) []backup.Object {
	if keep < 1 {
		keep = 1
	}
	re := scheduledRe
	if preUpdate {
		re = preUpdateRe
	}
	var match []backup.Object
	for _, o := range objs {
		if rest, ok := strings.CutPrefix(o.Key, dir); ok && re.MatchString(rest) {
			match = append(match, o)
		}
	}
	sort.Slice(match, func(i, j int) bool { return match[i].Key > match[j].Key })
	if len(match) <= keep {
		return nil
	}
	return match[keep:]
}
