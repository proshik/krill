package nav

import "context"

type updateKey struct{}

// WithUpdateAvailable returns a copy of ctx carrying the release tag the
// sidebar's Settings badge announces. It is sidebar state only: the server
// sets it for instance operators when a newer Krill release is known.
func WithUpdateAvailable(ctx context.Context, tag string) context.Context {
	return context.WithValue(ctx, updateKey{}, tag)
}

// UpdateAvailable returns the release tag for the Settings badge, or "" when
// the sidebar shows no badge.
func UpdateAvailable(ctx context.Context) string {
	tag, _ := ctx.Value(updateKey{}).(string)
	return tag
}
