package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxErrBody caps how much of an unexpected error body is read. A
// misconfigured proxy in front of Krill can answer with an HTML page, and
// none of it belongs in a terminal.
const maxErrBody = 8 << 10

// Client is a Krill agent-API client bound to one server and token.
type Client struct {
	base  string
	token string
	ua    string

	// hc handles ordinary calls, which are small and quick.
	hc *http.Client
	// upload handles the image upload. Nothing calls it yet — the upload
	// delivery mode is refused at pre-flight until the server endpoint exists
	// (see docs/superpowers/specs/2026-09-06-krill-cli-design.md §8) — and it
	// is kept rather than deleted because the reason it is a separate client
	// is the kind of thing that gets rediscovered the hard way:
	//
	// It is a SEPARATE client with no
	// Client.Timeout at all: that field covers the entire request including
	// the body transfer, so any value large enough for a 500 MB upload would
	// be useless as a timeout for anything else, and any value sensible for
	// the rest would kill every upload. Uploads are bounded by their context
	// instead.
	upload *http.Client
}

// New builds a client. server is the base URL, e.g. https://krill.example.com.
func New(server, token, userAgent string) (*Client, error) {
	s := strings.TrimRight(strings.TrimSpace(server), "/")
	if s == "" {
		return nil, fmt.Errorf("server URL is empty")
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("server must be a URL like https://krill.example.com, got %q", server)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("server URL must be http or https, got %q", u.Scheme)
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("API token is empty")
	}
	return &Client{
		base:   s,
		token:  strings.TrimSpace(token),
		ua:     userAgent,
		hc:     &http.Client{Timeout: 30 * time.Second, CheckRedirect: noRedirect},
		upload: &http.Client{CheckRedirect: noRedirect},
	}, nil
}

// noRedirect stops the client from following redirects.
//
// Following one is worse than it looks. Go rewrites a redirected POST into a
// body-less GET (301/302/303), so an operator's plain http→https redirect in
// front of Krill turns `POST /apps/x/deploy {"tag":…}` into `GET
// /apps/x/deploy`, which the router answers 404 — and only after the build and
// the push, because every pre-flight check is a GET and sails through. The
// Authorization header travels along for the ride. Refusing surfaces it on the
// first request instead, with a message that names the fix.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// Server returns the base URL, for messages that point the user at the web UI.
func (c *Client) Server() string { return c.base }

// appPath builds /api/v1/apps/<ref>[/<verb>].
//
// The reference is "project/environment/app" and its slashes are real path
// separators — chi splits the wildcard on them. Escaping the reference as a
// whole would turn them into %2F and the server would see one segment that
// matches no application, so each segment is escaped on its own and they are
// rejoined.
func appPath(ref, verb string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	p := "/api/v1/apps/" + strings.Join(parts, "/")
	if verb != "" {
		p += "/" + verb
	}
	return p
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	// Header only. The API does not accept a token in the query string, and
	// it should not: query strings are logged verbatim by reverse proxies.
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if c.ua != "" {
		req.Header.Set("User-Agent", c.ua)
	}
	return req, nil
}

// do performs a request and decodes a JSON response into out (which may be
// nil to discard the body).
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		// The URL is echoed by url.Error and the token is not in it, but the
		// error is rebuilt from scratch anyway rather than wrapped, so no
		// future change to what net/http reports can start including a
		// header. A leaked token in a terminal is a leaked token in a
		// scrollback, a screenshot and a pasted bug report.
		return fmt.Errorf("cannot reach %s: %s", c.base, netErrText(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 && resp.StatusCode <= 399 {
		return redirectError(c.base, resp)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return parseAPIError(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("server sent a response this client could not read (%s %s): %w", method, path, err)
	}
	return nil
}

// parseAPIError turns a non-2xx response into an APIError.
//
// It must tolerate a body that is not JSON. Two of the API's own 404s come
// from the router rather than the handler and are plain text ("404 page not
// found"), and those are exactly the interesting ones: an unrecognized verb,
// and an agent API that is switched off server-side. A JSON decode failure
// here would replace a diagnosable condition with a parse error.
func parseAPIError(resp *http.Response) error {
	e := &APIError{HTTPStatus: resp.StatusCode}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
			e.RetryAfter = time.Duration(secs) * time.Second
		}
	}

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	var env struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal(raw, &env) == nil && env.Code != "" {
		e.Code, e.Message, e.RequestID = env.Code, env.Message, env.RequestID
		return e
	}

	e.Code = "http_" + strconv.Itoa(resp.StatusCode)
	e.Message = strings.TrimSpace(string(raw))
	if e.Message == "" {
		e.Message = http.StatusText(resp.StatusCode)
	}
	return e
}

// redirectError explains a redirect rather than following it. It is nearly
// always a proxy in front of Krill upgrading http to https, and the fix is to
// log in to the address the proxy redirects TO.
func redirectError(base string, resp *http.Response) error {
	loc := strings.TrimSpace(resp.Header.Get("Location"))
	e := &APIError{
		HTTPStatus: resp.StatusCode,
		Code:       "redirected",
		Message: fmt.Sprintf("%s redirected this request (%d) and krill-cli does not follow redirects: "+
			"a POST would arrive as a body-less GET and the deploy would silently do nothing", base, resp.StatusCode),
	}
	if loc != "" {
		e.Message += fmt.Sprintf(". It points at %s — run `krill-cli login --server <that base URL>`", loc)
	}
	return e
}

// netErrText strips a transport error down to its message.
func netErrText(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}
