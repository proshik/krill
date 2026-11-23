package templates

import db "github.com/proshik/krill/internal/database/gen"

// cloneDefaultName suggests a name for a clone of the source environment.
func cloneDefaultName(source db.Environment) string {
	if source.Slug == "production" {
		return "staging"
	}
	return source.Name + "-copy"
}
