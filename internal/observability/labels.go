package observability

import (
	"strconv"
	"strings"
)

// Container labels Krill puts on an app's containers. The node agent reads
// them through the docker socket and turns them into krill_* log labels.
const (
	LabelOrg     = "krill.org"
	LabelOrgID   = "krill.org-id"
	LabelProject = "krill.project"
	LabelEnv     = "krill.env"
	LabelApp     = "krill.app"
	LabelAppID   = "krill.app-id"
)

// swarmServiceLabel is set by Swarm on every task container.
const swarmServiceLabel = "com.docker.swarm.service.name"

// AppIdentity names an app and where it lives.
type AppIdentity struct {
	OrgID   int64
	Org     string
	Project string
	Env     string
	AppID   int64
	App     string
}

// ContainerLabels returns the labels for an app's containers. Names are for
// people; ids keep a series continuous when something is renamed.
func ContainerLabels(id AppIdentity) map[string]string {
	return map[string]string{
		LabelOrg:     id.Org,
		LabelOrgID:   strconv.FormatInt(id.OrgID, 10),
		LabelProject: id.Project,
		LabelEnv:     id.Env,
		LabelApp:     id.App,
		LabelAppID:   strconv.FormatInt(id.AppID, 10),
	}
}

// logLabels maps container labels onto log labels, in output order.
var logLabels = []struct{ Meta, Target string }{
	{metaLabel(LabelOrg), "krill_org"},
	{metaLabel(LabelOrgID), "krill_org_id"},
	{metaLabel(LabelProject), "krill_project"},
	{metaLabel(LabelEnv), "krill_env"},
	{metaLabel(LabelApp), "krill_app"},
	{metaLabel(LabelAppID), "krill_app_id"},
	{metaLabel(swarmServiceLabel), "krill_service"},
}

// metaLabel is the discovery label Alloy's docker discovery exposes for a
// container label: every character outside [a-zA-Z0-9_] becomes "_".
func metaLabel(key string) string {
	return "__meta_docker_container_label_" + strings.Map(func(r rune) rune {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, key)
}
