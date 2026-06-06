// Package nav carries the current request path through the request context so
// the layout can highlight the active sidebar section.
package nav

import (
	"context"
	"strings"
)

type ctxKey struct{}

// WithPath returns a copy of ctx carrying the current request path.
func WithPath(ctx context.Context, path string) context.Context {
	return context.WithValue(ctx, ctxKey{}, path)
}

// Path returns the request path stored in ctx, or "".
func Path(ctx context.Context) string {
	p, _ := ctx.Value(ctxKey{}).(string)
	return p
}

// IsActive reports whether the current path belongs to the given sidebar
// section ("projects" | "members" | "destinations" | "registries").
func IsActive(ctx context.Context, section string) bool {
	return sectionOf(Path(ctx)) == section
}

// IsSettings reports whether the current path is one of the Settings pages
// (Destinations or Registries), which share a single sidebar entry.
func IsSettings(ctx context.Context) bool {
	s := sectionOf(Path(ctx))
	return s == "destinations" || s == "registries"
}

// sectionOf maps a request path to its sidebar section. The org-level pages
// (members/destinations/registries) are leaf routes, so a substring match is
// unambiguous; everything else under an org is the projects/dashboard section.
func sectionOf(path string) string {
	switch {
	case strings.Contains(path, "/members"):
		return "members"
	case strings.Contains(path, "/destinations"):
		return "destinations"
	case strings.Contains(path, "/registries"):
		return "registries"
	default:
		return "projects"
	}
}
