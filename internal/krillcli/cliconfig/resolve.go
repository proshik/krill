package cliconfig

import (
	"fmt"
	"strings"
)

// Environment variables. KRILL_TOKEN and KRILL_SERVER match the names the
// README's CI examples already use. Anything this CLI invents is prefixed
// KRILL_CLI_ so it can never collide with the server's own KRILL_* settings
// on a machine that runs both.
const (
	EnvToken   = "KRILL_TOKEN"
	EnvServer  = "KRILL_SERVER"
	EnvContext = "KRILL_CLI_CONTEXT"
)

// Options are the inputs to Resolve, gathered from flags, the environment and
// krill.yaml.
type Options struct {
	ContextFlag string // --context
	ServerFlag  string // --server

	// ProjectContext is krill.yaml's `context:`, pinning a repository to one
	// server.
	ProjectContext string

	EnvToken   string
	EnvServer  string
	EnvContext string
}

// Resolved is the server and credential one command will use.
type Resolved struct {
	// Name is the stored context this came from; empty when the environment
	// supplied the token directly.
	Name    string
	Server  string
	Token   string
	Level   string
	OrgName string
}

// CanWriteHint reports whether the CACHED level rules out writing.
//
// It answers only "definitely not": a cached read level is a fact about the
// token itself and lets a deploy fail before a four-minute build, in zero
// requests. A cached write level proves nothing, because the effective right
// is the token's level intersected with its owner's current role — so the
// caller still confirms with whoami before writing.
func (r Resolved) CanWriteHint() bool { return r.Level != "read" }

// Resolve picks the context to act as.
//
// Precedence for WHICH context: --context, then KRILL_CLI_CONTEXT, then
// krill.yaml's `context:`, then the stored current one. The project's pin
// sits above the stored current deliberately — a shell left pointing at
// production must not ship a project that belongs to staging — but below the
// flag and the environment, because those are someone typing an override now.
//
// KRILL_TOKEN replaces the CREDENTIAL, never the choice of server. It used to
// short-circuit the whole function, which quietly defeated every selector
// above: `--context staging` with a staging token exported would send that
// token to whichever server happened to be current, and the project pin whose
// entire purpose is to stop a production shell from shipping a staging project
// was bypassed by any exported token. The token says who you are; the context
// says where you are, and the two are answered separately.
func Resolve(s *Store, o Options) (Resolved, error) {
	envToken := strings.TrimSpace(o.EnvToken)

	name := firstNonEmpty(o.ContextFlag, o.EnvContext, o.ProjectContext, s.Current)
	if name == "" {
		// No stored context at all. With a token and a server in the
		// environment this is a CI runner with no config file, which is a
		// supported way to run; without them there is nothing to act as.
		if envToken == "" {
			return Resolved{}, fmt.Errorf("not logged in to any Krill server — run `krill-cli login --server https://krill.example.com`")
		}
		server := firstNonEmpty(o.ServerFlag, o.EnvServer)
		if server == "" {
			return Resolved{}, fmt.Errorf("%s is set but no server is: pass --server or set %s", EnvToken, EnvServer)
		}
		return Resolved{Server: server, Token: envToken}, nil
	}

	c, ok := s.Contexts[name]
	if !ok {
		// An explicitly named context that does not exist is an error even
		// when a token is exported: silently falling back would send it
		// somewhere the user did not name.
		if name == o.ProjectContext {
			return Resolved{}, fmt.Errorf("%s pins this project to context %q, which does not exist here — run `krill-cli login --server <url> --name %s`", "krill.yaml", name, name)
		}
		return Resolved{}, fmt.Errorf("no context named %q; run `krill-cli context` to list them", name)
	}

	server := firstNonEmpty(o.ServerFlag, o.EnvServer, c.Server)
	if envToken != "" {
		// The stored level describes the stored token, not this one, so it is
		// deliberately left empty: a cached "read" would refuse a write token
		// the environment just supplied.
		return Resolved{Name: name, Server: server, Token: envToken, OrgName: c.OrgName}, nil
	}
	if trimmed(c.Token) == "" {
		return Resolved{}, fmt.Errorf("context %q has no token — run `krill-cli login --server %s --name %s`", name, server, name)
	}
	return Resolved{
		Name:    name,
		Server:  server,
		Token:   c.Token,
		Level:   c.Level,
		OrgName: c.OrgName,
	}, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if t := trimmed(v); t != "" {
			return t
		}
	}
	return ""
}
