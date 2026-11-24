//go:build acceptance

package acceptance

import (
	"context"
	"strings"

	"github.com/proshik/krill/internal/web/i18n"
)

// The texts the harness looks for on Krill's pages, taken from the English
// catalog the release under test was built from. The client signs in with no
// krill_lang cookie and no Accept-Language header, so every page it reads is
// in the default locale. Written as literals, these drifted from the UI when
// its text became lowercase (v0.13.0) and the checks failed — or, for the
// "must not say" ones, silently stopped being able to fail.

var enCtx = i18n.WithLocale(context.Background(), "en")

// ui is the English catalog text for key, formatted with args when given. A
// key missing from the catalog panics: i18n.T would return the key itself,
// and a check for it would never match (or, as a "must not say", never fail).
func ui(key string, args ...any) string {
	s := i18n.T(enCtx, key)
	if s == key {
		panic("acceptance: no catalog text for " + key)
	}
	if len(args) == 0 {
		return s
	}
	return i18n.Tf(enCtx, key, args...)
}

// uiBefore is the part of a catalog text before its first sep: the stable
// head of a message whose tail is a detail (an error, a version in brackets).
func uiBefore(key, sep string, args ...any) string {
	head, _, _ := strings.Cut(ui(key, args...), sep)
	return head
}

// runningVersion is the Updates page's "running version" row as page text:
// the label and the version are adjacent elements, one space apart.
func runningVersion(v string) string { return ui("update.current") + " " + v }

// busyRefused is the flash of an install refused because work is running.
func busyRefused(reason string) string {
	return ui("flash.err.update_busy", ui("update.busy."+reason))
}

// busyFailed is the job's failure when work started while it ran.
func busyFailed(reason string) string {
	return ui("update.job_failed", ui("update.busy."+reason))
}

// phase is a job phase's label in the status fragment.
func phase(name string) string { return ui("update.phase." + name) }

// rollbackOffer is the rollback button for version v; rollbackAny is its
// version-independent head, for "no rollback is offered" checks.
func rollbackOffer(v string) string { return ui("update.rollback_to", v) }

var rollbackAny = uiBefore("update.rollback_to", "%")
