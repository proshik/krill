package templates

import (
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/proshik/krill/internal/database/gen"
)

// itoa formats an int64 for interpolation into URLs inside templates.
func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// destName returns the Name of the destination with the given id, or "" if none.
func destName(dests []db.Destination, id int64) string {
	for _, d := range dests {
		if d.ID == id {
			return d.Name
		}
	}
	return ""
}

// tsStr formats a timestamptz as "2006-01-02 15:04", or "—" if not set.
func tsStr(t pgtype.Timestamptz) string {
	if !t.Valid {
		return "—"
	}
	return t.Time.Format("2006-01-02 15:04")
}

// humanSize renders a byte count as a short human-readable string.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + string("KMGTPE"[exp]) + "iB"
}

// envText serializes the env map into KEY=VALUE text, one per line (deterministically).
func envText(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(env[k])
		b.WriteByte('\n')
	}
	return b.String()
}
