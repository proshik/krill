package observability

import (
	"strings"
	"testing"
)

func sampleTargets() []AppTarget {
	return []AppTarget{
		{OrgID: 1, AppID: 7, EndpointID: 3, Org: "Acme", Project: "bots", Env: "prod", App: "base13",
			Token: "t0k", Port: 27015, Path: "/metrics", Job: "relay"},
		{OrgID: 2, AppID: 9, EndpointID: 4, Org: "Org $1", Project: "p", Env: "e", App: "gitea",
			Token: "t1k", Port: 3000, Path: "/metrics", Job: ""},
	}
}

func TestRenderAppsModuleGolden(t *testing.T) {
	out, err := RenderAppsModule(sampleTargets(), map[int64][]TaskNode{
		7: {{IP: "10.0.1.5", Node: "krill-cp-msk"}},
		9: {{IP: "10.0.2.7", Node: "db узел"}, {IP: "10.0.2.8", Node: "worker.1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "apps_module.golden.alloy", out)
}

func TestRenderAppsModuleEmpty(t *testing.T) {
	out, err := RenderAppsModule(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "apps_module_empty.golden.alloy", out)
}

func TestRenderAppsModuleEscapes(t *testing.T) {
	out, _ := RenderAppsModule(sampleTargets(), map[int64][]TaskNode{9: {{IP: "10.0.2.7", Node: "a$1"}}})
	s := string(out)
	for _, want := range []string{
		`regex         = "10\\.0\\.2\\.7:3000"`, // double backslash: Alloy string escape of a regex escape
		`replacement   = "a$$1"`,                // $ doubled: relabel replacement expansion
		`replacement  = "Org $$1"`,
		`job_name        = "gitea"`, // empty job → app name
		`job_name        = "relay"`,
		`credentials = "t0k"`,
		`action = "labeldrop"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("module lacks %s", want)
		}
	}
}

func TestRenderAppsModuleSkipsUnsafe(t *testing.T) {
	ts := sampleTargets()
	ts[1].App = `bad"name`
	out, err := RenderAppsModule(ts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "app_9_4") || !strings.Contains(string(out), "app_7_3") {
		t.Fatal("an unsafe app must be skipped alone, the rest rendered")
	}
	ts[0].Path = "/a`b" // bypassed validation somehow
	out, _ = RenderAppsModule(ts[:1], nil)
	if strings.Contains(string(out), "app_7_3") {
		t.Fatal("an unsafe path must be skipped")
	}
}

func TestRenderAppsModuleStable(t *testing.T) {
	ts := sampleTargets()
	a, _ := RenderAppsModule(ts, map[int64][]TaskNode{9: {{"10.0.2.8", "w"}, {"10.0.2.7", "v"}}})
	b, _ := RenderAppsModule(ts, map[int64][]TaskNode{9: {{"10.0.2.7", "v"}, {"10.0.2.8", "w"}}})
	if string(a) != string(b) {
		t.Fatal("task order must not change the module")
	}
}
