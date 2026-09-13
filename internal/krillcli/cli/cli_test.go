package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/krillcli/client"
	"github.com/proshik/krill/internal/krillcli/deployflow"
	"gopkg.in/yaml.v3"
)

// TestYamlScalarSurvivesFreeTextNames: project and environment names are free
// text in Krill, so `app: Bot: prod/production/bot` is a file no later command
// can read at all — not a mistake the user can see, just a CLI that stops
// working the moment init writes it.
func TestYamlScalarSurvivesFreeTextNames(t *testing.T) {
	for _, path := range []string{
		"acme/production/bot",
		"Bot: prod/production/bot",
		`say "hi"/production/bot`,
		"back\\slash/production/bot",
		"# hash/production/bot",
		"{brace}/production/bot",
		"tab\there/production/bot",
		"17",
	} {
		doc := "app: " + yamlScalar(path) + "\n"
		var out struct {
			App string `yaml:"app"`
		}
		if err := yaml.Unmarshal([]byte(doc), &out); err != nil {
			t.Fatalf("%q rendered unreadable YAML %q: %v", path, doc, err)
		}
		if out.App != path {
			t.Fatalf("round trip of %q gave %q (document %q)", path, out.App, doc)
		}
	}
}

// TestClassifyGivesEveryErrorTheDocumentedCode: the exit-code contract is
// printed in `deploy --help`, in the krill-cli guide and in the agent skill, but it
// used to hold only inside the deploy flow — every other command answered a
// revoked token or a mistyped id with 1, which the same contract defines as
// "the deployment failed".
func TestClassifyGivesEveryErrorTheDocumentedCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, deployflow.ExitOK},
		{"a Failure keeps its own code", &deployflow.Failure{Code: deployflow.ExitNotRunning, Err: errors.New("x")}, deployflow.ExitNotRunning},
		{"401", &client.APIError{HTTPStatus: 401}, deployflow.ExitAuth},
		{"403", &client.APIError{HTTPStatus: 403}, deployflow.ExitAuth},
		{"404", &client.APIError{HTTPStatus: 404}, deployflow.ExitUsage},
		{"409", &client.APIError{HTTPStatus: 409}, deployflow.ExitBusy},
		{"400", &client.APIError{HTTPStatus: 400}, deployflow.ExitUsage},
		{"429", &client.APIError{HTTPStatus: 429}, deployflow.ExitUsage},
		{"500 is the server's problem, not the invocation's", &client.APIError{HTTPStatus: 500}, deployflow.ExitFailed},
		{"anything else", errors.New("boom"), deployflow.ExitFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := deployflow.CodeOf(classify(tc.err)); got != tc.want {
				t.Fatalf("code = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestRouterNotFoundIsExplained: a 404 from the router means the request never
// reached Krill's API — wrong base URL, or KRILL_AGENT_API_ENABLED=false — but
// it reads as a rejected token, and people regenerate tokens over it.
func TestRouterNotFoundIsExplained(t *testing.T) {
	err := classifyHint(&client.APIError{HTTPStatus: 404, Code: "http_404", Message: "404 page not found"})
	if !strings.Contains(err.Error(), "KRILL_AGENT_API_ENABLED") {
		t.Fatalf("the router 404 should be explained, got %v", err)
	}

	// A 404 from a handler is a real not-found and must not collect the hint.
	plain := classifyHint(&client.APIError{HTTPStatus: 404, Code: "not_found", Message: `no application "acme/production/typo"`})
	if strings.Contains(plain.Error(), "KRILL_AGENT_API_ENABLED") {
		t.Fatalf("a genuine not-found was mislabelled: %v", plain)
	}
}
