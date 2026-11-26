package server_test

import (
	"context"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
)

// The canvas's node filter matches cards to nodes by Swarm node ID, so a
// label with a space still filters; and someone who may not see the cluster
// is shown only the nodes this environment's cards run on, while an
// instance operator gets every node.
func TestCanvasNodeChips(t *testing.T) {
	var tasks []docker.TaskInfo
	eng := nodesEngine{taskInfos: &tasks, live: []docker.SwarmNode{
		{ID: "n1", Hostname: "cp", Role: "manager", State: "ready", Leader: true},
		{ID: "n2", Hostname: "w2", Role: "worker", State: "ready"},
		{ID: "n3", Hostname: "secret-worker-host", Role: "worker", State: "ready"},
	}}
	h, q, orgSvc := newServerWithNodesEngine(t, eng)
	ctx := context.Background()
	owner := mkUser(t, q, "chips-owner@k.local")
	o, _ := orgSvc.CreateOrg(ctx, owner, "Chips")
	p, _ := orgSvc.CreateProject(ctx, o.ID, "shop", "")
	e, _ := orgSvc.CreateEnvironment(ctx, p.ID, "production")
	app, err := q.CreateApplication(ctx, db.CreateApplicationParams{
		EnvironmentID: e.ID, Name: "api", Image: "nginx", Tag: "1", Domain: "chips-api.example.com", Port: 80,
		SourceType: "image", DockerfilePath: "Dockerfile",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.UpsertNodeLabel(ctx, db.UpsertNodeLabelParams{SwarmNodeID: "n2", Label: "west worker"}); err != nil {
		t.Fatal(err)
	}
	tasks = []docker.TaskInfo{{ServiceName: "krill-" + i64(app.ID), NodeID: "n2", State: "running", Desired: "running"}}

	member := mkUser(t, q, "chips-member@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: o.ID, UserID: member, Role: "member"}); err != nil {
		t.Fatal(err)
	}
	path := "/orgs/" + i64(o.ID) + "/projects/" + i64(p.ID) + "/environments/" + i64(e.ID)

	body := getWithCookie(t, h, path, loginAs(t, q, "chips-member@k.local")).Body.String()
	if !strings.Contains(body, `data-node="n2"`) || !strings.Contains(body, `data-nodes="n2"`) || !strings.Contains(body, "west worker") {
		t.Fatalf("the card's node chip must match by node ID and show the label:\n%s", excerpt(body, "float--nodes"))
	}
	if strings.Contains(body, `data-node="n3"`) || strings.Contains(body, "secret-worker-host") || strings.Contains(body, `data-node="n1"`) {
		t.Fatal("a member is shown cluster nodes no card of this environment runs on")
	}

	// The status poll carries the placement of the same snapshot as the
	// badges: the app moves from n2 to n1, and its card follows.
	tasks = []docker.TaskInfo{{ServiceName: "krill-" + i64(app.ID), NodeID: "n1", State: "running", Desired: "running"}}
	poll := getWithCookie(t, h, path+"/statuses", loginAs(t, q, "chips-member@k.local")).Body.String()
	// Each element is swapped out of band only with an explicit value: htmx 2
	// reads a bare hx-swap-oob as none, and the poll (hx-swap="none") then
	// drops the element.
	where := `id="cw-s-app-` + i64(app.ID) + `" data-nodes="n1" hx-swap-oob="true"`
	if !strings.Contains(poll, where) {
		t.Fatalf("the poll does not refresh where the app runs:\n%s", poll)
	}
	if !strings.Contains(poll, `id="cn-n1" hx-swap-oob="true"`) || !strings.Contains(poll, `id="cs-app-`+i64(app.ID)+`"`) {
		t.Fatalf("the poll lacks the node counts or the badge:\n%s", poll)
	}

	if err := q.SetUserAdmin(ctx, db.SetUserAdminParams{ID: owner, IsAdmin: true}); err != nil {
		t.Fatal(err)
	}
	if body := getWithCookie(t, h, path, loginAs(t, q, "chips-owner@k.local")).Body.String(); !strings.Contains(body, `data-node="n3"`) {
		t.Fatal("an instance operator must see every node in the filter")
	}
}

func excerpt(body, marker string) string {
	i := strings.Index(body, marker)
	if i < 0 {
		return "(no " + marker + ")"
	}
	end := i + 800
	if end > len(body) {
		end = len(body)
	}
	return body[i:end]
}
