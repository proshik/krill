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

// "CPU из 1 ядер" on the monitoring page of a 1-CPU node: counts with a noun
// go through Tn, including the ones whose count is not the first value shown.
func TestCountTextsAgree(t *testing.T) {
	en, ru := WithLocale(context.Background(), "en"), WithLocale(context.Background(), "ru")
	cases := []struct{ got, want string }{
		{Tn(ru, "mon.cpu_of_cores", 1), "CPU из 1 ядра"},
		{Tn(ru, "mon.cpu_of_cores", 2), "CPU из 2 ядер"},
		{Tn(ru, "mon.cpu_of_cores", 21), "CPU из 21 ядра"},
		{Tn(en, "mon.cpu_of_cores", 1), "CPU of 1 core"},
		{Tn(en, "mon.cpu_of_cores", 4), "CPU of 4 cores"},
		{Tn(ru, "nodes.cpu_of", 1, 37), "37% из 1 ядра"},
		{Tn(en, "nodes.cpu_of", 8, 5), "5% of 8 cores"},
		{Tn(ru, "obs.coverage.partial", 1, 0, "cp"), "развёрнуто на 0 из 1 ноды; на прежних настройках остаются: cp."},
		{Tn(en, "obs.coverage.partial", 3, 2, "w1"), "deployed on 2 of 3 nodes; still on the previous settings: w1."},
		{Tn(en, "projects.n_failed", 1), "1 failed deploy"},
		{Tn(en, "projects.n_failed", 2), "2 failed deploys"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}
