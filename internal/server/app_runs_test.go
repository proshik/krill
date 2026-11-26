package server_test

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/docker"
)

// runsEngine reports a fixed service state and task list, both changeable
// between requests, so a test can follow one app from its first render to
// the deployments tab's poll.
type runsEngine struct {
	noopEngine
	snap *runsSnapshot
}

type runsSnapshot struct {
	state docker.ServiceState
	tasks []docker.TaskPlacement
}

func (e runsEngine) ServiceState(context.Context, string) (docker.ServiceState, error) {
	return e.snap.state, nil
}

func (e runsEngine) ServiceStates(_ context.Context, names []string) (map[string]docker.ServiceState, error) {
	out := map[string]docker.ServiceState{}
	for _, n := range names {
		out[n] = e.snap.state
	}
	return out, nil
}

func (e runsEngine) ServiceTasks(context.Context, string) ([]docker.TaskPlacement, error) {
	return e.snap.tasks, nil
}

func (e runsEngine) Nodes(context.Context) ([]docker.SwarmNode, error) {
	return []docker.SwarmNode{{ID: "n1", Hostname: "cp", Role: "manager", State: "ready", Leader: true}}, nil
}

var (
	tagRE   = regexp.MustCompile(`<[^>]+>`)
	spaceRE = regexp.MustCompile(`\s+`)
)

// oobRE matches an element with the given id swapped out of band.
func oobRE(id string) *regexp.Regexp {
	return regexp.MustCompile(`<[a-z]+ [^>]*id="` + id + `"[^>]*hx-swap-oob="true"`)
}

func plainText(html string) string {
	return strings.TrimSpace(spaceRE.ReplaceAllString(tagRE.ReplaceAllString(html, " "), " "))
}

// headState is the text of the sheet head's state badge in an HTML page or
// fragment, "" when there is none.
func headState(html string) string {
	i := strings.Index(html, `id="app-state"`)
	if i < 0 {
		return ""
	}
	rest := html[i:]
	return plainText(rest[strings.Index(rest, ">")+1 : strings.Index(rest, "</span>")])
}

// runsText is the "runs on" list as text: one "node state" entry per task,
// or the empty-list note.
func runsText(t *testing.T, html string) []string {
	t.Helper()
	i := strings.Index(html, `id="app-runs"`)
	if i < 0 {
		t.Fatal("no runs-on section")
	}
	rest := html[i:]
	rest = rest[:strings.Index(rest, `<p class="hint">`)]
	if !strings.Contains(rest, `class="run"`) {
		return []string{plainText(rest[strings.Index(rest, `class="empty">`)+len(`class="empty">`):])}
	}
	var out []string
	for _, row := range strings.Split(rest, `<div class="run">`)[1:] {
		out = append(out, plainText(row))
	}
	return out
}

// The sheet head and the "runs on" list tell one story: on the first render
// and in the deployments tab's poll, which refreshes both from one Swarm
// read (the head is not polled on its own there). The list never says where
// a task is meant to be ("→ running"): it lists only tasks Swarm wants
// running, so that would repeat itself.
func TestHeadAndRunsOnAgree(t *testing.T) {
	running := docker.TaskPlacement{ID: "t-old", NodeID: "n1", NodeName: "cp", State: "running", Desired: "running"}
	starting := docker.TaskPlacement{ID: "t-new", NodeID: "n1", NodeName: "cp", State: "starting", Desired: "running"}
	preparing := docker.TaskPlacement{ID: "t-new", NodeID: "n1", NodeName: "cp", State: "preparing", Desired: "running"}
	failed := docker.TaskPlacement{ID: "t-bad", NodeID: "n1", NodeName: "cp", State: "rejected", Desired: "running"}
	cases := []struct {
		name string
		snap runsSnapshot
		head string
		runs []string
	}{
		{"running", runsSnapshot{docker.ServiceState{Found: true, Desired: 1, Running: 1}, []docker.TaskPlacement{running}},
			"online", []string{"control-plane running"}},
		{"starting with nothing running", runsSnapshot{docker.ServiceState{Found: true, Desired: 1}, []docker.TaskPlacement{starting}},
			"deploying", []string{"control-plane starting"}},
		// StartFirst: the old task serves until the new one runs. Swarm lists
		// the new task first; the serving one leads the list.
		{"rolling update", runsSnapshot{docker.ServiceState{Found: true, Desired: 1, Running: 1}, []docker.TaskPlacement{starting, running}},
			"online", []string{"control-plane running", "control-plane starting"}},
		// StopFirst (a host-mode port): the old task is gone first.
		{"restart, stop first", runsSnapshot{docker.ServiceState{Found: true, Desired: 1}, []docker.TaskPlacement{preparing}},
			"deploying", []string{"control-plane starting"}},
		{"failing task", runsSnapshot{docker.ServiceState{Found: true, Desired: 1, Failed: 1}, []docker.TaskPlacement{failed}},
			"deploying", []string{"control-plane failed"}},
		// A crash loop between attempts: the next task waits out the restart
		// delay with desired state "ready", so Swarm lists no task.
		{"crash loop between attempts", runsSnapshot{docker.ServiceState{Found: true, Desired: 1}, nil},
			"deploying", []string{"no running replicas"}},
		{"stopped", runsSnapshot{docker.ServiceState{Found: true}, nil},
			"stopped", []string{"no running replicas"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := &runsSnapshot{}
			f := newSheetFixture(t, runsEngine{snap: snap}, "image")
			f.deploy(t, "nginx:1.0")
			*snap = tc.snap
			page := f.get(t, f.base+"?tab=deployments")
			poll := f.get(t, f.base+"/deployments-list")
			for what, html := range map[string]string{"first render": page, "poll": poll} {
				if got := headState(html); got != tc.head {
					t.Errorf("%s: head reads %q, want %q", what, got, tc.head)
				}
				if got := runsText(t, html); strings.Join(got, "|") != strings.Join(tc.runs, "|") {
					t.Errorf("%s: runs on reads %q, want %q", what, got, tc.runs)
				}
				if strings.Contains(html, "→") {
					t.Errorf("%s: the list still points at a desired state", what)
				}
			}
			if !oobRE("app-state").MatchString(poll) || !oobRE("app-runs").MatchString(poll) {
				t.Error("the poll must replace the head state and the runs-on list out of band")
			}
		})
	}
}

// The deployments tab's poll keeps the head in step, so the head does not
// poll on its own there (two polls would disagree for up to a period); on
// other tabs, and on a deployment's log, it still does.
func TestHeadPollsItselfOffTheDeploymentsOverview(t *testing.T) {
	snap := &runsSnapshot{docker.ServiceState{Found: true, Desired: 1, Running: 1}, nil}
	f := newSheetFixture(t, runsEngine{snap: snap}, "image")
	dep := f.deploy(t, "nginx:1.0")
	selfPoll := `hx-get="` + f.base + `/status"`
	if strings.Contains(f.get(t, f.base+"?tab=deployments"), selfPoll) {
		t.Error("the head polls itself on the deployments overview")
	}
	if strings.Contains(f.get(t, f.base+"/deployments-list"), selfPoll) {
		t.Error("the deployments poll turns the head's own poll back on")
	}
	for _, path := range []string{f.base + "?tab=logs", f.base + "?tab=settings", f.base + "/deployments/" + i64(dep.ID)} {
		if !strings.Contains(f.get(t, path), selfPoll) {
			t.Errorf("%s: the head must poll itself", path)
		}
	}
}
