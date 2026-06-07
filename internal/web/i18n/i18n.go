package i18n

import (
	"context"
	"fmt"
)

type ctxKey int

const localeKey ctxKey = 0

// DefaultLocale is the default language.
const DefaultLocale = "en"

// WithLocale puts the locale into the context (for future detection via cookie/Accept-Language).
func WithLocale(ctx context.Context, locale string) context.Context {
	return context.WithValue(ctx, localeKey, locale)
}

func localeOf(ctx context.Context) string {
	if v, ok := ctx.Value(localeKey).(string); ok && v != "" {
		return v
	}
	return DefaultLocale
}

// catalogs: locale -> key -> text. Only en for now; adding a language = one more map.
var catalogs = map[string]map[string]string{
	"en": en,
}

// T returns the translation for a key in the locale from ctx; fallback is the key as-is.
func T(ctx context.Context, key string) string {
	loc := localeOf(ctx)
	if m, ok := catalogs[loc]; ok {
		if s, ok := m[key]; ok {
			return s
		}
	}
	if s, ok := catalogs[DefaultLocale][key]; ok {
		return s
	}
	return key
}

// Tf is T with printf-style formatting — for messages with a dynamic part
// (e.g. a name) so the whole string stays in the catalog: T value uses %s.
func Tf(ctx context.Context, key string, args ...any) string {
	return fmt.Sprintf(T(ctx, key), args...)
}
