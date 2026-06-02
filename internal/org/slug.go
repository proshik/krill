package org

import "strings"

// Slugify converts a human-readable name into a slug: lowercase, [a-z0-9-],
// sequences of separators collapse into a single dash, edges are trimmed.
// Returns "" if there are no valid characters (the caller must check for this).
func Slugify(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == ' ' || r == '-' || r == '_' || r == '.':
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		default:
			// non-ascii and special characters are ignored
		}
	}
	return strings.TrimRight(b.String(), "-")
}
