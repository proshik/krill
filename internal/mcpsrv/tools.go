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

	// Provisioning: create, never delete; provision-level tokens only.
	toolCreateDatabase = "krill_create_database"
	toolCreateApp      = "krill_create_app"
	toolAddDomain      = "krill_add_domain"
	toolLinkDatabase   = "krill_link_database"

	// Diagnostics added for agents that used to fall back to ssh.
	toolAppConfig    = "krill_app_config"
	toolListBackups  = "krill_list_backups"
	toolBackupNow    = "krill_backup_now"
	toolDomainStatus = "krill_domain_status"
)

// --- Argument shapes -------------------------------------------------------
//
// Field names are chosen to match the REST adapter's JSON request bodies
// exactly (apiDeployRequest.Tag and apiSetEnvRequest.{Key,Value,Remove} in
// internal/server/api_handlers.go): two adapters fronting the same
// operations that disagreed on field names would be a bug waiting to happen.

type appRefArgs struct {
	App string `json:"app" jsonschema:"application reference: numeric id or project/environment/app path"`
}

type deployArgs struct {
	App string `json:"app" jsonschema:"application reference: numeric id or project/environment/app path"`
	Tag string `json:"tag,omitempty" jsonschema:"new image tag; image apps only"`
}

type logsArgs struct {
	App      string `json:"app"`
	Tail     int    `json:"tail,omitempty" jsonschema:"lines to return, default 200, max 1000; with wait_for: lines before the match, default 20, max 200"`
	Level    string `json:"level,omitempty" jsonschema:"minimum level: trace, debug, info, warn, error, fatal. A line's level is detected from a level= field or a severity word near the start of the line; a line with no detected level is always kept, since it cannot be ranked"`
	Since    string `json:"since,omitempty" jsonschema:"start of the window: an RFC3339 time, or a duration ago such as 10m or 2h"`
	Until    string `json:"until,omitempty" jsonschema:"end of the window, same forms as since; needs since"`
	Contains string `json:"contains,omitempty" jsonschema:"keep only lines whose message contains this text, case-insensitive"`
	WaitFor  string `json:"wait_for,omitempty" jsonschema:"wait until a line containing this text (case-insensitive) is written AFTER the call, and return whether it was found with the lines around it and following_since, the moment from which lines were watched; earlier lines are not searched (read them with since and contains); not combinable with since, until, contains or level"`
	Timeout  int    `json:"timeout,omitempty" jsonschema:"seconds wait_for waits, default 60, max 120"`
}

type backupsArgs struct {
	App         string `json:"app,omitempty" jsonschema:"only the backups of this app's volumes and of the databases and buckets it is linked to: numeric id or project/environment/app path"`
	Environment string `json:"environment,omitempty" jsonschema:"only the backups of this project/environment"`
}

type backupNowArgs struct {
	Kind string `json:"kind" jsonschema:"database, volume or bucket, as krill_list_backups names it"`
	ID   int64  `json:"id" jsonschema:"the backup's id from krill_list_backups"`
}

type deploymentsArgs struct {
	App   string `json:"app" jsonschema:"application reference: numeric id or project/environment/app path"`
	Limit int    `json:"limit,omitempty" jsonschema:"rows to return, default 20, max 50"`
}

type deploymentArgs struct {
	DeploymentID int64 `json:"deployment_id"`
	Wait         int   `json:"wait,omitempty" jsonschema:"seconds to wait while the deployment is still running before answering, max 120; 0 answers at once"`
}

type setEnvArgs struct {
	App    string `json:"app"`
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Remove bool   `json:"remove,omitempty" jsonschema:"delete the variable instead of setting it"`
}

type emptyArgs struct{}

// The provisioning arguments embed the REST request bodies (api.*Input), so
// both adapters accept exactly the same fields.

type createDatabaseArgs struct {
	Environment string `json:"environment" jsonschema:"project/environment path"`
	api.CreateDatabaseInput
}

type createAppArgs struct {
	Environment string `json:"environment" jsonschema:"project/environment path"`
	api.CreateAppInput
}

type addDomainArgs struct {
	App string `json:"app" jsonschema:"application reference: numeric id or project/environment/app path"`
	api.DomainInput
}

type linkDatabaseArgs struct {
	App string `json:"app" jsonschema:"application reference: numeric id or project/environment/app path"`
	api.LinkDatabaseInput
}

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
	{toolCreateDatabase, registerCreateDatabase},
	{toolCreateApp, registerCreateApp},
	{toolAddDomain, registerAddDomain},
	{toolLinkDatabase, registerLinkDatabase},
	{toolAppConfig, registerAppConfig},
	{toolListBackups, registerListBackups},
	{toolBackupNow, registerBackupNow},
	{toolDomainStatus, registerDomainStatus},
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

// requestScoped returns ctx ended also when the HTTP request that carried
// this call goes away. A tool handler runs on its session's context, which a
// disconnected POST does not end, so a tool that waits needs this to stop
// working for a caller that left. cancel must be called.
func requestScoped(ctx context.Context, req *mcp.CallToolRequest) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	if req == nil || req.Extra == nil || req.Extra.TokenInfo == nil {
		return ctx, cancel
	}
	done, ok := req.Extra.TokenInfo.Extra[extraRequestDone].(<-chan struct{})
	if !ok || done == nil {
		return ctx, cancel
	}
	go func() {
		select {
		case <-done:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
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
		Description: "Report the caller's own resolved identity: user, organization, access level, and which Krill installation answered (instance url and version).",
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
		Description: "Report one application's live status: running/desired replicas, node, image, domains, its most recent deployment, its recent Swarm tasks (node, state, error, exit code), how many tasks ended since the last deploy, container health and memory/CPU. The Swarm, health and usage parts are left out when they cannot be read.",
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
		Description: "Read an application's runtime log across all of its tasks, merged by time: the most recent lines, oldest first, parsed into structured lines. Optional window (since, until), case-insensitive substring filter (contains) and minimum severity (level). The level filter only drops lines whose detected level is below it; a line with no detected level (lvl \"\") is always kept, so most of an app's unstructured output passes whatever level is asked for. With wait_for it instead waits (timeout, max 120 s) for a NEW line containing that text — written after the call; earlier lines are not searched — and answers {found, stopped_by, match, lines, following_since, incomplete}. To wait for a line after a deploy without a gap: read with contains and since = the deployment's start; if absent, wait_for; on a timeout, read with contains and since again. At most a few waits run at once on an installation; a further one is refused as unavailable.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in logsArgs) (*mcp.CallToolResult, any, error) {
		ctx, id, errRes := callerIdentity(ctx, req)
		if errRes != nil {
			return errRes, nil, nil
		}
		ctx, cancel := requestScoped(ctx, req)
		defer cancel()
		res, err := svc.Logs(ctx, id, in.App, api.LogsInput{
			Tail: in.Tail, Level: in.Level, Since: in.Since, Until: in.Until,
			Contains: in.Contains, WaitFor: in.WaitFor, Timeout: in.Timeout,
		})
		if err != nil {
			return toolError(ctx, err), nil, nil
		}
		return jsonResult(ctx, res), nil, nil
	})
}

func registerListEnv(srv *mcp.Server, svc *api.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolListEnv,
		Description: "List an application's environment variable names and source (literal, db-link or bucket-link) — values are never returned.",
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
		Description: "Report one deployment's status and its bounded build log tail. With wait (seconds, max 120) it first waits while the deployment is still running and answers when it is done or failed, or when the time is up. settling=true means the spec is applied and Swarm is still converging: the deployment is still running, not done.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in deploymentArgs) (*mcp.CallToolResult, any, error) {
		ctx, id, errRes := callerIdentity(ctx, req)
		if errRes != nil {
			return errRes, nil, nil
		}
		ctx, cancel := requestScoped(ctx, req)
		defer cancel()
		res, err := svc.DeploymentStatusWait(ctx, id, in.DeploymentID, in.Wait)
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
		Description: "Scale the application's service to zero replicas. A deploy of it that has not applied its service spec yet is cancelled first. krill_deploy brings it back up.",
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
		// rule AGENTS.md states for the web UI's env editor, and that the
		// REST adapter's apiSetEnv already follows.
		slog.InfoContext(ctx, "mcp set env requested", "app_ref", in.App, "key", in.Key, "remove", in.Remove, "token_id", id.TokenID, "user_id", id.UserID, "request_id", middleware.GetReqID(ctx))
		return jsonResult(ctx, okResult), nil, nil
	})
}

// provisionNote ends every provisioning tool's description.
const provisionNote = " Needs a provision-level token. Idempotent: repeating a request that already took effect returns the existing resource with created=false; one naming an existing resource with different settings is a conflict — changes and deletions are made in the web UI."

func registerCreateDatabase(srv *mcp.Server, svc *api.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolCreateDatabase,
		Description: "Create a logical Postgres database (and its owner user) in an environment, inside an existing database server of the organization. The password is never returned; link the database to an app with krill_link_database." + provisionNote,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in createDatabaseArgs) (*mcp.CallToolResult, any, error) {
		ctx, id, errRes := callerIdentity(ctx, req)
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := svc.CreateDatabase(ctx, id, in.Environment, in.CreateDatabaseInput)
		if err != nil {
			return toolError(ctx, err), nil, nil
		}
		slog.InfoContext(ctx, "mcp create database", "env", in.Environment, "ldb_id", res.ID, "created", res.Created, "token_id", id.TokenID, "user_id", id.UserID, "request_id", middleware.GetReqID(ctx))
		return jsonResult(ctx, res), nil, nil
	})
}

func registerCreateApp(srv *mcp.Server, svc *api.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolCreateApp,
		Description: "Create an app that runs a container image, in an environment. It is NOT deployed: set its variables with krill_set_env and link databases first, then krill_deploy. Its primary domain defaults to <name>.<base domain>, internal (not exposed) and without HTTPS; pass domain to choose." + provisionNote,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in createAppArgs) (*mcp.CallToolResult, any, error) {
		ctx, id, errRes := callerIdentity(ctx, req)
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := svc.CreateApp(ctx, id, in.Environment, in.CreateAppInput)
		if err != nil {
			return toolError(ctx, err), nil, nil
		}
		slog.InfoContext(ctx, "mcp create app", "env", in.Environment, "app_id", res.ID, "created", res.Created, "token_id", id.TokenID, "user_id", id.UserID, "request_id", middleware.GetReqID(ctx))
		return jsonResult(ctx, res), nil, nil
	})
}

func registerAddDomain(srv *mcp.Server, svc *api.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolAddDomain,
		Description: "Add a domain to an app: exposed makes it reachable from the internet, tls turns on a Let's Encrypt certificate, paths limits it to those path prefixes. A host other apps already use has one HTTPS setting for all: omit tls to take it. The host's DNS must point at the cluster for HTTPS to be issued. Takes effect on the gateway at once for a deployed app." + provisionNote,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in addDomainArgs) (*mcp.CallToolResult, any, error) {
		ctx, id, errRes := callerIdentity(ctx, req)
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := svc.AddDomain(ctx, id, in.App, in.DomainInput)
		if err != nil {
			return toolError(ctx, err), nil, nil
		}
		slog.InfoContext(ctx, "mcp add domain", "app_ref", in.App, "domain_id", res.ID, "host", res.Host, "created", res.Created, "token_id", id.TokenID, "user_id", id.UserID, "request_id", middleware.GetReqID(ctx))
		return jsonResult(ctx, res), nil, nil
	})
}

func registerLinkDatabase(srv *mcp.Server, svc *api.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolLinkDatabase,
		Description: "Inject a database's connection value into one of an app's environment variables on every deploy: database names a logical database of the app's environment, server a non-Postgres server (Redis, Valkey, S3). field selects the value: default url for Postgres and Redis-compatible servers; an S3 server needs endpoint, access_key, secret_key or region. The value is never shown; deploy the app for it to take effect." + provisionNote,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in linkDatabaseArgs) (*mcp.CallToolResult, any, error) {
		ctx, id, errRes := callerIdentity(ctx, req)
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := svc.LinkDatabase(ctx, id, in.App, in.LinkDatabaseInput)
		if err != nil {
			return toolError(ctx, err), nil, nil
		}
		slog.InfoContext(ctx, "mcp link database", "app_ref", in.App, "link_id", res.ID, "var", res.Var, "created", res.Created, "token_id", id.TokenID, "user_id", id.UserID, "request_id", middleware.GetReqID(ctx))
		return jsonResult(ctx, res), nil, nil
	})
}

func registerAppConfig(srv *mcp.Server, svc *api.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolAppConfig,
		Description: "Report an application's saved settings without secrets: source (image and tag, or git repository, branch and Dockerfile path), resources and placement, healthcheck timing (not its command), ports, volumes, domains with their routing and protection kinds, database/server/bucket links by variable, and which settings sections are saved but not applied yet (pending). Never env values, commands, passwords or tokens.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in appRefArgs) (*mcp.CallToolResult, any, error) {
		ctx, id, errRes := callerIdentity(ctx, req)
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := svc.AppConfig(ctx, id, in.App)
		if err != nil {
			return toolError(ctx, err), nil, nil
		}
		return jsonResult(ctx, res), nil, nil
	})
}

func registerListBackups(srv *mcp.Server, svc *api.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolListBackups,
		Description: "List the organization's configured backups — database (pg_dump), volume and bucket — with schedule, retention, whether one is running, and the last run's time, status and error, plus the last success. Optionally only one app's or one environment's.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in backupsArgs) (*mcp.CallToolResult, any, error) {
		ctx, id, errRes := callerIdentity(ctx, req)
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := svc.ListBackups(ctx, id, api.BackupFilter{App: in.App, Environment: in.Environment})
		if err != nil {
			return toolError(ctx, err), nil, nil
		}
		return jsonResult(ctx, res), nil, nil
	})
}

func registerBackupNow(srv *mcp.Server, svc *api.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolBackupNow,
		Description: "Start an existing configured backup now, like the web UI's Back up now. Asynchronous: answers started with started_at; follow it with krill_list_backups until running is false — its result is the one whose last_run_at equals started_at (a later last_run_at is another run's and leaves this one's outcome unknown; an earlier one means it recorded nothing: treat it as failed). The run applies the backup's retention, which can delete the oldest stored copies. Needs a write-level token.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in backupNowArgs) (*mcp.CallToolResult, any, error) {
		ctx, id, errRes := callerIdentity(ctx, req)
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := svc.BackupNow(ctx, id, in.Kind, in.ID)
		if err != nil {
			return toolError(ctx, err), nil, nil
		}
		slog.InfoContext(ctx, "mcp backup now requested", "kind", in.Kind, "backup_id", in.ID, "token_id", id.TokenID, "user_id", id.UserID, "request_id", middleware.GetReqID(ctx))
		return jsonResult(ctx, res), nil, nil
	})
}

func registerDomainStatus(srv *mcp.Server, svc *api.Service) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolDomainStatus,
		Description: "Check each domain of an app: what DNS resolves it to from the control plane, whether that is Krill's gateway, and for an HTTPS domain which certificate the gateway serves for it (issuer, validity, trusted, and default_cert when it is Traefik's own self-signed one — no certificate issued yet).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in appRefArgs) (*mcp.CallToolResult, any, error) {
		ctx, id, errRes := callerIdentity(ctx, req)
		if errRes != nil {
			return errRes, nil, nil
		}
		res, err := svc.DomainStatus(ctx, id, in.App)
		if err != nil {
			return toolError(ctx, err), nil, nil
		}
		return jsonResult(ctx, res), nil, nil
	})
}
