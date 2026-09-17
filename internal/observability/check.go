package observability

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/exp/api/remote"
	writev2 "github.com/prometheus/client_golang/exp/api/remote/genproto/v2"

	"github.com/proshik/krill/internal/buildinfo"
	"github.com/proshik/krill/internal/netguard"
)

const (
	defaultCheckTimeout = 10 * time.Second
	snippetLimit        = 300
	// maxResponseBody bounds how much of a response body the transport will
	// ever hand to a reader (the exp remote-write client's own io.ReadAll,
	// or our own), so a misbehaving or malicious endpoint can't force an
	// unbounded read.
	maxResponseBody = 64 * 1024
)

// Result is the verdict on one target. Key is an i18n key; Detail is its
// argument, or "" when the key takes none.
type Result struct {
	OK     bool
	Key    string
	Detail string
}

// Report is the verdict on both targets; nil means "not configured".
type Report struct {
	Metrics *Result
	Logs    *Result
}

// Checker sends one test sample and one test log line to the configured
// targets. It proves that the address answers and accepts the credentials,
// not that data will be kept.
type Checker struct {
	AllowPrivate bool          // KRILL_ALLOW_PRIVATE_EGRESS
	Instance     string        // krill_instance label of the test data
	Timeout      time.Duration // per target; 0 => 10s
}

// Check runs both checks in parallel.
func (c Checker) Check(ctx context.Context, s Settings) Report {
	var rep Report
	var wg sync.WaitGroup
	now := time.Now()
	if s.Metrics.Configured() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := c.checkMetrics(ctx, s.Metrics, now)
			rep.Metrics = &r
		}()
	}
	if s.Logs.Configured() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := c.checkLogs(ctx, s.Logs, now)
			rep.Logs = &r
		}()
	}
	wg.Wait()
	return rep
}

func (c Checker) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return defaultCheckTimeout
}

func (c Checker) instance() string {
	if c.Instance != "" {
		return c.Instance
	}
	return "krill"
}

// recordingTransport adds basic auth and remembers the last response code and
// connection error. The remote-write client reports the code only inside its
// error text and wraps connection errors in a type without Unwrap, so this is
// the reliable place to learn both.
type recordingTransport struct {
	base           http.RoundTripper
	user, password string

	mu      sync.Mutex
	status  int
	connErr error
}

func (t *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	if t.user != "" {
		req.SetBasicAuth(t.user, t.password)
	}
	req.Header.Set("User-Agent", "krill/"+buildinfo.String())
	resp, err := t.base.RoundTrip(req)
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil {
		t.status, t.connErr = 0, err
		return nil, err
	}
	t.status, t.connErr = resp.StatusCode, nil
	if resp.Body != nil {
		// Bound how much of the body the caller (the exp remote-write
		// client's own io.ReadAll, or checkLogs') can ever read, regardless
		// of what the endpoint sends.
		resp.Body = limitedBody{Reader: io.LimitReader(resp.Body, maxResponseBody), Closer: resp.Body}
	}
	return resp, nil
}

// limitedBody caps a response body's Read while still closing the
// underlying body.
type limitedBody struct {
	io.Reader
	io.Closer
}

func (t *recordingTransport) last() (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status, t.connErr
}

func (c Checker) client(t Target) (*http.Client, *recordingTransport) {
	rt := &recordingTransport{
		base: &http.Transport{
			DialContext:           netguard.DialContext(c.AllowPrivate),
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: c.timeout(),
			// One request per target per check: keeping a connection idle
			// afterwards only leaks it (and the goroutines that go with it).
			DisableKeepAlives: true,
		},
		user:     t.User,
		password: t.Password,
	}
	return &http.Client{
		Transport: rt,
		Timeout:   c.timeout(),
		// A push endpoint that redirects is misconfigured; following it would
		// also resend the credentials somewhere else.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, rt
}

func (c Checker) checkMetrics(ctx context.Context, t Target, now time.Time) Result {
	hc, rt := c.client(t)
	redact := redactor(t)
	api, err := remote.NewAPI(t.URL,
		remote.WithAPIPath(""), // the stored URL is the full push URL, as in Alloy
		remote.WithAPIHTTPClient(hc),
		// MaxRetries 0 would mean "retry forever".
		remote.WithAPIBackoff(remote.BackoffConfig{Min: 500 * time.Millisecond, Max: time.Second, MaxRetries: 1}),
		remote.WithAPINoRetryOnRateLimit(),
	)
	if err != nil {
		return Result{Key: "obs.check.network", Detail: safeSnippet(err.Error(), redact)}
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	_, werr := api.Write(ctx, remote.WriteV2MessageType, c.sample(now))
	code, connErr := rt.last()
	return classify(code, werr, connErr, "obs.check.not_found_metrics", true, redact)
}

func (c Checker) sample(now time.Time) *writev2.Request {
	st := writev2.NewSymbolTable()
	// Labels must be sorted by name.
	refs := st.SymbolizeLabels([]string{"__name__", "krill_observability_check", "krill_instance", c.instance()}, nil)
	return &writev2.Request{
		Symbols: st.Symbols(),
		Timeseries: []*writev2.TimeSeries{{
			LabelsRefs: refs,
			Samples:    []*writev2.Sample{{Value: 1, Timestamp: now.UnixMilli()}},
			Metadata:   &writev2.Metadata{Type: writev2.Metadata_METRIC_TYPE_GAUGE},
		}},
	}
}

func (c Checker) checkLogs(ctx context.Context, t Target, now time.Time) Result {
	hc, rt := c.client(t)
	redact := redactor(t)
	body, err := json.Marshal(map[string]any{"streams": []any{map[string]any{
		"stream": map[string]string{"krill_check": "observability", "krill_instance": c.instance()},
		"values": [][]string{{strconv.FormatInt(now.UnixNano(), 10), "krill observability check"}},
	}}})
	if err != nil {
		return Result{Key: "obs.check.network", Detail: safeSnippet(err.Error(), redact)}
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(body))
	if err != nil {
		return Result{Key: "obs.check.network", Detail: safeSnippet(err.Error(), redact)}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, herr := hc.Do(req)
	if herr == nil {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			herr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, b)
		}
	}
	code, connErr := rt.last()
	return classify(code, herr, connErr, "obs.check.not_found_logs", false, redact)
}

// classify turns an attempt into a verdict (spec §4, «Проверить»). redact
// strips t's credentials from any text pulled from the response; classify
// applies it around snippet() rather than leaving that to the caller, so a
// truncated Detail can never leave a partial secret behind.
func classify(code int, err, connErr error, notFoundKey string, remoteWriteV2 bool, redact func(string) string) Result {
	switch {
	case code >= 200 && code < 300 && err == nil:
		return Result{OK: true, Key: "obs.check.ok"}
	case code >= 200 && code < 300:
		// The v2 client treats a 2xx without write statistics as a failure:
		// the receiver most likely parsed the body as Remote Write 1.0.
		return Result{OK: true, Key: "obs.check.ok_unconfirmed"}
	case code == http.StatusUnsupportedMediaType && remoteWriteV2:
		// Reached and authorized, but only 1.0 is accepted — which is what
		// the agent sends.
		return Result{OK: true, Key: "obs.check.ok_v1_only"}
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return Result{Key: "obs.check.auth", Detail: strconv.Itoa(code)}
	case code == http.StatusNotFound:
		return Result{Key: notFoundKey}
	case code != 0:
		text := fmt.Sprintf("HTTP %d", code)
		if err != nil {
			text = err.Error()
			if !strings.Contains(text, strconv.Itoa(code)) {
				text = fmt.Sprintf("HTTP %d: %s", code, text)
			}
		}
		return Result{Key: "obs.check.http", Detail: safeSnippet(text, redact)}
	case errors.Is(connErr, netguard.ErrBlocked):
		return Result{Key: "obs.check.private"}
	}
	e := err
	if connErr != nil {
		e = connErr
	}
	if e == nil {
		e = errors.New("no response")
	}
	return Result{Key: "obs.check.network", Detail: safeSnippet(e.Error(), redact)}
}

// redactor returns a function that strips t's password and its basic-auth
// base64 encoding from a string. A target that echoes the request back in
// its error response — deliberately or by a bug — must never be able to
// leak the credential into a flash message; "passwords are never rendered
// or logged" applies to whatever text the target itself sends back too.
func redactor(t Target) func(string) string {
	return func(s string) string {
		if t.Password != "" {
			s = strings.ReplaceAll(s, t.Password, "***")
		}
		if t.User != "" || t.Password != "" {
			enc := base64.StdEncoding.EncodeToString([]byte(t.User + ":" + t.Password))
			s = strings.ReplaceAll(s, enc, "***")
		}
		return s
	}
}

// safeSnippet redacts, then truncates via snippet, then redacts again.
// Redacting only after truncation could leave half a secret behind if
// snippetLimit happened to cut through it; the second pass is a defensive
// guarantee that nothing snippet's own tag/whitespace cleanup could expose
// survives either.
func safeSnippet(s string, redact func(string) string) string {
	return redact(snippet(redact(s)))
}

var (
	tagRe   = regexp.MustCompile(`<[^>]*>`)
	spaceRe = regexp.MustCompile(`\s+`)
)

// snippet makes a response body or error fit a one-line flash: tags and
// runs of whitespace removed, cut to snippetLimit bytes on a rune boundary.
func snippet(s string) string {
	s = strings.TrimSpace(spaceRe.ReplaceAllString(tagRe.ReplaceAllString(s, " "), " "))
	if len(s) <= snippetLimit {
		return s
	}
	cut := snippetLimit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
