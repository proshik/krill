package server_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
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
	// stateTasks, when set, is what ServiceState counted: a task that moved
	// between its listing and ServiceTasks's.
	stateTasks []docker.TaskPlacement
	tasksErr   error
	taskReads  int
	nodes      []docker.SwarmNode // nil: one ready control plane
}

func (e runsEngine) ServiceState(context.Context, string) (docker.ServiceState, error) {
	if e.snap.stateTasks != nil {
		st := e.snap.state
		st.Running = 0
		for _, t := range e.snap.stateTasks {
			if t.State == "running" {
				st.Running++
			}
		}
		return st, nil
	}
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
	e.snap.taskReads++
	if !e.snap.state.Found {
		return nil, errors.New("service krill-1 not found") // as the daemon answers
	}
	return e.snap.tasks, e.snap.tasksErr
}

func (e runsEngine) Nodes(context.Context) ([]docker.SwarmNode, error) {
	if e.snap.nodes != nil {
		return e.snap.nodes, nil
	}
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
		{"running", runsSnapshot{state: docker.ServiceState{Found: true, Desired: 1, Running: 1}, tasks: []docker.TaskPlacement{running}},
			"online", []string{"control-plane running"}},
		{"starting with nothing running", runsSnapshot{state: docker.ServiceState{Found: true, Desired: 1}, tasks: []docker.TaskPlacement{starting}},
			"deploying", []string{"control-plane starting"}},
		// StartFirst: the old task serves until the new one runs. Swarm lists
		// the new task first; the serving one leads the list.
		{"rolling update", runsSnapshot{state: docker.ServiceState{Found: true, Desired: 1, Running: 1}, tasks: []docker.TaskPlacement{starting, running}},
			"online", []string{"control-plane running", "control-plane starting"}},
		// StopFirst (a host-mode port): the old task is gone first.
		{"restart, stop first", runsSnapshot{state: docker.ServiceState{Found: true, Desired: 1}, tasks: []docker.TaskPlacement{preparing}},
			"deploying", []string{"control-plane starting"}},
		{"failing task", runsSnapshot{state: docker.ServiceState{Found: true, Desired: 1, Failed: 1}, tasks: []docker.TaskPlacement{failed}},
			"deploying", []string{"control-plane failed"}},
		// A crash loop between attempts: the next task waits out the restart
		// delay with desired state "ready", so Swarm lists no task.
		{"crash loop between attempts", runsSnapshot{state: docker.ServiceState{Found: true, Desired: 1}},
			"deploying", []string{"no running replicas"}},
		{"stopped", runsSnapshot{state: docker.ServiceState{Found: true}},
			"stopped", []string{"no running replicas"}},
		// ServiceState and ServiceTasks list tasks in two requests: the old
		// task of a StopFirst restart was running for the first and gone for
		// the second. The head counts the tasks the list shows.
		{"task stopped between reads", runsSnapshot{state: docker.ServiceState{Found: true, Desired: 1}, tasks: []docker.TaskPlacement{starting}, stateTasks: []docker.TaskPlacement{running}},
			"deploying", []string{"control-plane starting"}},
		// A global service wants one task per eligible node: its desired
		// count is its task list.
		{"global, one node still starting", runsSnapshot{state: docker.ServiceState{Found: true, Global: true, Desired: 2, Running: 2}, tasks: []docker.TaskPlacement{running, starting}},
			"deploying", []string{"control-plane running", "control-plane starting"}},
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
	snap := &runsSnapshot{state: docker.ServiceState{Found: true, Desired: 1, Running: 1}}
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

// An app with no service yet (never deployed) has no tasks to ask Swarm
// for: the daemon answers "not found", which the 3-second poll would log
// every time.
func TestRunsOnWithoutAServiceSkipsTheTaskRead(t *testing.T) {
	snap := &runsSnapshot{}
	f := newSheetFixture(t, runsEngine{snap: snap}, "image")
	for _, path := range []string{f.base + "?tab=deployments", f.base + "/deployments-list"} {
		html := f.get(t, path)
		if got := headState(html); got != "not deployed" {
			t.Errorf("%s: head reads %q, want not deployed", path, got)
		}
		if got := runsText(t, html); strings.Join(got, "|") != "no running replicas" {
			t.Errorf("%s: runs on reads %q", path, got)
		}
	}
	if snap.taskReads != 0 {
		t.Errorf("asked Swarm %d times for the tasks of a service that is not there", snap.taskReads)
	}
}

// A failed task read must not put "no running replicas" next to "online":
// the poll then leaves the head and the list the page shows.
func TestRunsOnKeptWhenTheTaskReadFails(t *testing.T) {
	snap := &runsSnapshot{state: docker.ServiceState{Found: true, Desired: 1, Running: 1}, tasksErr: errors.New("daemon busy")}
	f := newSheetFixture(t, runsEngine{snap: snap}, "image")
	f.deploy(t, "nginx:1.0")
	poll := f.get(t, f.base+"/deployments-list")
	if strings.Contains(poll, `id="app-state"`) || strings.Contains(poll, `id="app-runs"`) {
		t.Fatal("a failed read replaced the head state or the runs-on list")
	}
	if !strings.Contains(poll, `id="deployments-list"`) {
		t.Fatal("the deployments list itself must still refresh")
	}
}

// A pinned app whose only node is down reads "node down" in the head on the
// first render, in the head's own poll and in the deployments poll.
func TestHeadNodeBadgeOnEveryPath(t *testing.T) {
	snap := &runsSnapshot{state: docker.ServiceState{Found: true, Desired: 1}, nodes: []docker.SwarmNode{
		{ID: "n1", Hostname: "cp", Role: "manager", State: "ready", Leader: true},
		{ID: "n2", Hostname: "w2", Role: "worker", State: "down"},
	}}
	f := newSheetFixture(t, runsEngine{snap: snap}, "image")
	f.deploy(t, "nginx:1.0")
	if err := f.q.SetApplicationPlacement(context.Background(), db.SetApplicationPlacementParams{ID: f.app.ID, PlacementMode: "pin", PlacementNodes: "n2"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{f.base + "?tab=deployments", f.base + "?tab=logs", f.base + "/status", f.base + "/deployments-list"} {
		if got := headState(f.get(t, path)); got != "node down" {
			t.Errorf("%s: head reads %q, want node down", path, got)
		}
	}
}
