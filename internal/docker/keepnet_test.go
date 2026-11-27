package docker

import (
	"reflect"
	"testing"

	"github.com/docker/docker/api/types/swarm"
)

func TestKeepAttachments(t *testing.T) {
	cur := []swarm.NetworkAttachmentConfig{
		{Target: "id-org"},
		{Target: "id-egress", Aliases: []string{"api.telegram.org"}},
		{Target: "id-other"},
		{Target: "id-gone", Aliases: []string{"x"}},
	}
	names := map[string]string{"id-org": "krill-org-1", "id-egress": "krill-egress-7", "id-other": "krill-net"}
	want := []swarm.NetworkAttachmentConfig{{Target: "krill-org-2"}}
	got := keepAttachments(cur, names, want, "krill-egress-")
	exp := []swarm.NetworkAttachmentConfig{{Target: "krill-org-2"}, {Target: "id-egress", Aliases: []string{"api.telegram.org"}}}
	if !reflect.DeepEqual(got, exp) {
		t.Fatalf("got %+v, want %+v", got, exp)
	}
	// A spec that already attaches the network keeps its own attachment.
	want = []swarm.NetworkAttachmentConfig{{Target: "krill-org-1"}, {Target: "krill-egress-7", Aliases: []string{"new.example.com"}}}
	got = keepAttachments(cur, names, want, "krill-egress-")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want the spec's own attachments %+v", got, want)
	}
	// The kept aliases are a copy: changing them later does not touch cur.
	got = keepAttachments(cur, names, nil, "krill-egress-")
	got[0].Aliases[0] = "changed"
	if cur[1].Aliases[0] != "api.telegram.org" {
		t.Fatal("kept aliases share memory with the running spec")
	}
}
