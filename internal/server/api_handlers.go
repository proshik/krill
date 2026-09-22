package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/proshik/krill/internal/api"
)

// errAPIIdentityMissing is a defensive "this should never happen" error: every
// route under this file sits behind RequireAPIToken, which always stashes an
// Identity before calling the handler. Handling the missing case anyway (as
// an internal error, not a 4xx) means a future routing mistake that skips the
// middleware fails loudly instead of silently, e.g., zero-valuing the caller.
var errAPIIdentityMissing = errors.New("api: identity missing from request context")

// writeAPIJSON writes v as a 200 JSON response. Error responses go through
// writeAPIError instead, which sets its own status.
func writeAPIJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// decodeAPIBody decodes r.Body as JSON into v. An empty body is treated as
// "no fields set" rather than an error — several write routes (reload, stop,
// rebuild, and deploy without a tag) have nothing required in the body, and
// an agent calling them with no body at all is not malformed input. A
// non-empty body that fails to parse is reported as api.Invalid, never a
// 500: a client sending broken JSON is the client's mistake, not a server
// fault.
func decodeAPIBody(r *http.Request, v any) error {
	if r.Body == nil {
		return nil
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return api.Invalid(fmt.Sprintf("invalid JSON body: %v", err))
	}
	return nil
}

// apiVerbs are the recognized trailing-segment verbs on /api/v1/apps/*.
var apiVerbs = map[string]bool{
	"logs":        true,
	"env":         true,
	"deployments": true,
	"deploy":      true,
	"rebuild":     true,
	"reload":      true,
	"stop":        true,
}

// SplitAppRef splits the wildcard remainder of an /api/v1/apps/* request into
// an application reference and an optional trailing verb.
//
// The reference itself may contain slashes — it is either a numeric id
// ("17") or a "project/environment/app" path — so chi's router cannot carve
// it out as an ordinary path parameter; the whole tail after "/apps/" is
// captured as one wildcard and split here instead.
//
// The split is anchored on the LAST segment: when the remainder has more
// than one segment and that last segment is a recognized verb (see
// apiVerbs), it is peeled off as the verb and everything before it (still
// joined by "/") is the reference. In every other case there is no verb and
// the whole remainder is the reference — including a single segment that
// happens to spell a verb (SplitAppRef("deploy") == ("deploy", "")): a verb
// only ever qualifies a PRECEDING reference, so with nothing before it there
// is nothing to peel off, and the segment is read as an app reference
// instead. That keeps the function total — every input, including "",
// yields a defined pair rather than a case the caller has to guess at.
//
// One accepted consequence of anchoring on the last segment: an application
// whose path-form reference ends in a segment that is itself a verb word
// (e.g. an app literally named "deploy" inside acme/production) cannot be
// addressed by its bare status route, and the two HTTP methods fail
// differently on the exact same string:
//   - GET .../acme/production/deploy parses as ref="acme/production",
//     verb="deploy" (see doc comment above), but there is no GET+"deploy"
//     combination among the twelve routes (apiAppRouter's switch only
//     recognizes GET with "", "logs", "env" or "deployments") — so it 404s
//     in the router itself, indistinguishable from a route that was never
//     registered, and never reaches resolveApp at all.
//   - POST .../acme/production/deploy parses the same way, but POST+"deploy"
//     IS a registered combination, so it dispatches to apiDeploy. There
//     resolveApp rejects the truncated 2-segment ref "acme/production" as
//     Invalid (400) — a real error, just from a different layer and for a
//     different reason than the GET case.
//
// Either way the app is only unambiguously reachable by numeric id.
// Reserving verb words as a disallowed app name is a separate concern
// outside this task's scope.
//
// A trailing slash is trimmed before splitting, so ".../17/logs/" behaves
// exactly like ".../17/logs".
func SplitAppRef(rest string) (ref, verb string) {
	rest = strings.TrimRight(rest, "/")
	if rest == "" {
		return "", ""
	}
	idx := strings.LastIndex(rest, "/")
	if idx < 0 {
		// A single segment is never treated as a bare verb — see doc comment.
		return rest, ""
	}
	last := rest[idx+1:]
	if apiVerbs[last] {
		return rest[:idx], last
	}
	return rest, ""
}

// apiWhoami reports the caller's own resolved identity.
func (s *Server) apiWhoami(w http.ResponseWriter, r *http.Request) {
	ident, ok := api.IdentityFrom(r.Context())
	if !ok {
		writeAPIError(w, r, errAPIIdentityMissing)
		return
	}
	res, err := s.apiSvc.Whoami(r.Context(), ident)
	if err != nil {
		writeAPIError(w, r, err)
		return
	}
	writeAPIJSON(w, res)
}

// apiListApps lists every application in the caller's organization.
func (s *Server) apiListApps(w http.ResponseWriter, r *http.Request) {
	ident, ok := api.IdentityFrom(r.Context())
	if !ok {
		writeAPIError(w, r, errAPIIdentityMissing)
		return
	}
	apps, err := s.apiSvc.ListApps(r.Context(), ident)
	if err != nil {
		writeAPIError(w, r, err)
		return
	}
	writeAPIJSON(w, apps)
}

// apiAppRouter is the single chi handler mounted on /api/v1/apps/* (both GET
// and POST). It splits the wildcard remainder via SplitAppRef and dispatches
// on (method, verb) onto one of the nine app-scoped operations. Any
// combination that isn't one of those nine — including a recognized verb
// paired with the wrong HTTP method — 404s exactly like a route that was
// never registered; there is nothing here to disclose either way.
func (s *Server) apiAppRouter(w http.ResponseWriter, r *http.Request) {
	ref, verb := SplitAppRef(chi.URLParam(r, "*"))

	switch {
	case r.Method == http.MethodGet && verb == "":
		s.apiAppStatus(w, r, ref)
	case r.Method == http.MethodGet && verb == "logs":
		s.apiAppLogs(w, r, ref)
	case r.Method == http.MethodGet && verb == "env":
		s.apiListEnv(w, r, ref)
	case r.Method == http.MethodGet && verb == "deployments":
		s.apiListDeployments(w, r, ref)
	case r.Method == http.MethodPost && verb == "deploy":
		s.apiDeploy(w, r, ref)
	case r.Method == http.MethodPost && verb == "rebuild":
		s.apiRebuild(w, r, ref)
	case r.Method == http.MethodPost && verb == "reload":
		s.apiReload(w, r, ref)
	case r.Method == http.MethodPost && verb == "stop":
		s.apiStop(w, r, ref)
	case r.Method == http.MethodPost && verb == "env":
		s.apiSetEnv(w, r, ref)
	default:
		http.NotFound(w, r)
	}
}

// apiAppStatus reports one application's live status.
func (s *Server) apiAppStatus(w http.ResponseWriter, r *http.Request, ref string) {
	ident, ok := api.IdentityFrom(r.Context())
	if !ok {
		writeAPIError(w, r, errAPIIdentityMissing)
		return
	}
	status, err := s.apiSvc.AppStatus(r.Context(), ident, ref)
	if err != nil {
		writeAPIError(w, r, err)
		return
	}
	writeAPIJSON(w, status)
}

// apiAppLogs tails an application's runtime log. Query parameters: tail
// (line count, clamped server-side) and level (minimum severity filter).
// Both are parsed defensively — a non-numeric tail is api.Invalid, not a
// panic or a silently-ignored value.
func (s *Server) apiAppLogs(w http.ResponseWriter, r *http.Request, ref string) {
	ident, ok := api.IdentityFrom(r.Context())
	if !ok {
		writeAPIError(w, r, errAPIIdentityMissing)
		return
	}
	tail := 0
	if v := r.URL.Query().Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeAPIError(w, r, api.Invalid(fmt.Sprintf("tail must be a number, got %q", v)))
			return
		}
		tail = n
	}
	level := r.URL.Query().Get("level")
	lines, err := s.apiSvc.AppLogs(r.Context(), ident, ref, tail, level)
	if err != nil {
		writeAPIError(w, r, err)
		return
	}
	writeAPIJSON(w, lines)
}

// apiListEnv lists an application's environment variable names — never values.
func (s *Server) apiListEnv(w http.ResponseWriter, r *http.Request, ref string) {
	ident, ok := api.IdentityFrom(r.Context())
	if !ok {
		writeAPIError(w, r, errAPIIdentityMissing)
		return
	}
	keys, err := s.apiSvc.ListEnv(r.Context(), ident, ref)
	if err != nil {
		writeAPIError(w, r, err)
		return
	}
	writeAPIJSON(w, keys)
}

// apiListDeployments lists an application's deployment history. Query
// parameter: limit (row count, clamped server-side), parsed defensively.
func (s *Server) apiListDeployments(w http.ResponseWriter, r *http.Request, ref string) {
	ident, ok := api.IdentityFrom(r.Context())
	if !ok {
		writeAPIError(w, r, errAPIIdentityMissing)
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeAPIError(w, r, api.Invalid(fmt.Sprintf("limit must be a number, got %q", v)))
			return
		}
		limit = n
	}
	deps, err := s.apiSvc.ListDeployments(r.Context(), ident, ref, limit)
	if err != nil {
		writeAPIError(w, r, err)
		return
	}
	writeAPIJSON(w, deps)
}

// apiDeploymentStatus reports one deployment's status and bounded build log.
// A deployment is addressed directly by numeric id (no app in the path), so
// unlike the /apps/* routes it is registered as an ordinary chi path param.
func (s *Server) apiDeploymentStatus(w http.ResponseWriter, r *http.Request) {
	ident, ok := api.IdentityFrom(r.Context())
	if !ok {
		writeAPIError(w, r, errAPIIdentityMissing)
		return
	}
	raw := chi.URLParam(r, "deployID")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeAPIError(w, r, api.Invalid(fmt.Sprintf("deployment id must be a number, got %q", raw)))
		return
	}
	detail, err := s.apiSvc.DeploymentStatus(r.Context(), ident, id)
	if err != nil {
		writeAPIError(w, r, err)
		return
	}
	writeAPIJSON(w, detail)
}

// apiDeployRequest is krill_deploy's optional JSON body: retag an image app
// before deploying. Empty/absent means "deploy as-is".
type apiDeployRequest struct {
	Tag string `json:"tag"`
}

// apiDeploy triggers a new deployment, optionally retagging an image app first.
func (s *Server) apiDeploy(w http.ResponseWriter, r *http.Request, ref string) {
	ident, ok := api.IdentityFrom(r.Context())
	if !ok {
		writeAPIError(w, r, errAPIIdentityMissing)
		return
	}
	var req apiDeployRequest
	if err := decodeAPIBody(r, &req); err != nil {
		writeAPIError(w, r, err)
		return
	}
	res, err := s.apiSvc.Deploy(r.Context(), ident, ref, req.Tag)
	if err != nil {
		writeAPIError(w, r, err)
		return
	}
	logFrom(r).Info("api deploy requested", "app_ref", ref, "deployment_id", res.DeploymentID, "token_id", ident.TokenID, "user_id", ident.UserID)
	writeAPIJSON(w, res)
}

// apiRebuild forces a from-scratch build of a dockerfile app.
func (s *Server) apiRebuild(w http.ResponseWriter, r *http.Request, ref string) {
	ident, ok := api.IdentityFrom(r.Context())
	if !ok {
		writeAPIError(w, r, errAPIIdentityMissing)
		return
	}
	res, err := s.apiSvc.Rebuild(r.Context(), ident, ref)
	if err != nil {
		writeAPIError(w, r, err)
		return
	}
	logFrom(r).Info("api rebuild requested", "app_ref", ref, "deployment_id", res.DeploymentID, "token_id", ident.TokenID, "user_id", ident.UserID)
	writeAPIJSON(w, res)
}

// apiReload force-restarts the application's current tasks in place, or
// deploys it when it is stopped.
func (s *Server) apiReload(w http.ResponseWriter, r *http.Request, ref string) {
	ident, ok := api.IdentityFrom(r.Context())
	if !ok {
		writeAPIError(w, r, errAPIIdentityMissing)
		return
	}
	res, err := s.apiSvc.Reload(r.Context(), ident, ref)
	if err != nil {
		writeAPIError(w, r, err)
		return
	}
	logFrom(r).Info("api reload requested", "app_ref", ref, "action", res.Action, "deployment_id", res.DeploymentID, "token_id", ident.TokenID, "user_id", ident.UserID)
	writeAPIJSON(w, res)
}

// apiStop scales the application's service to zero replicas.
func (s *Server) apiStop(w http.ResponseWriter, r *http.Request, ref string) {
	ident, ok := api.IdentityFrom(r.Context())
	if !ok {
		writeAPIError(w, r, errAPIIdentityMissing)
		return
	}
	if err := s.apiSvc.Stop(r.Context(), ident, ref); err != nil {
		writeAPIError(w, r, err)
		return
	}
	logFrom(r).Info("api stop requested", "app_ref", ref, "token_id", ident.TokenID, "user_id", ident.UserID)
	writeAPIJSON(w, map[string]string{"status": "ok"})
}

// apiSetEnvRequest is krill_set_env's JSON body: set (or add) key=value, or
// remove key entirely when remove is true (value is then ignored).
type apiSetEnvRequest struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Remove bool   `json:"remove"`
}

// apiSetEnv makes a single targeted edit to an application's environment.
// The edit is saved but NOT applied to the running container — env vars are
// baked into the Swarm service spec at deploy time, so the change takes
// effect on the next deployment (see api.Service.SetEnv for why this does not
// redeploy by itself).
// The audit line deliberately omits the value: it may be a secret, and
// CLAUDE.md's logging rule ("never log secrets... connection strings")
// applies here exactly as it does to the web UI's own env editor.
func (s *Server) apiSetEnv(w http.ResponseWriter, r *http.Request, ref string) {
	ident, ok := api.IdentityFrom(r.Context())
	if !ok {
		writeAPIError(w, r, errAPIIdentityMissing)
		return
	}
	var req apiSetEnvRequest
	if err := decodeAPIBody(r, &req); err != nil {
		writeAPIError(w, r, err)
		return
	}
	if err := s.apiSvc.SetEnv(r.Context(), ident, ref, req.Key, req.Value, req.Remove); err != nil {
		writeAPIError(w, r, err)
		return
	}
	logFrom(r).Info("api set env requested", "app_ref", ref, "key", req.Key, "remove", req.Remove, "token_id", ident.TokenID, "user_id", ident.UserID)
	writeAPIJSON(w, map[string]string{"status": "ok"})
}
