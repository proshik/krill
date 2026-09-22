// Package mcpsrv is the MCP (Model Context Protocol) adapter over
// internal/api.Service: a streamable-HTTP MCP server exposing the same
// twelve agent-facing operations the REST adapter (internal/server's
// /api/v1) exposes, mapped onto MCP tools instead of HTTP routes. Both
// adapters share the exact same tenancy/permission logic in
// internal/api.Service; this package is deliberately thin — it only reads
// identity, decodes arguments, calls the service, and maps the result or the
// error (see tools.go).
//
// Architecture note (one shared *mcp.Server, identity per request): Server
// builds exactly one *mcp.Server at construction time (New). RequireAPIToken
// (internal/server) authenticates every HTTP request and stashes the resolved
// Identity on its context via api.WithIdentity, but a tool handler cannot read
// it from its own ctx: in a stateful session the SDK runs every handler on the
// context of the request that OPENED the session (initialize), so ctx.Value
// would return the identity of that first request for the session's whole
// life. Instead Handler lifts each request's Identity into an SDK
// auth.TokenInfo (bindSession), which the SDK hands to the handler of that very
// request as req.Extra.TokenInfo — that is where callerIdentity (tools.go)
// reads it. The same TokenInfo's UserID binds the session to the token that
// opened it, so a request presenting another token with a known
// Mcp-Session-Id is refused by the SDK ("session user mismatch", 403).
package mcpsrv

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/modelcontextprotocol/go-sdk/auth"
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
	// the caller's Identity reaches each tool through the per-request
	// TokenInfo instead (see the package doc). JSONResponse:true keeps every tools/call response a single JSON
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
	//
	// DisableLocalhostProtection turns off the SDK's DNS rebinding guard,
	// which 403s any request that arrives on a loopback address with a
	// non-loopback Host header. A reverse proxy on the same host forwarding a
	// domain to 127.0.0.1 sends exactly that, and the guard protects nothing
	// here: every request to /mcp must already carry a bearer token
	// (RequireAPIToken), which a rebound browser page does not have.
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return srv
	}, &mcp.StreamableHTTPOptions{
		JSONResponse:               true,
		SessionTimeout:             sessionTimeout,
		DisableLocalhostProtection: true,
	})
	// auth.RequireBearerToken is the only way to put a TokenInfo where the
	// SDK looks for it (its context key is unexported). It does not verify
	// anything here: RequireAPIToken already did, and bindSession only
	// repackages the Identity it resolved. A request with no Identity (a bare
	// handler in tests) goes straight to the SDK and every tool refuses it.
	bound := auth.RequireBearerToken(bindSession, &auth.RequireBearerTokenOptions{
		AllowMissingExpiration: true,
	})(sdk)
	s.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := api.IdentityFrom(r.Context()); ok {
			bound.ServeHTTP(w, r)
			return
		}
		sdk.ServeHTTP(w, r)
	})
	return s
}

// Keys of the auth.TokenInfo.Extra map bindSession fills in.
const (
	extraIdentity  = "krill.identity"
	extraRequestID = "krill.request_id"
)

// bindSession turns the Identity RequireAPIToken resolved for this request
// into the SDK's per-request TokenInfo. UserID is the token id, so a session
// belongs to the one token that initialized it: the SDK compares it on every
// later request of the session and answers 403 on a mismatch. Binding to the
// token rather than to the user is the stricter choice — a user's read token
// cannot ride the session their write token opened either.
func bindSession(ctx context.Context, _ string, _ *http.Request) (*auth.TokenInfo, error) {
	id, ok := api.IdentityFrom(ctx)
	if !ok {
		return nil, auth.ErrInvalidToken
	}
	return &auth.TokenInfo{
		UserID: "token:" + strconv.FormatInt(id.TokenID, 10),
		Extra: map[string]any{
			extraIdentity:  id,
			extraRequestID: middleware.GetReqID(ctx),
		},
	}, nil
}

// Handler returns the streamable-HTTP handler serving MCP sessions. It is
// built once in New and returned as-is on every call: a StreamableHTTPHandler
// owns the session table an incoming request's Mcp-Session-Id is looked up
// in, so mounting it at more than one route (e.g. "/mcp" and "/mcp/*") MUST
// share this exact instance — two separately constructed handlers would each
// hold their own session table, and a client's session started against one
// would silently fail to be found by the other.
func (s *Server) Handler() http.Handler { return s.handler }
