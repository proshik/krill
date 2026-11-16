package traefik

import (
	"regexp"
	"strings"
	"testing"
)

func redirectDomains(primaryTLS, redirectTLS bool) []Domain {
	return []Domain{
		{Host: "app.example.com", TLS: primaryTLS, Primary: true},
		{
			Host: "old.example.org", TLS: redirectTLS, RedirectToPrimary: true,
			Exposed: true, Paths: []string{"/api"},
			BasicAuthUsers: []string{"u:$2a$10$x"}, AllowedIPs: []string{"10.0.0.0/8"},
		},
	}
}

func TestAppLabelsRedirectToPrimaryTLS(t *testing.T) {
	l := AppLabels("krill-5", redirectDomains(true, true), 8080, "krill-net", []string{"/metrics"})
	mw := "traefik.http.middlewares.krill-5-d1-toprimary.redirectregex."
	if got := l[mw+"replacement"]; got != "https://app.example.com${1}" {
		t.Fatalf("replacement = %q", got)
	}
	if l[mw+"permanent"] != "true" || l[mw+"regex"] != redirectRegex {
		t.Fatalf("middleware = %q / %q", l[mw+"permanent"], l[mw+"regex"])
	}
	for _, r := range []string{"krill-5-d1", "krill-5-d1s"} {
		p := "traefik.http.routers." + r + "."
		if got := l[p+"rule"]; got != "Host(`old.example.org`)" {
			t.Errorf("%s rule = %q (paths and hidden metrics paths must not apply)", r, got)
		}
		if got := l[p+"middlewares"]; got != "krill-5-d1-toprimary" {
			t.Errorf("%s middlewares = %q (no auth/allowlist, no https hop)", r, got)
		}
		if l[p+"priority"] != "100" || l[p+"service"] != "krill-5" {
			t.Errorf("%s priority/service = %q/%q", r, l[p+"priority"], l[p+"service"])
		}
	}
	if l["traefik.http.routers.krill-5-d1.entrypoints"] != "web" ||
		l["traefik.http.routers.krill-5-d1s.entrypoints"] != "websecure" ||
		l["traefik.http.routers.krill-5-d1s.tls.certresolver"] != "le" {
		t.Fatal("TLS redirect domain must keep its own certificate on websecure and redirect on web")
	}
	for k := range l {
		if strings.Contains(k, "krill-5-d1-auth") || strings.Contains(k, "krill-5-d1-ipallow") || strings.Contains(k, "krill-5-d1-redirect.") {
			t.Errorf("unexpected label %s on a redirect domain", k)
		}
	}
	// The unexposed primary is not routed, but the app is still enabled.
	if l["traefik.enable"] != "true" {
		t.Fatal("a redirect domain alone must enable the service")
	}
	if _, ok := l["traefik.http.routers.krill-5-d0s.rule"]; ok {
		t.Fatal("unexposed primary must not be routed")
	}
}

func TestAppLabelsRedirectToPlainPrimary(t *testing.T) {
	l := AppLabels("krill-5", redirectDomains(false, false), 8080, "krill-net", nil)
	if got := l["traefik.http.middlewares.krill-5-d1-toprimary.redirectregex.replacement"]; got != "http://app.example.com${1}" {
		t.Fatalf("replacement = %q", got)
	}
	if _, ok := l["traefik.http.routers.krill-5-d1s.rule"]; ok {
		t.Fatal("non-TLS redirect domain must not get a websecure router")
	}
	if l["traefik.http.routers.krill-5-d1.entrypoints"] != "web" {
		t.Fatal("redirect router must be on web")
	}
}

func TestAppLabelsRedirectFollowsPrimary(t *testing.T) {
	ds := []Domain{
		{Host: "a.example.com", TLS: true},
		{Host: "b.example.com", Primary: true},
		{Host: "old.example.org", RedirectToPrimary: true},
	}
	l := AppLabels("krill-5", ds, 80, "krill-net", nil)
	if got := l["traefik.http.middlewares.krill-5-d2-toprimary.redirectregex.replacement"]; got != "http://b.example.com${1}" {
		t.Fatalf("replacement = %q", got)
	}
	ds[0].Primary, ds[1].Primary = true, false
	l = AppLabels("krill-5", ds, 80, "krill-net", nil)
	if got := l["traefik.http.middlewares.krill-5-d2-toprimary.redirectregex.replacement"]; got != "https://a.example.com${1}" {
		t.Fatalf("after primary change replacement = %q", got)
	}
}

func TestAppLabelsRedirectWithoutPrimaryIsNotRouted(t *testing.T) {
	ds := []Domain{{Host: "old.example.org", RedirectToPrimary: true, Exposed: true}}
	l := AppLabels("krill-5", ds, 80, "krill-net", nil)
	if l["traefik.enable"] != "false" {
		t.Fatalf("labels = %v", l)
	}
	// A primary flagged as redirecting (forbidden by the schema) serves normally.
	ds = []Domain{{Host: "a.example.com", Primary: true, RedirectToPrimary: true, Exposed: true}}
	l = AppLabels("krill-5", ds, 80, "krill-net", nil)
	if l["traefik.http.routers.krill-5-d0.rule"] != "Host(`a.example.com`)" {
		t.Fatalf("primary rule = %q", l["traefik.http.routers.krill-5-d0.rule"])
	}
	for k := range l {
		if strings.Contains(k, "toprimary") {
			t.Fatalf("unexpected %s", k)
		}
	}
}

// The regex/replacement pair as Traefik applies it (regexp.ReplaceAllString on
// scheme://host[:port]+RequestURI) keeps path and query.
func TestRedirectRegexKeepsPathAndQuery(t *testing.T) {
	re := regexp.MustCompile(redirectRegex)
	for in, want := range map[string]string{
		"http://old.example.org/a/b?x=1&y=%2F": "https://app.example.com/a/b?x=1&y=%2F",
		"https://old.example.org:443/":         "https://app.example.com/",
		"http://old.example.org/metrics":       "https://app.example.com/metrics",
	} {
		if got := re.ReplaceAllString(in, "https://app.example.com"+redirectURIToken); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
}
