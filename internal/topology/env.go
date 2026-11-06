package topology

import (
	"sort"
)

// The environment view groups an environment's apps into systems and shows,
// per system, the resources its apps actually use. Like Build it is pure: the
// caller adapts rows into EnvViewInput, already restricted to what the viewer
// may see, and renders the result.

// EnvApp is one application of the environment.
type EnvApp struct {
	ID     int64
	Name   string
	Image  string
	Status string // derived live status (running/deploying/error/idle)
}

// EnvResource is something an app can use: a logical database (key
// "ldb:<id>"), a bucket ("bucket:<id>") or a whole server ("inst:<id>").
type EnvResource struct {
	Key        string
	Kind       string // "ldb" | "bucket" | "instance"
	Label      string // "penpot"
	Server     string // the server it lives on ("pg-main"); for a whole server, its own name
	Engine     string // postgres | redis | valkey | dragonfly | minio
	InstanceID int64  // the server's id
	Href       string
}

// EnvLink is one use of a resource by an app.
type EnvLink struct {
	AppID    int64
	ResKey   string
	VarName  string
	Detected bool // inferred from a raw env value, not a modeled link
}

// EnvSystem is a named group of apps.
type EnvSystem struct {
	ID   int64
	Name string
}

// EnvViewInput feeds BuildEnvView.
type EnvViewInput struct {
	Apps      []EnvApp
	Systems   []EnvSystem
	Members   map[int64]int64 // app id -> system id
	Resources map[string]EnvResource
	Links     []EnvLink
	// HostedByInstance counts the modeled resources (logical databases and
	// buckets, in every environment) each server hosts.
	HostedByInstance map[int64]int
}

// EnvView is the grouped environment.
type EnvView struct {
	Systems   []SystemView
	Ungrouped SystemView // ID 0: apps in no system
}

// SystemView is one system card (or the ungrouped section).
type SystemView struct {
	ID        int64
	Name      string
	Apps      []AppView
	Resources []ResourceView
	Running   int
	Failing   int
}

// AppView is a component card with the resources it uses.
type AppView struct {
	EnvApp
	Uses []string // resource keys, sorted
}

// ResourceView is a resource chip of a system.
type ResourceView struct {
	EnvResource
	Users []int64 // app ids of this system using it, sorted
	Vars  []string
	// Shared: the resource, or the server it lives on, also serves something
	// outside this system. The system never owns a shared resource.
	Shared bool
	// Detected: every use of it in this system is inferred, none modeled.
	Detected bool
}

// BuildEnvView groups apps by system and attaches each system's resources.
// Modeled links win over detected ones for the same app and resource; a link
// to an unknown resource or app is dropped.
func BuildEnvView(in EnvViewInput) EnvView {
	appSystem := func(id int64) int64 { return in.Members[id] }
	known := map[int64]bool{}
	for _, a := range in.Apps {
		known[a.ID] = true
	}
	// One use per (app, resource). A modeled link makes the use modeled and
	// its variables replace any detected ones.
	type pair struct {
		app int64
		res string
	}
	type use struct {
		modeled           bool
		modeledVars, seen []string
	}
	uses := map[pair]*use{}
	for _, l := range in.Links {
		if !known[l.AppID] {
			continue
		}
		if _, ok := in.Resources[l.ResKey]; !ok {
			continue
		}
		k := pair{l.AppID, l.ResKey}
		u := uses[k]
		if u == nil {
			u = &use{}
			uses[k] = u
		}
		if l.Detected {
			u.seen = appendUnique(u.seen, l.VarName)
		} else {
			u.modeled = true
			u.modeledVars = appendUnique(u.modeledVars, l.VarName)
		}
	}
	links := map[pair]EnvLink{}
	vars := map[pair][]string{}
	for k, u := range uses {
		links[k] = EnvLink{AppID: k.app, ResKey: k.res, Detected: !u.modeled}
		if u.modeled {
			vars[k] = u.modeledVars
		} else {
			vars[k] = u.seen
		}
	}
	// Who uses each resource, and from which system.
	usersBySystem := map[string]map[int64]bool{} // res -> system ids
	for k := range links {
		if usersBySystem[k.res] == nil {
			usersBySystem[k.res] = map[int64]bool{}
		}
		usersBySystem[k.res][appSystem(k.app)] = true
	}

	views := map[int64]*SystemView{0: {ID: 0}}
	order := []int64{}
	for _, s := range in.Systems {
		views[s.ID] = &SystemView{ID: s.ID, Name: s.Name}
		order = append(order, s.ID)
	}
	for _, a := range in.Apps {
		sid := appSystem(a.ID)
		v, ok := views[sid]
		if !ok {
			v = views[0]
			sid = 0
		}
		av := AppView{EnvApp: a}
		for k := range links {
			if k.app == a.ID {
				av.Uses = append(av.Uses, k.res)
			}
		}
		sort.Strings(av.Uses)
		v.Apps = append(v.Apps, av)
		switch a.Status {
		case "running":
			v.Running++
		case "error":
			v.Failing++
		}
	}
	for sid, v := range views {
		byRes := map[string]*ResourceView{}
		for k, l := range links {
			if appSystemOr0(views, appSystem(k.app)) != sid {
				continue
			}
			rv := byRes[k.res]
			if rv == nil {
				rv = &ResourceView{EnvResource: in.Resources[k.res], Detected: true}
				byRes[k.res] = rv
			}
			rv.Users = append(rv.Users, k.app)
			rv.Vars = appendUnique(rv.Vars, vars[k]...)
			if !l.Detected {
				rv.Detected = false
			}
		}
		// The resources of this system on each server, to tell whether the
		// server hosts anything else.
		ownOnServer := map[int64]int{}
		for _, rv := range byRes {
			if rv.Kind != "instance" {
				ownOnServer[rv.InstanceID]++
			}
		}
		for key, rv := range byRes {
			sort.Slice(rv.Users, func(i, j int) bool { return rv.Users[i] < rv.Users[j] })
			sort.Strings(rv.Vars)
			outside := false
			for other := range usersBySystem[key] {
				if appSystemOr0(views, other) != sid {
					outside = true
				}
			}
			hosted := in.HostedByInstance[rv.InstanceID]
			switch rv.Kind {
			case "instance":
				// A whole-server link is shared when the server also hosts
				// databases or buckets.
				rv.Shared = outside || hosted > 0
			default:
				rv.Shared = outside || hosted > ownOnServer[rv.InstanceID] || serverUsedElsewhere(usersBySystem, in.Resources, rv.InstanceID, sid, views)
			}
			v.Resources = append(v.Resources, *rv)
		}
		sort.Slice(v.Resources, func(i, j int) bool {
			a, b := v.Resources[i], v.Resources[j]
			if kindOrder(a.Kind) != kindOrder(b.Kind) {
				return kindOrder(a.Kind) < kindOrder(b.Kind)
			}
			return a.Key < b.Key
		})
	}
	out := EnvView{Ungrouped: *views[0]}
	for _, id := range order {
		out.Systems = append(out.Systems, *views[id])
	}
	return out
}

// appSystemOr0 maps a system id to itself when it is a known system, else 0.
func appSystemOr0(views map[int64]*SystemView, sid int64) int64 {
	if _, ok := views[sid]; ok {
		return sid
	}
	return 0
}

// serverUsedElsewhere reports whether a resource on the same server is used
// by an app outside system sid.
func serverUsedElsewhere(users map[string]map[int64]bool, res map[string]EnvResource, instID, sid int64, views map[int64]*SystemView) bool {
	for key, systems := range users {
		r := res[key]
		if r.InstanceID != instID {
			continue
		}
		for other := range systems {
			if appSystemOr0(views, other) != sid {
				return true
			}
		}
	}
	return false
}

func kindOrder(kind string) int {
	switch kind {
	case "ldb":
		return 0
	case "bucket":
		return 1
	}
	return 2
}

func appendUnique(s []string, vs ...string) []string {
	for _, v := range vs {
		found := false
		for _, x := range s {
			if x == v {
				found = true
				break
			}
		}
		if !found {
			s = append(s, v)
		}
	}
	return s
}
