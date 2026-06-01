package i18n

import "context"

type ctxKey int

const localeKey ctxKey = 0

// DefaultLocale — язык по умолчанию.
const DefaultLocale = "en"

// WithLocale кладёт локаль в контекст (для будущего детекта по cookie/Accept-Language).
func WithLocale(ctx context.Context, locale string) context.Context {
	return context.WithValue(ctx, localeKey, locale)
}

func localeOf(ctx context.Context) string {
	if v, ok := ctx.Value(localeKey).(string); ok && v != "" {
		return v
	}
	return DefaultLocale
}

// catalogs: locale -> key -> text. Сейчас только en; добавление языка = ещё один map.
var catalogs = map[string]map[string]string{
	"en": en,
}

// T возвращает перевод по ключу для локали из ctx; фолбэк — ключ как есть.
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
