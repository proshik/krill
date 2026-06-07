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

// catalogs: locale -> key -> text. Adding a language = one more map.
var catalogs = map[string]map[string]string{
	"en": en,
	"ru": ru,
}

// Locale is a selectable language for the Settings picker.
type Locale struct{ Code, Name string }

// Locales lists the available languages (order = display order).
var Locales = []Locale{
	{Code: "en", Name: "English"},
	{Code: "ru", Name: "Русский"},
}

// Supported reports whether loc has a catalog.
func Supported(loc string) bool { _, ok := catalogs[loc]; return ok }

// Current returns the locale active for ctx (for marking the picker selection).
func Current(ctx context.Context) string { return localeOf(ctx) }

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
