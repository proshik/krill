package templates

import (
	"context"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/web/i18n"
)

// The head preloads the Latin faces everywhere and the Cyrillic one only for
// the Russian UI, where the first paint needs it.
func TestDocHeadPreloadsFonts(t *testing.T) {
	for _, c := range []struct {
		locale   string
		cyrillic bool
	}{{"en", false}, {"ru", true}} {
		var b strings.Builder
		if err := docHead("x").Render(i18n.WithLocale(context.Background(), c.locale), &b); err != nil {
			t.Fatal(err)
		}
		html := b.String()
		for _, f := range []string{"onest-latin.woff2", "outfit-latin.woff2"} {
			if !strings.Contains(html, `<link rel="preload" href="/static/fonts/`+f+`" as="font" type="font/woff2" crossorigin>`) {
				t.Errorf("%s: no preload for %s", c.locale, f)
			}
		}
		if got := strings.Contains(html, "onest-cyrillic.woff2"); got != c.cyrillic {
			t.Errorf("%s: Cyrillic preload = %v, want %v", c.locale, got, c.cyrillic)
		}
	}
}
