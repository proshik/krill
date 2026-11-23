package templates

import (
	"context"
	"strings"
	"testing"
	"time"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/web/i18n"
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
		"exposed primary wins":                      {[]db.Domain{secondary, {Host: "p.example.com", IsPrimary: true, Exposed: true}}, true, "http://p.example.com"},
		"only a redirect is exposed":                {[]db.Domain{internalPrimary, redirect}, true, ""},
		"not exposed":                               {[]db.Domain{secondary}, false, ""},
	} {
		if got := primaryPublicURL(c.domains, c.exposed); got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
}

// "1 ч назад, по вручную": the trigger is a whole phrase in the catalogs,
// not a word glued after a preposition.
func TestDeployWhenPhrases(t *testing.T) {
	ru := i18n.WithLocale(context.Background(), "ru")
	en := i18n.WithLocale(context.Background(), "en")
	when := time.Now().Add(-90 * time.Minute)
	for _, tc := range []struct {
		ctx     context.Context
		trigger string
		suffix  string
	}{
		{ru, "manual", ", вручную"}, {ru, "webhook", ", по вебхуку"}, {ru, "api", ", через API"}, {ru, "system", ", сам Krill"},
		{en, "manual", " by hand"}, {en, "schedule", " on schedule"},
	} {
		d := db.ListDeploymentSummariesByApplicationRow{Trigger: tc.trigger, StartedAt: when}
		if got := deployWhen(tc.ctx, d); !strings.HasSuffix(got, tc.suffix) || strings.Contains(got, "по вручную") {
			t.Errorf("deployWhen(%s) = %q, want suffix %q", tc.trigger, got, tc.suffix)
		}
	}
}
