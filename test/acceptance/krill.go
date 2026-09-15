//go:build acceptance

package acceptance

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Client drives Krill's UI over the forwarded port the way a browser does:
// a cookie jar, same-origin Origin/Referer on every POST (the CSRF guard and
// the flash redirect both read them), redirects followed.
type Client struct {
	t    testing.TB
	base string
	http *http.Client
}

// NewClient returns a client for http://127.0.0.1:<hostPort>.
func NewClient(t testing.TB, hostPort int) *Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	return &Client{
		t:    t,
		base: "http://127.0.0.1:" + strconv.Itoa(hostPort),
		http: &http.Client{Jar: jar, Timeout: 30 * time.Second},
	}
}

// Response is a finished request: the final status after redirects, the
// final URL, the HX-Refresh header and the body.
type Response struct {
	Status    int
	URL       string
	HXRefresh bool
	Body      string
}

// Text is the body with tags stripped and entities decoded, whitespace
// collapsed — enough for strings.Contains assertions on page text.
func (r Response) Text() string { return PageText(r.Body) }

var (
	scriptRe = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	tagRe    = regexp.MustCompile(`(?s)<[^>]*>`)
	spaceRe  = regexp.MustCompile(`\s+`)
)

// PageText strips an HTML document down to its visible text.
func PageText(body string) string {
	s := scriptRe.ReplaceAllString(body, " ")
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.TrimSpace(spaceRe.ReplaceAllString(s, " "))
}

func (c *Client) do(ctx context.Context, method, path string, form url.Values, hdr map[string]string) (Response, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return Response{}, err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if method == http.MethodPost {
		req.Header.Set("Origin", c.base)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return Response{}, err
	}
	return Response{
		Status:    resp.StatusCode,
		URL:       resp.Request.URL.String(),
		HXRefresh: resp.Header.Get("HX-Refresh") == "true",
		Body:      string(data),
	}, nil
}

// Get fetches path.
func (c *Client) Get(ctx context.Context, path string) (Response, error) {
	resp, err := c.do(ctx, http.MethodGet, path, nil, nil)
	c.t.Logf("GET %s -> %d %s (err=%v)", path, resp.Status, resp.URL, err)
	return resp, err
}

// Post submits a form to path as if from the page at referer (a path).
func (c *Client) Post(ctx context.Context, path, referer string, form url.Values) (Response, error) {
	if form == nil {
		form = url.Values{}
	}
	hdr := map[string]string{}
	if referer != "" {
		hdr["Referer"] = c.base + referer
	}
	resp, err := c.do(ctx, http.MethodPost, path, form, hdr)
	c.t.Logf("POST %s %v -> %d %s (err=%v)", path, redactForm(form), resp.Status, resp.URL, err)
	return resp, err
}

func redactForm(f url.Values) url.Values {
	out := url.Values{}
	for k, v := range f {
		if k == "password" {
			out[k] = []string{"***"}
			continue
		}
		out[k] = v
	}
	return out
}

// Login signs in and checks that a session cookie was issued.
func (c *Client) Login(ctx context.Context, email, password string) error {
	resp, err := c.Post(ctx, "/login", "/login", url.Values{"email": {email}, "password": {password}})
	if err != nil {
		return err
	}
	u, _ := url.Parse(c.base)
	for _, ck := range c.http.Jar.Cookies(u) {
		if ck.Name == "krill_session" && ck.Value != "" {
			return nil
		}
	}
	return fmt.Errorf("login did not issue a session cookie (status %d, url %s): %s", resp.Status, resp.URL, bounded(resp.Text(), 500))
}

// UpdatesPath is the Updates page of an organization.
func UpdatesPath(orgID int64) string {
	return "/orgs/" + strconv.FormatInt(orgID, 10) + "/updates"
}

// UpdatesPage renders Settings -> Updates.
func (c *Client) UpdatesPage(ctx context.Context, orgID int64) (Response, error) {
	resp, err := c.Get(ctx, UpdatesPath(orgID))
	if err == nil && resp.Status != http.StatusOK {
		err = fmt.Errorf("updates page answered %d", resp.Status)
	}
	return resp, err
}

// CheckNow clicks "Check now"; the response is the page it redirects back to,
// carrying the flash.
func (c *Client) CheckNow(ctx context.Context, orgID int64) (Response, error) {
	p := UpdatesPath(orgID)
	return c.Post(ctx, p+"/check", p, nil)
}

// Install clicks "Update to <tag>".
func (c *Client) Install(ctx context.Context, orgID int64, tag string) (Response, error) {
	p := UpdatesPath(orgID)
	return c.Post(ctx, p+"/install", p, url.Values{"tag": {tag}})
}

// Rollback clicks "Roll back to ...".
func (c *Client) Rollback(ctx context.Context, orgID int64) (Response, error) {
	p := UpdatesPath(orgID)
	return c.Post(ctx, p+"/rollback", p, nil)
}

// Status polls the job fragment the way the page's htmx does. Redirects are
// not followed, and a 503 starting page is returned as a response, not an
// error.
func (c *Client) Status(ctx context.Context, orgID int64, from string) (Response, error) {
	path := UpdatesPath(orgID) + "/status?from=" + url.QueryEscape(from)
	return c.do(ctx, http.MethodGet, path, nil, map[string]string{"HX-Request": "true"})
}

// Serving reports whether /login answers 200 (the real router, not the
// starting page).
func (c *Client) Serving(ctx context.Context) (int, error) {
	resp, err := c.do(ctx, http.MethodGet, "/login", nil, nil)
	return resp.Status, err
}

// WaitServing polls /login until it answers 200. Connection errors (Krill
// restarting, or the forward not up yet) and the 503 starting page are
// expected while waiting; it reports whether the starting page was seen.
func (c *Client) WaitServing(ctx context.Context, timeout time.Duration) (sawStarting bool, err error) {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		status, rerr := c.Serving(rctx)
		cancel()
		switch {
		case rerr == nil && status == http.StatusOK:
			c.t.Logf("serving (last non-ready state: %s, starting page seen: %v)", last, sawStarting)
			return sawStarting, nil
		case rerr == nil && status == http.StatusServiceUnavailable:
			sawStarting = true
			last = "503 starting page"
		case rerr != nil:
			last = rerr.Error()
		default:
			last = "status " + strconv.Itoa(status)
		}
		if time.Now().After(deadline) {
			return sawStarting, fmt.Errorf("krill not serving after %s (last: %s)", timeout, last)
		}
		select {
		case <-ctx.Done():
			return sawStarting, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// WaitAvailable clicks "Check now" until the page offers "Krill <tag> is
// available." The checker throttles manual checks to one a minute, and
// GitHub's releases/latest redirect can lag a release edit, so it retries
// within timeout.
func (c *Client) WaitAvailable(ctx context.Context, orgID int64, tag string, timeout time.Duration) (Response, error) {
	want := "Krill " + tag + " is available."
	deadline := time.Now().Add(timeout)
	for {
		resp, err := c.CheckNow(ctx, orgID)
		if err == nil {
			if strings.Contains(resp.Text(), want) {
				return resp, nil
			}
			c.t.Logf("waiting for %q; page says: %s", want, bounded(resp.Text(), 600))
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = errors.New("page never offered " + tag)
			}
			return resp, fmt.Errorf("waiting for %s to be available: %w", tag, err)
		}
		select {
		case <-ctx.Done():
			return resp, ctx.Err()
		case <-time.After(20 * time.Second):
		}
	}
}
