package i18n

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
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
	{Code: "en", Name: "english"},
	{Code: "ru", Name: "русский"},
}

// Supported reports whether loc has a catalog.
func Supported(loc string) bool { _, ok := catalogs[loc]; return ok }

// MatchAcceptLanguage returns the best supported locale for an Accept-Language
// header value, or "" if none match. It honors q-weights (highest first, header
// order breaking ties) and matches on the primary subtag (e.g. "ru-RU" -> "ru").
func MatchAcceptLanguage(header string) string {
	type pref struct {
		loc string
		q   float64
	}
	var prefs []pref
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		loc, q := part, 1.0
		if i := strings.IndexByte(part, ';'); i >= 0 {
			loc = strings.TrimSpace(part[:i])
			for _, p := range strings.Split(part[i+1:], ";") {
				if p = strings.TrimSpace(p); strings.HasPrefix(p, "q=") {
					if v, err := strconv.ParseFloat(strings.TrimPrefix(p, "q="), 64); err == nil {
						q = v
					}
				}
			}
		}
		base := strings.ToLower(loc)
		if i := strings.IndexByte(base, '-'); i >= 0 {
			base = base[:i]
		}
		if base == "" || base == "*" {
			continue
		}
		prefs = append(prefs, pref{base, q})
	}
	sort.SliceStable(prefs, func(i, j int) bool { return prefs[i].q > prefs[j].q })
	for _, p := range prefs {
		if Supported(p.loc) {
			return p.loc
		}
	}
	return ""
}

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

// Tn formats a count with its noun in the right plural form: the catalogs
// hold key.one, key.few and key.many (English uses one and many; Russian all
// three), each a format whose first verb is the count. Extra args follow it.
func Tn(ctx context.Context, key string, n int, args ...any) string {
	return Tf(ctx, key+"."+PluralForm(localeOf(ctx), n), append([]any{n}, args...)...)
}

// PluralForm is the plural category of n in loc: "one", "few" or "many".
func PluralForm(loc string, n int) string {
	if n < 0 {
		n = -n
	}
	if loc != "ru" {
		if n == 1 {
			return "one"
		}
		return "many"
	}
	switch m10, m100 := n%10, n%100; {
	case m10 == 1 && m100 != 11:
		return "one"
	case m10 >= 2 && m10 <= 4 && (m100 < 12 || m100 > 14):
		return "few"
	}
	return "many"
}

// Sentence starts s with a capital letter. Catalog values are lowercase, as
// the UI shows them; a value that becomes a new sentence after a period (a
// flash joined from two messages, a browser tab title) goes through this.
func Sentence(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 || r == utf8.RuneError {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}
