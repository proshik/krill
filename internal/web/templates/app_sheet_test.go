package templates

import (
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
)

// The sheet links a domain Traefik actually routes: an internal primary is
// skipped for an exposed secondary, with the secondary's own scheme.
func TestPrimaryPublicURL(t *testing.T) {
	internalPrimary := db.Domain{Host: "api.internal.example.com", IsPrimary: true}
	secondary := db.Domain{Host: "api.example.com", Exposed: true, Tls: true, Paths: "/v1"}
	redirect := db.Domain{Host: "old.example.com", Exposed: true, RedirectToPrimary: true}
	for name, c := range map[string]struct {
		domains []db.Domain
		exposed bool
		want    string
	}{
		"internal primary, exposed https secondary": {[]db.Domain{internalPrimary, redirect, secondary}, true, "https://api.example.com/v1"},
		"exposed primary wins":                     {[]db.Domain{secondary, {Host: "p.example.com", IsPrimary: true, Exposed: true}}, true, "http://p.example.com"},
		"only a redirect is exposed":               {[]db.Domain{internalPrimary, redirect}, true, ""},
		"not exposed":                              {[]db.Domain{secondary}, false, ""},
	} {
		if got := primaryPublicURL(c.domains, c.exposed); got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
}
