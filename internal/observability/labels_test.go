package observability

import "testing"

func TestContainerLabels(t *testing.T) {
	got := ContainerLabels(AppIdentity{OrgID: 7, Org: "Acme", Project: "shop", Env: "production", AppID: 42, App: "web"})
	want := map[string]string{
		"krill.org": "Acme", "krill.org-id": "7", "krill.project": "shop",
		"krill.env": "production", "krill.app": "web", "krill.app-id": "42",
	}
	if len(got) != len(want) {
		t.Fatalf("labels = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestMetaLabel(t *testing.T) {
	for in, want := range map[string]string{
		"krill.org-id":                  "__meta_docker_container_label_krill_org_id",
		"krill.app":                     "__meta_docker_container_label_krill_app",
		"com.docker.swarm.service.name": "__meta_docker_container_label_com_docker_swarm_service_name",
	} {
		if got := metaLabel(in); got != want {
			t.Errorf("metaLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
