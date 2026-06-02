package dbservice

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// GenerateAppName builds a unique DB service name: krill-<engine>-<slug>-<rand6>.
// This name = Swarm service name = DNS host = volume prefix <appName>-data.
func GenerateAppName(engine, name string) string {
	slug := slugify(name)
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	suffix := hex.EncodeToString(b) // 6 hex characters
	if slug == "" {
		return "krill-" + engine + "-" + suffix
	}
	return "krill-" + engine + "-" + slug + "-" + suffix
}

func slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == ' ' || r == '-' || r == '_':
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.TrimRight(b.String(), "-")
}
