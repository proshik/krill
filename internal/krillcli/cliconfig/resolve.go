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
// KRILL_TOKEN short-circuits all of it and forms an unnamed context, which is
// how the same binary runs in CI with no config file on disk.
func Resolve(s *Store, o Options) (Resolved, error) {
	if tok := strings.TrimSpace(o.EnvToken); tok != "" {
		server := firstNonEmpty(o.ServerFlag, o.EnvServer)
		if server == "" {
			// Same server, different token: a common way to use a
			// short-lived CI token against an already-configured host.
			if cur, ok := s.Contexts[s.Current]; ok {
				server = cur.Server
			}
		}
		if server == "" {
			return Resolved{}, fmt.Errorf("%s is set but no server is: pass --server or set %s", EnvToken, EnvServer)
		}
		return Resolved{Server: server, Token: tok}, nil
	}

	name := firstNonEmpty(o.ContextFlag, o.EnvContext, o.ProjectContext, s.Current)
	if name == "" {
		return Resolved{}, fmt.Errorf("not logged in to any Krill server — run `krill-cli login --server https://krill.example.com`")
	}
	c, ok := s.Contexts[name]
	if !ok {
		if name == o.ProjectContext {
			return Resolved{}, fmt.Errorf("%s pins this project to context %q, which does not exist here — run `krill-cli login --server <url> --name %s`", "krill.yaml", name, name)
		}
		return Resolved{}, fmt.Errorf("no context named %q; run `krill-cli context` to list them", name)
	}
	server := firstNonEmpty(o.ServerFlag, c.Server)
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
