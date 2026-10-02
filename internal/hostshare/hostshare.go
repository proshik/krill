// Package hostshare holds the rules for several applications sharing one host:
// only apps of the same environment, at most one catch-all (a domain row with
// no paths), and no two rows whose paths overlap by segment. Store applies them
// under a per-host advisory lock.
package hostshare

import (
	"fmt"
	"strings"
)

// i18n keys of the conflicts, rendered by the server through i18n.Tf.
const (
	KeyHostTaken     = "flash.err.host_in_use"
	KeyHostOnThisApp = "flash.err.host_on_this_app"
	KeyCatchAllTaken = "flash.err.host_catchall_taken"
	KeyPathOverlap   = "flash.err.host_path_overlap"
)

// ConflictError is a rule violation; Key/Args render the user-facing message.
type ConflictError struct {
	Key  string
	Args []any
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("host sharing conflict: %s %v", e.Key, e.Args)
}

// Claim is one domains row as the rules see it. Paths are normalized
// (traefik.NormalizePaths); empty means the whole host (catch-all).
type Claim struct {
	DomainID int64 // 0 for a row being added
	AppID    int64
	AppName  string
	EnvID    int64
	Paths    []string
}

// Overlaps reports whether two normalized path prefixes can match the same
// request under segment semantics: /api overlaps /api/v2, not /apiary.
func Overlaps(a, b string) bool {
	return a == b || strings.HasPrefix(b, a+"/") || strings.HasPrefix(a, b+"/")
}

// Check validates cand against every other row already on the host. A row from
// another environment is reported without details: it may belong to another
// organization. Rows are compared regardless of exposure — an internal row
// still claims its paths.
func Check(existing []Claim, cand Claim) error {
	others := make([]Claim, 0, len(existing))
	for _, e := range existing {
		if cand.DomainID != 0 && e.DomainID == cand.DomainID {
			continue
		}
		if e.EnvID != cand.EnvID {
			return &ConflictError{Key: KeyHostTaken}
		}
		others = append(others, e)
	}
	for _, e := range others {
		if e.AppID == cand.AppID {
			return &ConflictError{Key: KeyHostOnThisApp}
		}
		if len(cand.Paths) == 0 && len(e.Paths) == 0 {
			return &ConflictError{Key: KeyCatchAllTaken, Args: []any{e.AppName}}
		}
		for _, cp := range cand.Paths {
			for _, ep := range e.Paths {
				if Overlaps(cp, ep) {
					return &ConflictError{Key: KeyPathOverlap, Args: []any{cp, ep, e.AppName}}
				}
			}
		}
	}
	return nil
}
