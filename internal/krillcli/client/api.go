package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
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

// Logs returns a tail of the application's runtime log.
//
// There is no follow mode, and there cannot be one here: live streaming is a
// WebSocket the server authenticates with a session cookie, which a token
// client does not have.
func (c *Client) Logs(ctx context.Context, ref string, tail int, level string) ([]LogLine, error) {
	q := url.Values{}
	if tail > 0 {
		q.Set("tail", strconv.Itoa(tail))
	}
	if level != "" {
		q.Set("level", level)
	}
	p := appPath(ref, "logs")
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	var out []LogLine
	err := c.do(ctx, http.MethodGet, p, nil, &out)
	return out, err
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

// FindApp resolves a user-typed application reference against the org's
// applications, so a mistyped path is answered with the list of what exists
// instead of a bare not-found.
func (c *Client) FindApp(ctx context.Context, ref string) (App, error) {
	apps, err := c.ListApps(ctx)
	if err != nil {
		return App{}, err
	}
	for _, a := range apps {
		if a.Path == ref || strconv.FormatInt(a.ID, 10) == ref {
			return a, nil
		}
	}
	return App{}, fmt.Errorf("no application %q in this organization; run `krill-cli apps` to see what there is", ref)
}
