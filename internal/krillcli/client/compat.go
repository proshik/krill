package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// OldServerError says the server answered as a Krill that predates an
// operation this client asked for. It is reported instead of the server's
// own answer, which for an unknown operation is either a bare 404, a 400
// about an app reference nobody typed, or — for an ignored parameter — a
// plausible but wrong result.
type OldServerError struct {
	Op string
}

func (e *OldServerError) Error() string {
	return fmt.Sprintf("this Krill server does not support %s: it is older than the release that added the "+
		"agent API diagnostics. Update Krill (Settings → Updates in the web UI) to use it", e.Op)
}

// WellFormedRef reports a numeric id or a project/environment/app path — the
// only references the server peels a trailing config or domain-status off.
func WellFormedRef(ref string) bool {
	if _, err := strconv.ParseInt(ref, 10, 64); err == nil {
		return true
	}
	parts := strings.Split(ref, "/")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
	}
	return true
}

// refShaped reads GET /apps/<ref>/<verb> for a verb the server recognizes
// only after a well-formed reference.
//
// The server tells "no such route" from "no such app" for these verbs only
// through the reference: an older server does not know the verb, reads
// "<ref>/config" as the app reference, and refuses it as neither an id nor a
// three-segment path (400). A current one answers 404 not_found for a
// missing app. Sending only well-formed references makes that 400 mean one
// thing — an older server — and nothing else.
func (c *Client) refShaped(ctx context.Context, ref, verb, op string, out any) error {
	if !WellFormedRef(ref) {
		return &UsageError{Err: fmt.Errorf("%q is not an app reference: use the numeric id or project/environment/app", ref)}
	}
	err := c.do(ctx, http.MethodGet, appPath(ref, verb), nil, out)
	var ae *APIError
	if errors.As(err, &ae) && ae.HTTPStatus == http.StatusBadRequest &&
		strings.Contains(ae.Message, "is neither a numeric id nor") &&
		strings.Contains(ae.Message, "/"+verb) {
		return &OldServerError{Op: op}
	}
	return err
}

// routeMissing turns the router's plain 404 on a route this client knows
// into OldServerError — but only once whoami shows the agent API is there at
// all: the same bare 404 is what every route answers when the API is
// switched off (KRILL_AGENT_API_ENABLED=false) or the base URL is wrong, and
// the CLI explains that case separately.
func (c *Client) routeMissing(ctx context.Context, err error, op string) error {
	var ae *APIError
	if !errors.As(err, &ae) || !isRouterNotFound(ae) {
		return err
	}
	if _, werr := c.Whoami(ctx); werr != nil {
		// A throttled or briefly unavailable probe proves nothing either
		// way: report that, with its Retry-After, rather than the bare 404
		// the CLI would explain as a switched-off API.
		var we *APIError
		if errors.As(werr, &we) && we.IsRetryable() {
			return werr
		}
		return err
	}
	return &OldServerError{Op: op}
}

// isRouterNotFound is a 404 that did not come from an API handler: those
// always carry a JSON error envelope, the router's is plain text.
func isRouterNotFound(ae *APIError) bool {
	return ae.HTTPStatus == http.StatusNotFound && ae.Code == "http_404"
}

// requireDiagnostics refuses an operation whose parameters an older server
// would silently ignore. whoami's instance block arrived in the same release
// as those parameters, and a current server always fills its version.
func (c *Client) requireDiagnostics(ctx context.Context, op string) error {
	who, err := c.Whoami(ctx)
	if err != nil {
		return err
	}
	if who.InstanceVersion() == "" {
		return &OldServerError{Op: op}
	}
	return nil
}

// UsageError is an argument this client refuses before sending anything.
type UsageError struct{ Err error }

func (e *UsageError) Error() string { return e.Err.Error() }
func (e *UsageError) Unwrap() error { return e.Err }
