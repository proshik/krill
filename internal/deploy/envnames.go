package deploy

import "github.com/proshik/krill/internal/envtext"

// EnvNameInUse reports whether name is already a variable the app receives
// from one of its own sources: a line of env_text or a database link's
// variable. It knows the sources, not the policy — the metrics token refuses
// such a name, while a later stage (OTEL_*) will defer to it instead.
func EnvNameInUse(envText string, linkVars []string, name string) bool {
	for _, k := range envtext.Keys(envText) {
		if k == name {
			return true
		}
	}
	for _, v := range linkVars {
		if v == name {
			return true
		}
	}
	return false
}
