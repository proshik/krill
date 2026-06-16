package i18n

import "testing"

func TestMatchAcceptLanguage(t *testing.T) {
	cases := map[string]string{
		"":                          "",   // no header
		"fr-FR,fr;q=0.9":            "",   // unsupported only
		"ru":                        "ru", // exact supported
		"ru-RU,ru;q=0.9":            "ru", // primary subtag match
		"en-US,en;q=0.9":            "en", // english region
		"en-US,en;q=0.9,ru;q=0.8":   "en", // highest q wins (en)
		"ru;q=0.8,en;q=0.9":         "en", // q-weight beats header order
		"fr;q=1.0,ru;q=0.5":         "ru", // skip unsupported, take next supported
		"*":                         "",   // wildcard ignored
		"de, ru-RU;q=0.7, en;q=0.3": "ru", // first supported by q
	}
	for header, want := range cases {
		if got := MatchAcceptLanguage(header); got != want {
			t.Errorf("MatchAcceptLanguage(%q) = %q, want %q", header, got, want)
		}
	}
}
