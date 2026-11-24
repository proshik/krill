package i18n

import (
	"context"
	"testing"
)

// TestCatalogsInSync guarantees en and ru have exactly the same key set, so a
// new UI string can never be added to one catalog and forgotten in the other.
func TestCatalogsInSync(t *testing.T) {
	for k := range en {
		if _, ok := ru[k]; !ok {
			t.Errorf("key %q present in en, missing in ru", k)
		}
	}
	for k := range ru {
		if _, ok := en[k]; !ok {
			t.Errorf("key %q present in ru, missing in en", k)
		}
	}
}

func TestSentence(t *testing.T) {
	for in, want := range map[string]string{"": "", "logs: none": "Logs: none", "узлы": "Узлы", "S3 server": "S3 server", "%s saved": "%s saved"} {
		if got := Sentence(in); got != want {
			t.Errorf("Sentence(%q) = %q, want %q", in, got, want)
		}
	}
}

// "1 members" and "1 tokens" in English: counts go through Tn and pick the
// plural form of their locale.
func TestPluralForm(t *testing.T) {
	cases := []struct {
		loc  string
		n    int
		want string
	}{
		{"en", 0, "many"}, {"en", 1, "one"}, {"en", 2, "many"}, {"en", 21, "many"},
		{"ru", 0, "many"}, {"ru", 1, "one"}, {"ru", 2, "few"}, {"ru", 4, "few"}, {"ru", 5, "many"},
		{"ru", 11, "many"}, {"ru", 12, "many"}, {"ru", 21, "one"}, {"ru", 22, "few"}, {"ru", 111, "many"},
	}
	for _, c := range cases {
		if got := PluralForm(c.loc, c.n); got != c.want {
			t.Errorf("PluralForm(%q, %d) = %q, want %q", c.loc, c.n, got, c.want)
		}
	}
	ctx := WithLocale(context.Background(), "en")
	if got := Tn(ctx, "members.n", 1); got != "1 member" {
		t.Errorf("Tn(en, members.n, 1) = %q", got)
	}
	if got := Tn(WithLocale(context.Background(), "ru"), "members.n", 3); got != "3 участника" {
		t.Errorf("Tn(ru, members.n, 3) = %q", got)
	}
}
