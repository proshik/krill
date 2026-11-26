package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Whoami reports who the token acts as. Called before a build so that a
// read-only token is refused in one request rather than after a build and a
// push: the server checks write level before it resolves the application, so
// there is no earlier point at which a deploy would fail.
func (c *Client) Whoami(ctx context.Context) (Whoami, error) {
	var out Whoami
	err := c.do(ctx, http.MethodGet, "/api/v1/whoami", nil, &out)
	return out, err
}

// ListApps returns every application in the token's organization.
func (c *Client) ListApps(ctx context.Context) ([]App, error) {
	var out []App
	err := c.do(ctx, http.MethodGet, "/api/v1/apps", nil, &out)
	return out, err
}

// AppStatus returns one application's live status.
func (c *Client) AppStatus(ctx context.Context, ref string) (AppStatus, error) {
	var out AppStatus
	err := c.do(ctx, http.MethodGet, appPath(ref, ""), nil, &out)
	return out, err
}

// LogsQuery is one read of the runtime log. Since and Until are an RFC3339
// time or a duration ago (10m); Contains ignores case. The server checks
// every value and is the authority on what is accepted.
type LogsQuery struct {
	Tail     int
	Level    string
	Since    string
	Until    string
	Contains string
}

// Logs returns lines of the application's runtime log: the most recent ones,
// or those of a window.
//
// There is no follow mode, and there cannot be one here: live streaming is a
// WebSocket the server authenticates with a session cookie, which a token
// client does not have. WaitLog waits for one line instead.
//
// A server older than the agent API diagnostics ignores since, until and
// contains and answers with the plain tail — a wrong answer that looks like
// a right one. So when any of them is asked for, the server is asked first
// whether it is new enough (one whoami request).
func (c *Client) Logs(ctx context.Context, ref string, lq LogsQuery) ([]LogLine, error) {
	if lq.Since != "" || lq.Until != "" || lq.Contains != "" {
		if err := c.requireDiagnostics(ctx, "log windows and search (--since, --until, --contains)"); err != nil {
			return nil, err
		}
	}
	q := url.Values{}
	if lq.Tail > 0 {
		q.Set("tail", strconv.Itoa(lq.Tail))
	}
	setNonEmpty(q, "level", lq.Level)
	setNonEmpty(q, "since", lq.Since)
	setNonEmpty(q, "until", lq.Until)
	setNonEmpty(q, "contains", lq.Contains)
	var out []LogLine
	err := c.do(ctx, http.MethodGet, withQuery(appPath(ref, "logs"), q), nil, &out)
	return out, err
}

// WaitLog waits, on the server, for a NEW log line containing text (ignoring
// case), for at most timeout (0: the server's default of 60 s; it allows 120
// s). tail is how many lines before the match come back (0: the server's
// default).
//
// The server holds the request open, so this runs on the long client. The
// server is first asked whether it is new enough (one whoami request); an
// answer shaped as an array of lines is reported as an old server as well,
// never decoded as "not found".
func (c *Client) WaitLog(ctx context.Context, ref, text string, tail int, timeout time.Duration) (LogWaitResult, error) {
	// An older server would not wait at all: it reads an ordinary tail, and
	// when that read fails (no such service yet, Docker unreachable) it
	// answers 500 before the shape check below can tell. Ask first.
	if err := c.requireDiagnostics(ctx, "waiting for a log line (--wait-for)"); err != nil {
		return LogWaitResult{}, err
	}
	q := url.Values{}
	q.Set("wait_for", text)
	if tail > 0 {
		q.Set("tail", strconv.Itoa(tail))
	}
	wait := 60 * time.Second
	if timeout > 0 {
		q.Set("timeout", strconv.Itoa(ceilSeconds(timeout)))
		wait = timeout
	}
	var raw json.RawMessage
	if err := c.doLong(ctx, wait, http.MethodGet, withQuery(appPath(ref, "logs"), q), nil, &raw); err != nil {
		return LogWaitResult{}, err
	}
	if b := bytes.TrimSpace(raw); len(b) > 0 && b[0] == '[' {
		return LogWaitResult{}, &OldServerError{Op: "waiting for a log line (--wait-for)"}
	}
	var out LogWaitResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return LogWaitResult{}, fmt.Errorf("server sent a log wait answer this client could not read: %w", err)
	}
	return out, nil
}

// Deployments returns the deploy history, newest first.
func (c *Client) Deployments(ctx context.Context, ref string, limit int) ([]Deployment, error) {
	p := appPath(ref, "deployments")
	if limit > 0 {
		p += "?limit=" + strconv.Itoa(limit)
	}
	var out []Deployment
	err := c.do(ctx, http.MethodGet, p, nil, &out)
	return out, err
}

// Deployment returns one deployment plus the tail of its log.
func (c *Client) Deployment(ctx context.Context, id int64) (DeploymentDetail, error) {
	var out DeploymentDetail
	err := c.do(ctx, http.MethodGet, "/api/v1/deployments/"+strconv.FormatInt(id, 10), nil, &out)
	return out, err
}

// DeploymentWait is Deployment with the server waiting, for at most wait,
// while the deployment is still running (the server allows 120 s). It answers
// as soon as the deployment ends. A server older than the diagnostics ignores
// the parameter and answers at once; the caller tells the two apart by how
// long the answer took (see deployflow's await).
func (c *Client) DeploymentWait(ctx context.Context, id int64, wait time.Duration) (DeploymentDetail, error) {
	if wait <= 0 {
		return c.Deployment(ctx, id)
	}
	var out DeploymentDetail
	p := "/api/v1/deployments/" + strconv.FormatInt(id, 10) + "?wait=" + strconv.Itoa(ceilSeconds(wait))
	err := c.doLong(ctx, wait, http.MethodGet, p, nil, &out)
	return out, err
}

// AppConfig returns the application's saved settings without secrets.
func (c *Client) AppConfig(ctx context.Context, ref string) (AppConfig, error) {
	var out AppConfig
	err := c.refShaped(ctx, ref, "config", "app settings (config)", &out)
	return out, err
}

// DomainStatus checks DNS and the served certificate of each of the app's
// domains.
func (c *Client) DomainStatus(ctx context.Context, ref string) (DomainStatus, error) {
	var out DomainStatus
	err := c.refShaped(ctx, ref, "domain-status", "the domain check (domains)", &out)
	return out, err
}

// BackupFilter narrows Backups to one app or one project/environment.
type BackupFilter struct {
	App         string
	Environment string
}

// Backups lists the organization's configured backups.
func (c *Client) Backups(ctx context.Context, f BackupFilter) ([]BackupInfo, error) {
	q := url.Values{}
	setNonEmpty(q, "app", f.App)
	setNonEmpty(q, "environment", f.Environment)
	var out []BackupInfo
	err := c.do(ctx, http.MethodGet, withQuery("/api/v1/backups", q), nil, &out)
	return out, c.routeMissing(ctx, err, "backups")
}

// RunBackup starts a configured backup now. It answers at once; the run's
// result is read from Backups afterwards.
func (c *Client) RunBackup(ctx context.Context, kind string, id int64) (BackupStarted, error) {
	var out BackupStarted
	p := "/api/v1/backups/" + url.PathEscape(kind) + "/" + strconv.FormatInt(id, 10) + "/run"
	err := c.do(ctx, http.MethodPost, p, map[string]string{}, &out)
	return out, c.routeMissing(ctx, err, "starting a backup (backups run)")
}

// Env returns variable names and their source. Values are never returned.
func (c *Client) Env(ctx context.Context, ref string) ([]EnvKey, error) {
	var out []EnvKey
	err := c.do(ctx, http.MethodGet, appPath(ref, "env"), nil, &out)
	return out, err
}

// Deploy enqueues a deployment, optionally moving an image app to a new tag
// first.
//
// The retag is persistent and the API has no operation that undoes it, so an
// empty tag is sent as an absent field rather than an empty string: this is
// also the call used after an image upload, where the server has already set
// the reference and must not be asked to change it.
func (c *Client) Deploy(ctx context.Context, ref, tag string) (Accepted, error) {
	body := map[string]string{}
	if tag != "" {
		body["tag"] = tag
	}
	var out Accepted
	err := c.do(ctx, http.MethodPost, appPath(ref, "deploy"), body, &out)
	return out, err
}

// Rebuild forces a no-cache build of a dockerfile app.
func (c *Client) Rebuild(ctx context.Context, ref string) (Accepted, error) {
	var out Accepted
	err := c.do(ctx, http.MethodPost, appPath(ref, "rebuild"), map[string]string{}, &out)
	return out, err
}

// Reload restarts the running tasks in place.
func (c *Client) Reload(ctx context.Context, ref string) error {
	return c.do(ctx, http.MethodPost, appPath(ref, "reload"), map[string]string{}, nil)
}

// Stop scales the application to zero replicas. Deploy brings it back.
func (c *Client) Stop(ctx context.Context, ref string) error {
	return c.do(ctx, http.MethodPost, appPath(ref, "stop"), map[string]string{}, nil)
}

// SetEnv sets or removes one variable. It does NOT restart anything: the
// running container keeps the old value until the next deploy.
func (c *Client) SetEnv(ctx context.Context, ref, key, value string, remove bool) error {
	body := map[string]any{"key": key}
	if remove {
		body["remove"] = true
	} else {
		body["value"] = value
	}
	return c.do(ctx, http.MethodPost, appPath(ref, "env"), body, nil)
}

func setNonEmpty(q url.Values, key, v string) {
	if v != "" {
		q.Set(key, v)
	}
}

func withQuery(p string, q url.Values) string {
	if len(q) == 0 {
		return p
	}
	return p + "?" + q.Encode()
}

// ceilSeconds rounds a wait up to whole seconds, the unit the API takes, and
// never to zero: zero means "do not wait" there.
func ceilSeconds(d time.Duration) int {
	n := int((d + time.Second - 1) / time.Second)
	if n < 1 {
		n = 1
	}
	return n
}
