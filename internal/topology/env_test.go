package topology

import (
	"reflect"
	"testing"
)

// The owner's environment: Penpot (backend+frontend+exporter), lets-project
// and base13, one Postgres server holding both systems' databases, a bucket
// and a Valkey server used by Penpot only.
func penpotInput() EnvViewInput {
	return EnvViewInput{
		Apps: []EnvApp{
			{ID: 1, Name: "penpot-backend", Status: "running"},
			{ID: 2, Name: "penpot-frontend", Status: "running"},
			{ID: 3, Name: "penpot-exporter", Status: "error"},
			{ID: 4, Name: "lets-project", Status: "running"},
			{ID: 5, Name: "base13", Status: "running"},
			{ID: 6, Name: "stray", Status: "idle"},
		},
		Systems: []EnvSystem{{ID: 10, Name: "Penpot"}, {ID: 11, Name: "lets-project"}, {ID: 12, Name: "base13"}},
		Members: map[int64]int64{1: 10, 2: 10, 3: 10, 4: 11, 5: 12},
		Resources: map[string]EnvResource{
			"ldb:1":    {Key: "ldb:1", Kind: "ldb", Label: "penpot", Server: "pg-main", Engine: "postgres", InstanceID: 100},
			"ldb:2":    {Key: "ldb:2", Kind: "ldb", Label: "lets_project", Server: "pg-main", Engine: "postgres", InstanceID: 100},
			"bucket:7": {Key: "bucket:7", Kind: "bucket", Label: "penpot", Server: "silo", Engine: "minio", InstanceID: 200},
			"inst:300": {Key: "inst:300", Kind: "instance", Label: "penpot-cache", Server: "penpot-cache", Engine: "valkey", InstanceID: 300},
		},
		Links: []EnvLink{
			{AppID: 1, ResKey: "ldb:1", VarName: "PENPOT_DATABASE_URI"},
			{AppID: 1, ResKey: "bucket:7", VarName: "AWS_ACCESS_KEY_ID"},
			{AppID: 1, ResKey: "bucket:7", VarName: "AWS_SECRET_ACCESS_KEY"},
			{AppID: 1, ResKey: "inst:300", VarName: "PENPOT_REDIS_URI"},
			{AppID: 3, ResKey: "inst:300", VarName: "PENPOT_REDIS_URI", Detected: true},
			{AppID: 4, ResKey: "ldb:2", VarName: "DATABASE_URL"},
			{AppID: 1, ResKey: "ldb:1", VarName: "OLD_DSN", Detected: true},
			{AppID: 99, ResKey: "ldb:1", VarName: "GHOST"},   // unknown app: dropped
			{AppID: 2, ResKey: "ldb:404", VarName: "GHOST"}, // unknown resource: dropped
		},
		HostedByInstance: map[int64]int{100: 2, 200: 1},
	}
}

func TestBuildEnvViewGroupsSystemsAndTheirResources(t *testing.T) {
	v := BuildEnvView(penpotInput())
	if len(v.Systems) != 3 || v.Systems[0].Name != "Penpot" {
		t.Fatalf("systems: %+v", v.Systems)
	}
	penpot := v.Systems[0]
	if len(penpot.Apps) != 3 || penpot.Running != 2 || penpot.Failing != 1 {
		t.Fatalf("penpot apps/status: %+v", penpot)
	}
	keys := []string{}
	for _, r := range penpot.Resources {
		keys = append(keys, r.Key)
	}
	if !reflect.DeepEqual(keys, []string{"ldb:1", "bucket:7", "inst:300"}) {
		t.Fatalf("penpot resources (logical DBs, buckets, servers): %v", keys)
	}
	// The backend's links are exactly its modeled and detected uses.
	if !reflect.DeepEqual(penpot.Apps[0].Uses, []string{"bucket:7", "inst:300", "ldb:1"}) {
		t.Fatalf("backend uses: %v", penpot.Apps[0].Uses)
	}
	if len(penpot.Apps[1].Uses) != 0 {
		t.Fatalf("frontend uses nothing modeled or detected: %v", penpot.Apps[1].Uses)
	}
	res := map[string]ResourceView{}
	for _, r := range penpot.Resources {
		res[r.Key] = r
	}
	// penpot's database lives on a server that also hosts lets-project's.
	if !res["ldb:1"].Shared {
		t.Fatal("a database on a server hosting another system's database is shared")
	}
	// A modeled link replaces the detected variable for the same use.
	if !reflect.DeepEqual(res["ldb:1"].Vars, []string{"PENPOT_DATABASE_URI"}) || res["ldb:1"].Detected {
		t.Fatalf("ldb vars: %+v", res["ldb:1"])
	}
	if res["bucket:7"].Shared || !reflect.DeepEqual(res["bucket:7"].Vars, []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"}) {
		t.Fatalf("bucket: %+v", res["bucket:7"])
	}
	// Valkey is used by two Penpot apps (one detected) and nothing else.
	if v := res["inst:300"]; v.Shared || v.Detected || !reflect.DeepEqual(v.Users, []int64{1, 3}) {
		t.Fatalf("valkey: %+v", v)
	}
	lets := v.Systems[1]
	if len(lets.Resources) != 1 || !lets.Resources[0].Shared {
		t.Fatalf("lets-project sees its database on the shared server: %+v", lets.Resources)
	}
	if base := v.Systems[2]; len(base.Resources) != 0 || len(base.Apps) != 1 {
		t.Fatalf("base13: %+v", base)
	}
	if len(v.Ungrouped.Apps) != 1 || v.Ungrouped.Apps[0].Name != "stray" {
		t.Fatalf("ungrouped: %+v", v.Ungrouped)
	}
}

// A resource used from two systems is shared in both; a resource only seen in
// env values is marked detected.
func TestBuildEnvViewSharedAndDetected(t *testing.T) {
	in := penpotInput()
	in.Links = append(in.Links, EnvLink{AppID: 5, ResKey: "inst:300", VarName: "CACHE", Detected: true})
	v := BuildEnvView(in)
	for _, s := range []SystemView{v.Systems[0], v.Systems[2]} {
		found := false
		for _, r := range s.Resources {
			if r.Key == "inst:300" {
				found = true
				if !r.Shared {
					t.Fatalf("%s: valkey used by two systems must be shared", s.Name)
				}
			}
		}
		if !found {
			t.Fatalf("%s: valkey missing", s.Name)
		}
	}
	if r := v.Systems[2].Resources[0]; !r.Detected {
		t.Fatalf("base13 only detects valkey: %+v", r)
	}
}

// Apps of a system that no longer exists fall back to ungrouped, and an
// environment without systems is all ungrouped.
func TestBuildEnvViewWithoutSystems(t *testing.T) {
	in := penpotInput()
	in.Systems = nil
	v := BuildEnvView(in)
	if len(v.Systems) != 0 || len(v.Ungrouped.Apps) != 6 || len(v.Ungrouped.Resources) != 4 {
		t.Fatalf("view: %d systems, %d apps, %d resources", len(v.Systems), len(v.Ungrouped.Apps), len(v.Ungrouped.Resources))
	}
}
