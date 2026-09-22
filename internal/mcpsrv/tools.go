package mcpsrv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/proshik/krill/internal/api"
)

// Tool names. This is the entire security surface of the MCP adapter: an
// agent can call exactly what is registered in toolRegistrations (below) and
// nothing else.
//
// There is deliberately no tool for uploading an image, even though krill-cli
// has a delivery mode for it. Three reasons, in order of weight: a model has
// no image bytes to send and no business having them; MCP has no binary
// channel, so hundreds of megabytes would have to travel base64-encoded
// inside a JSON-RPC string and be materialized in memory twice; and it would
// be the first tool that puts caller-chosen code on the host, at an
// autonomous agent's discretion. Deploying a tag that already exists in a
// registry covers what an agent legitimately needs. Do not add a thirteenth
// tool to make the surface symmetrical with the CLI's.
const (
	toolWhoami           = "krill_whoami"
	toolListApps         = "krill_list_apps"
	toolAppStatus        = "krill_app_status"
	toolAppLogs          = "krill_app_logs"
	toolListEnv          = "krill_list_env"
	toolDeployments      = "krill_deployments"
	toolDeploymentStatus = "krill_deployment_status"
	toolDeploy           = "krill_deploy"
	toolRebuild          = "krill_rebuild"
	toolReload           = "krill_reload"
	toolStop             = "krill_stop"
	toolSetEnv           = "krill_set_env"
)

// --- Argument shapes -------------------------------------------------------
//
// Field names are chosen to match the REST adapter's JSON request bodies
// exactly (apiDeployRequest.Tag and apiSetEnvRequest.{Key,Value,Remove} in
// internal/server/api_handlers.go): two adapters fronting the same twelve
// operations that disagreed on field names would be a bug waiting to happen.

type appRefArgs struct {
	App string `json:"app" jsonschema:"application reference: numeric id or project/environment/app path"`
}

type deployArgs struct {
	App string `json:"app" jsonschema:"application reference: numeric id or project/environment/app path"`
	Tag string `json:"tag,omitempty" jsonschema:"new image tag; image apps only"`
}

type logsArgs struct {
	App   string `json:"app"`
	Tail  int    `json:"tail,omitempty" jsonschema:"lines to return, default 200, max 1000"`
	Level string `json:"level,omitempty" jsonschema:"minimum level: trace, debug, info, warn, error, fatal"`
}

type deploymentsArgs struct {
	App   string `json:"app" jsonschema:"application reference: numeric id or project/environment/app path"`
	Limit int    `json:"limit,omitempty" jsonschema:"rows to return, default 20, max 50"`
}

type deploymentArgs struct {
	DeploymentID int64 `json:"deployment_id"`
}

type setEnvArgs struct {
	App    string `json:"app"`
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Remove bool   `json:"remove,omitempty" jsonschema:"delete the variable instead of setting it"`
}

type emptyArgs struct{}

// --- Registry ----------------------------------------------------------

// toolRegistration binds one tool's name to the closure that registers it on
// a *mcp.Server. Keeping the name alongside its registration func (rather
// than deriving names by inspecting the built *mcp.Server afterward — the
// SDK exposes no such introspection) means ToolNames() and the actual
// mcp.AddTool calls in registerTools always agree, by construction: there is
// no second, separately hand-maintained list that could drift out of sync
// with what actually gets registered.
type toolRegistration struct {
	name     string
	register func(*mcp.Server, *api.Service)
}

var toolRegistrations = []toolRegistration{
	{toolWhoami, registerWhoami},
	{toolListApps, registerListApps},
	{toolAppStatus, registerAppStatus},
	{toolAppLogs, registerAppLogs},
	{toolListEnv, registerListEnv},
	{toolDeployments, registerDeployments},
	{toolDeploymentStatus, registerDeploymentStatus},
	{toolDeploy, registerDeploy},
	{toolRebuild, registerRebuild},
	{toolReload, registerReload},
	{toolStop, registerStop},
	{toolSetEnv, registerSetEnv},
}

// ToolNames returns the exact set of tool names this adapter registers.
// Tests sort the result before comparing, so registration order stays free
// to change without breaking the registry-drift check.
func ToolNames() []string {
	names := make([]string, len(toolRegistrations))
	for i, t := range toolRegistrations {
		names[i] = t.name
	}
	return names
}

// registerTools adds every tool in toolRegistrations to srv, bound to svc.
// Called exactly once, from New.
func registerTools(srv *mcp.Server, svc *api.Service) {
	for _, t := range toolRegistrations {
		t.register(srv, svc)
	}
}

// --- Identity + error mapping ------------------------------------------

// callerIdentity returns the Identity of the HTTP request that carried THIS
// tools/call, taken from req.Extra.TokenInfo (see bindSession in server.go),
// plus ctx with that identity and that request's id stamped on it.
//
// It must not read the identity from ctx: in a stateful session the SDK runs
// every handler on the context of the request that opened the session
// (initialize), so ctx still carries the identity and request id resolved
// back then — a later demotion would never be seen, and neither would a
// different token presented with the same Mcp-Session-Id.
//
// A miss should never happen in practice, since /mcp is always mounted behind
// RequireAPIToken; treating it as an internal error rather than silently
// zero-valuing the caller matches errAPIIdentityMissing's posture in the REST
// adapter.
func callerIdentity(ctx context.Context, req *mcp.CallToolRequest) (context.Context, api.Identity, *mcp.CallToolResult) {
	if req != nil && req.Extra != nil && req.Extra.TokenInfo != nil {
		ti := req.Extra.TokenInfo
		if id, ok := ti.Extra[extraIdentity].(api.Identity); ok {
			reqID, _ := ti.Extra[extraRequestID].(string)
			ctx = context.WithValue(api.WithIdentity(ctx, id), middleware.RequestIDKey, reqID)
			return ctx, id, nil
		}
	}
	reqID := middleware.GetReqID(ctx)
	slog.Error("mcp: identity missing from the request; a tool ran outside RequireAPIToken", "request_id", reqID)
	return ctx, api.Identity{}, errorResult(fmt.Sprintf("internal error, see server logs (request_id=%s)", reqID))
}

// errorResult builds an isError CallToolResult carrying msg as its only
// content. msg must already be caller-safe: see toolError, the only caller
// that should ever originate one from an arbitrary error value.
func errorResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

// toolError maps a service-layer error onto a CallToolResult exactly the way
// the REST adapter's writeAPIError maps one onto an HTTP response
// (internal/server/api_middleware.go): an *api.Error's Message was authored
// to be shown to a caller, so it passes through untouched; anything else is
// an internal failure whose real cause is logged server-side and never
// handed to the client as raw err.Error() (which can carry a DSN, a host
// path, or other server-internal detail) — the caller gets a generic message
// plus a request id to correlate against the server log, the same shape
// writeAPIError's JSON body uses.
func toolError(ctx context.Context, err error) *mcp.CallToolResult {
	var aerr *api.Error
	if errors.As(err, &aerr) {
		// Prefix the code: the REST adapter returns {code,message} and an agent
		// branching on "not_found" versus "invalid" needs the same signal here,
		// where the only channel is text.
		return errorResult(string(aerr.Code) + ": " + aerr.Message)
	}
	reqID := middleware.GetReqID(ctx)
	slog.Error("mcp internal error", "err", err, "request_id", reqID)
	return errorResult(fmt.Sprintf("internal error, see server logs (request_id=%s)", reqID))
}

// jsonResult marshals v (one of internal/api's own exported result types)
// into a single TextContent block. Marshaling one of those well-known,
// exported structs should never fail; if it somehow does, the failure itself
// is routed through toolError rather than surfacing a marshal error whose
// text might quote a Go type/field name back at the caller.
func jsonResult(ctx context.Context, v any) *mcp.CallToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return toolError(ctx, fmt.Errorf("marshal tool result: %w", err))
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}
}

// okResult is the fixed success body for a write operation that has nothing
// of its own to report (reload, stop, set_env) — mirrors the REST adapter's
// {"status":"ok"} response for the same three operations.
var okResult = map[string]string{"status": "ok"}

// --- Tool registrations --------------------------------------------------
//
// Every handler follows the same shape: resolve the caller's identity, call
// straight through to the one internal/api.Service method that does the
// actual tenancy/permission checking and work, and map the result or the
// error. No tenancy or permission logic lives here — that is the whole point
// of internal/api.Service existing as a shared layer under both adapters.
//
// Handlers never return a non-nil Go error from the ToolHandlerFor callback:
// the SDK's AddTool wrapper packs a returned error into the result via
// err.Error() (see toolForErr in the SDK's server.go), which would bypass
// toolError's leak guard entirely. Every failure path here builds its own
// CallToolResult instead and returns a nil error.

func registerWhoami(srv *mcp.Server, svc *api.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolWhoami,
		Description: "Report the caller's own resolved identity: user, organization, and access level.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ emptyArgs) (*mcp.CallToolResult, any, error) {
		ctx, id, errRes := callerIdentity(ctx, req)
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := svc.Whoami(ctx, id)
		if err != nil {
			return toolError(ctx, err), nil, nil
		}
		return jsonResult(ctx, res), nil, nil
	})
}

func registerListApps(srv *mcp.Server, svc *api.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolListApps,
		Description: "List every application in the caller's organization: path, id, status, image/source type, domains.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ emptyArgs) (*mcp.CallToolResult, any, error) {
		ctx, id, errRes := callerIdentity(ctx, req)
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := svc.ListApps(ctx, id)
		if err != nil {
			return toolError(ctx, err), nil, nil
		}
		return jsonResult(ctx, res), nil, nil
	})
}

func registerAppStatus(srv *mcp.Server, svc *api.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolAppStatus,
		Description: "Report one application's live status: running/desired replicas, node, image, domains, and its most recent deployment.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in appRefArgs) (*mcp.CallToolResult, any, error) {
		ctx, id, errRes := callerIdentity(ctx, req)
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := svc.AppStatus(ctx, id, in.App)
		if err != nil {
			return toolError(ctx, err), nil, nil
		}
		return jsonResult(ctx, res), nil, nil
	})
}

func registerAppLogs(srv *mcp.Server, svc *api.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolAppLogs,
		Description: "Tail an application's live runtime log, parsed into structured lines and optionally filtered to a minimum severity.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in logsArgs) (*mcp.CallToolResult, any, error) {
		ctx, id, errRes := callerIdentity(ctx, req)
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := svc.AppLogs(ctx, id, in.App, in.Tail, in.Level)
		if err != nil {
			return toolError(ctx, err), nil, nil
		}
		return jsonResult(ctx, res), nil, nil
	})
}

func registerListEnv(srv *mcp.Server, svc *api.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolListEnv,
		Description: "List an application's environment variable names and source (literal or db-link) — values are never returned.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in appRefArgs) (*mcp.CallToolResult, any, error) {
		ctx, id, errRes := callerIdentity(ctx, req)
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := svc.ListEnv(ctx, id, in.App)
		if err != nil {
			return toolError(ctx, err), nil, nil
		}
		return jsonResult(ctx, res), nil, nil
	})
}

func registerDeployments(srv *mcp.Server, svc *api.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolDeployments,
		Description: "List an application's deployment history, most recent first.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in deploymentsArgs) (*mcp.CallToolResult, any, error) {
		ctx, id, errRes := callerIdentity(ctx, req)
		if errRes != nil {
			return errRes, nil, nil
		}
		// limit=0 requests the service's own default window (20 rows, capped at 50).
		res, err := svc.ListDeployments(ctx, id, in.App, in.Limit)
		if err != nil {
			return toolError(ctx, err), nil, nil
		}
		return jsonResult(ctx, res), nil, nil
	})
}

func registerDeploymentStatus(srv *mcp.Server, svc *api.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolDeploymentStatus,
		Description: "Report one deployment's status and its bounded build log tail.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in deploymentArgs) (*mcp.CallToolResult, any, error) {
		ctx, id, errRes := callerIdentity(ctx, req)
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := svc.DeploymentStatus(ctx, id, in.DeploymentID)
		if err != nil {
			return toolError(ctx, err), nil, nil
		}
		return jsonResult(ctx, res), nil, nil
	})
}

func registerDeploy(srv *mcp.Server, svc *api.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolDeploy,
		Description: "Trigger a new deployment. Optionally retags an image app first (image apps only). Asynchronous: enqueues and returns immediately with a deployment id — poll krill_deployment_status.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in deployArgs) (*mcp.CallToolResult, any, error) {
		ctx, id, errRes := callerIdentity(ctx, req)
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := svc.Deploy(ctx, id, in.App, in.Tag)
		if err != nil {
			return toolError(ctx, err), nil, nil
		}
		slog.InfoContext(ctx, "mcp deploy requested", "app_ref", in.App, "deployment_id", res.DeploymentID, "token_id", id.TokenID, "user_id", id.UserID, "request_id", middleware.GetReqID(ctx))
		return jsonResult(ctx, res), nil, nil
	})
}

func registerRebuild(srv *mcp.Server, svc *api.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolRebuild,
		Description: "Force a from-scratch build (--no-cache) of a dockerfile app. Asynchronous: enqueues and returns immediately with a deployment id — poll krill_deployment_status.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in appRefArgs) (*mcp.CallToolResult, any, error) {
		ctx, id, errRes := callerIdentity(ctx, req)
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := svc.Rebuild(ctx, id, in.App)
		if err != nil {
			return toolError(ctx, err), nil, nil
		}
		slog.InfoContext(ctx, "mcp rebuild requested", "app_ref", in.App, "deployment_id", res.DeploymentID, "token_id", id.TokenID, "user_id", id.UserID, "request_id", middleware.GetReqID(ctx))
		return jsonResult(ctx, res), nil, nil
	})
}

func registerReload(srv *mcp.Server, svc *api.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolReload,
		Description: "Force-restart the application's current tasks in place — same image, same config, no rebuild and no pull. A stopped app is deployed instead (action \"deploy\", with a deployment id — poll krill_deployment_status).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in appRefArgs) (*mcp.CallToolResult, any, error) {
		ctx, id, errRes := callerIdentity(ctx, req)
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := svc.Reload(ctx, id, in.App)
		if err != nil {
			return toolError(ctx, err), nil, nil
		}
		slog.InfoContext(ctx, "mcp reload requested", "app_ref", in.App, "action", res.Action, "deployment_id", res.DeploymentID, "token_id", id.TokenID, "user_id", id.UserID, "request_id", middleware.GetReqID(ctx))
		return jsonResult(ctx, res), nil, nil
	})
}

func registerStop(srv *mcp.Server, svc *api.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolStop,
		Description: "Scale the application's service to zero replicas. krill_deploy brings it back up.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in appRefArgs) (*mcp.CallToolResult, any, error) {
		ctx, id, errRes := callerIdentity(ctx, req)
		if errRes != nil {
			return errRes, nil, nil
		}
		if err := svc.Stop(ctx, id, in.App); err != nil {
			return toolError(ctx, err), nil, nil
		}
		slog.InfoContext(ctx, "mcp stop requested", "app_ref", in.App, "token_id", id.TokenID, "user_id", id.UserID, "request_id", middleware.GetReqID(ctx))
		return jsonResult(ctx, okResult), nil, nil
	})
}

func registerSetEnv(srv *mcp.Server, svc *api.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolSetEnv,
		Description: "Set (or add) one environment variable's value, or remove it, without touching any other line of the app's env file. The change is saved but NOT applied to the running container: call krill_deploy afterwards for it to take effect. Values must be a single line.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in setEnvArgs) (*mcp.CallToolResult, any, error) {
		ctx, id, errRes := callerIdentity(ctx, req)
		if errRes != nil {
			return errRes, nil, nil
		}
		if err := svc.SetEnv(ctx, id, in.App, in.Key, in.Value, in.Remove); err != nil {
			return toolError(ctx, err), nil, nil
		}
		// The audit line omits the value, which may be a secret — the same
		// rule CLAUDE.md states for the web UI's env editor, and that the
		// REST adapter's apiSetEnv already follows.
		slog.InfoContext(ctx, "mcp set env requested", "app_ref", in.App, "key", in.Key, "remove", in.Remove, "token_id", id.TokenID, "user_id", id.UserID, "request_id", middleware.GetReqID(ctx))
		return jsonResult(ctx, okResult), nil, nil
	})
}
