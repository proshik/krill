package client_test

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/api"
	"github.com/proshik/krill/internal/krillcli/client"
)

// TestResponseTypesMatchTheServer is the safety net for the decision not to
// import internal/api's types directly.
//
// The CLI keeps its own copies so a released binary's wire contract is not
// hostage to a server-side refactor, and so it can hold shapes the server
// type does not (an HTTP status on errors, tolerance for unknown fields).
// The cost of a copy is silent drift: the server renames a field, the CLI
// keeps compiling, and a value quietly decodes as zero. This test converts
// that into a build failure at the moment the server changes.
//
// Note the direction of the import — it appears in a _test file only, so the
// dependency graph of the shipped binary still has no edge from the CLI to
// the server, and `go list -deps` (which ignores test imports) can be used in
// CI to prove it.
func TestResponseTypesMatchTheServer(t *testing.T) {
	pairs := []struct {
		name             string
		cliType, apiType any
	}{
		{"Whoami", client.Whoami{}, api.WhoamiResult{}},
		{"App", client.App{}, api.AppSummary{}},
		{"AppStatus", client.AppStatus{}, api.AppStatus{}},
		{"EnvKey", client.EnvKey{}, api.EnvKey{}},
		{"LogLine", client.LogLine{}, api.LogLine{}},
		{"Deployment", client.Deployment{}, api.DeploymentSummary{}},
		{"DeploymentDetail", client.DeploymentDetail{}, api.DeploymentDetail{}},
		{"Accepted", client.Accepted{}, api.DeployAccepted{}},
	}
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			cli := jsonTags(reflect.TypeOf(p.cliType))
			srv := jsonTags(reflect.TypeOf(p.apiType))
			if !reflect.DeepEqual(cli, srv) {
				t.Fatalf("JSON shape drifted for %s:\n  cli: %v\n  api: %v\n"+
					"update internal/krillcli/client to match, or the CLI will decode a field as its zero value",
					p.name, cli, srv)
			}
		})
	}
}

// jsonTags returns the flattened set of JSON field names a type serializes,
// following anonymous embedded structs the way encoding/json does — both
// sides use embedding, and a nested-vs-flat mismatch is exactly the kind of
// drift worth catching.
func jsonTags(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			out = append(out, jsonTags(f.Type)...)
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
