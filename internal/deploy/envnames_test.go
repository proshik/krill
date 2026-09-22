package deploy

import "testing"

func TestEnvNameInUse(t *testing.T) {
	env := "# comment\nFOO=1\nMETRICS_TOKEN=x\n"
	if !EnvNameInUse(env, nil, "METRICS_TOKEN") {
		t.Error("env_text key must count")
	}
	if !EnvNameInUse("", []string{"DATABASE_URL"}, "DATABASE_URL") {
		t.Error("db-link var must count")
	}
	if EnvNameInUse(env, []string{"DATABASE_URL"}, "KRILL_METRICS_TOKEN") {
		t.Error("unrelated name must not count")
	}
	if EnvNameInUse("#METRICS_TOKEN=x", nil, "METRICS_TOKEN") {
		t.Error("a commented-out line is not a variable")
	}
}
