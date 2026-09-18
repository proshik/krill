package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/org"
)

// nodesOrg makes an org owned by email and returns its base URL + owner cookie + id.
// The owner is promoted to instance admin, since cluster-node management is now
// gated on that flag (not org-scoped RoleAdmin).
func nodesOrg(t *testing.T, q *db.Queries, orgSvc *org.Service, email, orgName string) (base string, cookie *http.Cookie, orgID int64) {
	t.Helper()
	ownerID := mkUser(t, q, email)
	o, err := orgSvc.CreateOrg(context.Background(), ownerID, orgName)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := q.SetUserAdmin(context.Background(), db.SetUserAdminParams{ID: ownerID, IsAdmin: true}); err != nil {
		t.Fatalf("set instance admin: %v", err)
	}
	return "/orgs/" + i64(o.ID), loginAs(t, q, email), o.ID
}

// TestNodesRequireInstanceAdmin verifies an ordinary org owner (not an instance
// operator) cannot reach the global cluster-node routes: they 404, closing the
// self-created-org privilege-escalation path.
func TestNodesRequireInstanceAdmin(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ownerID := mkUser(t, q, "node-nonadmin@k.local")
	o, err := orgSvc.CreateOrg(context.Background(), ownerID, "OrgNIA")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	cookie := loginAs(t, q, "node-nonadmin@k.local")
	base := "/orgs/" + i64(o.ID)
	req, _ := http.NewRequest(http.MethodGet, base+"/nodes", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /nodes as non-instance-admin: want 404, got %d", rec.Code)
	}
	// A mutation is likewise blocked (and nothing is persisted).
	rec = postForm(t, h, base+"/nodes", cookie, url.Values{
		"name": {"w1"}, "ssh_host": {"10.0.0.2"}, "ssh_user": {"root"}, "ssh_key": {"KEY"},
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST /nodes as non-instance-admin: want 404, got %d", rec.Code)
	}
	if rows, _ := q.ListClusterNodes(context.Background()); len(rows) != 0 {
		t.Fatalf("expected no cluster_nodes rows, got %d", len(rows))
	}
}

func TestListNodesRenders(t *testing.T) {
	h, q, orgSvc := newServer(t)
	base, cookie, _ := nodesOrg(t, q, orgSvc, "nodes-list@k.local", "OrgNL")
	req, _ := http.NewRequest(http.MethodGet, base+"/nodes", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /nodes want 200, got %d", rec.Code)
	}
}

func TestAddNodeValidation(t *testing.T) {
	h, q, orgSvc := newServer(t)
	base, cookie, _ := nodesOrg(t, q, orgSvc, "nodes-add@k.local", "OrgNA")

	// missing fields -> err flash
	if rec := postForm(t, h, base+"/nodes", cookie, url.Values{"name": {"w1"}}); rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("missing fields want 303+err, got %d", rec.Code)
	}
	// all fields present but AdvertiseAddr unset in the test server -> err flash
	// (returns before any SSH attempt)
	full := url.Values{"name": {"w1"}, "ssh_host": {"10.0.0.2"}, "ssh_user": {"root"}, "ssh_key": {"KEY"}}
	if rec := postForm(t, h, base+"/nodes", cookie, full); rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("advertise unset want 303+err, got %d", rec.Code)
	}
	// invalid ssh_port -> err flash
	bad := url.Values{"name": {"w1"}, "ssh_host": {"10.0.0.2"}, "ssh_user": {"root"}, "ssh_key": {"KEY"}, "ssh_port": {"abc"}}
	if rec := postForm(t, h, base+"/nodes", cookie, bad); rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("bad port want 303+err, got %d", rec.Code)
	}
	// nothing persisted (all paths returned before create)
	if rows, _ := q.ListClusterNodes(context.Background()); len(rows) != 0 {
		t.Fatalf("expected no cluster_nodes rows, got %d", len(rows))
	}
}

func TestSetNodeAvailabilityInvalid(t *testing.T) {
	h, q, orgSvc := newServer(t)
	base, cookie, _ := nodesOrg(t, q, orgSvc, "nodes-av@k.local", "OrgAV")
	rec := postForm(t, h, base+"/nodes/abc123/availability", cookie, url.Values{"availability": {"bogus"}})
	if rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("invalid availability want 303+err, got %d", rec.Code)
	}
}

// TestSetNodeLabel covers the display-label upsert/clear/validation. The test
// server wires a nil engine, so node-existence validation is skipped and the
// label is stored as given.
func TestSetNodeLabel(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, cookie, _ := nodesOrg(t, q, orgSvc, "nodes-label@k.local", "OrgNLBL")
	const nid = "swarmnode-xyz"

	// set a label -> persisted
	if rec := postForm(t, h, base+"/nodes/"+nid+"/label", cookie, url.Values{"label": {"worker-fra"}}); rec.Code != http.StatusSeeOther || hasErrFlash(rec) {
		t.Fatalf("set label want 303 ok, got %d err=%v", rec.Code, hasErrFlash(rec))
	}
	labels, _ := q.ListNodeLabels(ctx)
	got := ""
	for _, l := range labels {
		if l.SwarmNodeID == nid {
			got = l.Label
		}
	}
	if got != "worker-fra" {
		t.Fatalf("label = %q, want worker-fra", got)
	}

	// too-long label -> err flash, value unchanged
	long := url.Values{"label": {strings.Repeat("x", 41)}}
	if rec := postForm(t, h, base+"/nodes/"+nid+"/label", cookie, long); rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("too-long label want 303+err, got %d", rec.Code)
	}

	// a label with a quote -> rejected (it's also the observability agent's
	// krill_node name, and a quote would break out of its Alloy string
	// literal); the previously saved value is untouched.
	if rec := postForm(t, h, base+"/nodes/"+nid+"/label", cookie, url.Values{"label": {`bad"name`}}); rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("quoted label want 303+err, got %d", rec.Code)
	}
	labels, _ = q.ListNodeLabels(ctx)
	got = ""
	for _, l := range labels {
		if l.SwarmNodeID == nid {
			got = l.Label
		}
	}
	if got != "worker-fra" {
		t.Fatalf("label after a rejected quote = %q, want unchanged worker-fra", got)
	}
	// a backslash and a control character (newline) are rejected the same way.
	for _, bad := range []string{`bad\name`, "bad\nname"} {
		if rec := postForm(t, h, base+"/nodes/"+nid+"/label", cookie, url.Values{"label": {bad}}); rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
			t.Fatalf("label %q want 303+err, got %d", bad, rec.Code)
		}
	}
	// a space or non-Latin text is fine — only quote/backslash/control chars
	// are rejected (see observability.SafeNodeName).
	if rec := postForm(t, h, base+"/nodes/"+nid+"/label", cookie, url.Values{"label": {"db node one"}}); rec.Code != http.StatusSeeOther || hasErrFlash(rec) {
		t.Fatalf("label with a space want 303 ok, got %d", rec.Code)
	}

	// empty label -> cleared (row deleted)
	if rec := postForm(t, h, base+"/nodes/"+nid+"/label", cookie, url.Values{"label": {""}}); rec.Code != http.StatusSeeOther || hasErrFlash(rec) {
		t.Fatalf("clear label want 303 ok, got %d", rec.Code)
	}
	labels, _ = q.ListNodeLabels(ctx)
	for _, l := range labels {
		if l.SwarmNodeID == nid {
			t.Fatalf("label not cleared: %q", l.Label)
		}
	}
}

// TestSetNodeLabelTriggersObservabilityReconcile proves setNodeLabel asks the
// observability reconciler for a pass on success — labelling or relabelling
// a node is now the actual point where the agent's krill_node mapping
// changes (addNode/removeNode no longer are, see node_handlers.go), and a
// rejected (invalid) label must not trigger one.
func TestSetNodeLabelTriggersObservabilityReconcile(t *testing.T) {
	const nid = "swarmnode-obs"
	eng := nodesEngine{live: []docker.SwarmNode{{ID: nid, Hostname: "worker-host"}}}
	h, q, orgSvc, obs := newNodeServerWithEngine(t, eng)
	base, cookie, _ := nodesOrg(t, q, orgSvc, "nodes-obs-label@k.local", "OrgObsLabel")

	// An invalid label is rejected before anything is saved — no trigger.
	if rec := postForm(t, h, base+"/nodes/"+nid+"/label", cookie, url.Values{"label": {`bad"name`}}); rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("invalid label want 303+err, got %d", rec.Code)
	}
	if got := obs.count(); got != 0 {
		t.Fatalf("Trigger called %d times for a rejected label, want 0", got)
	}

	// A valid label is saved and triggers a pass.
	if rec := postForm(t, h, base+"/nodes/"+nid+"/label", cookie, url.Values{"label": {"worker-1"}}); rec.Code != http.StatusSeeOther || hasErrFlash(rec) {
		t.Fatalf("set label want 303 ok, got %d", rec.Code)
	}
	if got := obs.count(); got != 1 {
		t.Errorf("Trigger called %d times after setting a label, want 1", got)
	}

	// Clearing it triggers another pass.
	if rec := postForm(t, h, base+"/nodes/"+nid+"/label", cookie, url.Values{"label": {""}}); rec.Code != http.StatusSeeOther || hasErrFlash(rec) {
		t.Fatalf("clear label want 303 ok, got %d", rec.Code)
	}
	if got := obs.count(); got != 2 {
		t.Errorf("Trigger called %d times after clearing a label, want 2", got)
	}
}

// TestAddNodeDoesNotTriggerObservabilityYet documents the current wiring: a
// freshly joined node has no node_labels row yet (setNodeLabel is a
// separate, later action), so the observability agent's krill_node mapping
// is unaffected by addNode alone — this test exists so a future change that
// starts calling Trigger from addNode is a deliberate decision, not an
// accidental copy-paste from removeNode. addNode's happy path itself needs a
// live SSH server to exercise (see the report), so this only pins the
// documented early-validation paths, which never reach the join at all.
func TestAddNodeDoesNotTriggerObservabilityYet(t *testing.T) {
	h, q, orgSvc, obs := newNodeServer(t)
	base, cookie, _ := nodesOrg(t, q, orgSvc, "nodes-obs-add@k.local", "OrgObsAdd")
	rec := postForm(t, h, base+"/nodes", cookie, url.Values{"name": {"w1"}, "ssh_host": {"10.0.0.2"}, "ssh_user": {"root"}, "ssh_key": {"KEY"}})
	if rec.Code != http.StatusSeeOther || !hasErrFlash(rec) {
		t.Fatalf("addNode without AdvertiseAddr want 303+err, got %d", rec.Code)
	}
	if got := obs.count(); got != 0 {
		t.Errorf("Trigger called %d times, want 0", got)
	}
}

// TestRemoveNodeTriggersObservabilityReconcile proves removeNode asks the
// observability reconciler for a pass on success: the krill_node config
// content depends on the live cluster-node list (see
// internal/observability.NodeName), so a node leaving the cluster must
// redeploy the agent just like a settings change does.
func TestRemoveNodeTriggersObservabilityReconcile(t *testing.T) {
	h, q, orgSvc, obs := newNodeServer(t)
	base, cookie, _ := nodesOrg(t, q, orgSvc, "nodes-obs-remove@k.local", "OrgObsRemove")

	// No live Swarm match (noopEngine.Nodes returns nil) and no DB instances or
	// pinned apps on it, so removeNode runs the removal path (not the confirm
	// page) even without force=1.
	rec := postForm(t, h, base+"/nodes/unknown-swarm-id/remove", cookie, url.Values{})
	if rec.Code != http.StatusSeeOther || hasErrFlash(rec) {
		t.Fatalf("remove node want 303 ok, got %d (err flash: %v)", rec.Code, hasErrFlash(rec))
	}
	if got := obs.count(); got != 1 {
		t.Errorf("observability Trigger called %d times, want 1", got)
	}
}

// TestNodesAdminGateDoesNotTriggerObservability proves a rejected (404) node
// mutation by a non-operator never reaches the point of asking for a
// reconcile pass.
func TestNodesAdminGateDoesNotTriggerObservability(t *testing.T) {
	h, q, orgSvc, obs := newNodeServer(t)
	ctx := context.Background()
	base, _, orgID := nodesOrg(t, q, orgSvc, "nodes-obs-gate-owner@k.local", "OrgObsGate")
	memberID := mkUser(t, q, "nodes-obs-gate-member@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: orgID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatalf("create member: %v", err)
	}
	mc := loginAs(t, q, "nodes-obs-gate-member@k.local")
	rec := postForm(t, h, base+"/nodes/x/remove", mc, url.Values{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("member remove want 404, got %d", rec.Code)
	}
	if got := obs.count(); got != 0 {
		t.Errorf("observability Trigger called %d times, want 0", got)
	}
}

func TestNodesAdminGate(t *testing.T) {
	h, q, orgSvc := newServer(t)
	ctx := context.Background()
	base, _, orgID := nodesOrg(t, q, orgSvc, "nodes-gate-owner@k.local", "OrgNG")
	memberID := mkUser(t, q, "nodes-gate-member@k.local")
	if _, err := q.CreateMember(ctx, db.CreateMemberParams{OrganizationID: orgID, UserID: memberID, Role: "member"}); err != nil {
		t.Fatalf("create member: %v", err)
	}
	mc := loginAs(t, q, "nodes-gate-member@k.local")
	// Node routes are instance-operator only: a non-operator (here a plain org
	// member) gets 404 (not 403) so the capability is not even disclosed.
	for _, target := range []string{base + "/nodes", base + "/nodes/x/availability", base + "/nodes/x/remove", base + "/nodes/x/label"} {
		rec := postForm(t, h, target, mc, url.Values{"name": {"w"}, "ssh_host": {"h"}, "ssh_user": {"u"}, "ssh_key": {"k"}, "availability": {"drain"}})
		if rec.Code != http.StatusNotFound {
			t.Errorf("member POST %s want 404, got %d", target, rec.Code)
		}
	}
}
