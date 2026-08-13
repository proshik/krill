// Package mcpsrv is the MCP (Model Context Protocol) adapter over
// internal/api.Service: a streamable-HTTP MCP server exposing the same
// twelve agent-facing operations the REST adapter (internal/server's
// /api/v1) exposes, mapped onto MCP tools instead of HTTP routes. Both
// adapters share the exact same tenancy/permission logic in
// internal/api.Service; this package is deliberately thin — it only reads
// identity, decodes arguments, calls the service, and maps the result or the
// error (see tools.go).
//
// Architecture note (why one shared *mcp.Server, identity from ctx): Task 0's
// spike (docs/superpowers/plans/2026-08-13-krill-mcp-agent-api.md, section
// "Результат спайка") verified that the Identity RequireAPIToken
// (internal/server) stashes on the *http.Request's context via
// api.WithIdentity DOES reach a tool handler's ctx.Value(...) — confirmed
// across three independent runs, using a single shared *mcp.Server rather
// than one rebuilt per request. So Server builds exactly one *mcp.Server at
// construction time (New) and every tool handler reads the caller's Identity
// from ctx via api.IdentityFrom, the same pattern the REST adapter uses via
// api_middleware.go. There is no per-request factory baking identity into
// tool closures.
package mcpsrv

import (
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/proshik/krill/internal/api"
)

// serverVersion is reported to MCP clients during initialize. Purely
// descriptive metadata; it has no effect on behavior.
const serverVersion = "0.1.0"

// Server is the MCP adapter: one shared *mcp.Server with all twelve tools
// registered once, wrapped in a streamable-HTTP handler that is itself built
// once and reused for every mount point (see Handler).
type Server struct {
	mcpServer *mcp.Server
	handler   http.Handler
}

// New builds the MCP server over svc and registers every tool in the
// registry (see tools.go, toolRegistrations). svc must not be nil.
//
// sessionTimeout bounds how long an idle session is kept before the handler
// closes it (see the SessionTimeout comment in New's options literal below);
// 0 means "never close an idle session", the SDK's zero-value behavior.
func New(svc *api.Service, sessionTimeout time.Duration) *Server {
	impl := &mcp.Implementation{Name: "krill", Version: serverVersion}
	srv := mcp.NewServer(impl, nil)
	registerTools(srv, svc)

	s := &Server{mcpServer: srv}
	// getServer always returns the same *mcp.Server regardless of the
	// request: there is nothing request-specific to bake in at this layer —
	// the caller's Identity travels through ctx instead (see the package
	// doc). JSONResponse:true keeps every tools/call response a single JSON
	// body rather than a text/event-stream of one event; both are
	// spec-compliant streamable-HTTP response shapes ($2.1.5 of the MCP
	// spec), and JSON is simpler for a caller (and for this package's own
	// protocol smoke test) to consume without an SSE parser.
	//
	// SessionTimeout reclaims idle sessions. The SDK closes a session only on
	// an explicit DELETE /mcp or a transport error, and its zero value means
	// "never close an idle session" — so an agent that crashes, a CI job that
	// exits, a restarted container or a dropped connection would each leave a
	// session (and the goroutine serving it) alive in both this handler's
	// session table and the shared *mcp.Server's session list, for the life
	// of the process. Krill runs for months as a systemd unit on a small VPS,
	// so that is a slow leak, not a rounding error.
	s.handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return srv
	}, &mcp.StreamableHTTPOptions{JSONResponse: true, SessionTimeout: sessionTimeout})
	return s
}

// Handler returns the streamable-HTTP handler serving MCP sessions. It is
// built once in New and returned as-is on every call: a StreamableHTTPHandler
// owns the session table an incoming request's Mcp-Session-Id is looked up
// in, so mounting it at more than one route (e.g. "/mcp" and "/mcp/*") MUST
// share this exact instance — two separately constructed handlers would each
// hold their own session table, and a client's session started against one
// would silently fail to be found by the other.
func (s *Server) Handler() http.Handler { return s.handler }
