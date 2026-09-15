// Package selfupdate discovers the latest published Krill release on GitHub
// (Checker) and installs it over the running binary behind a dead-man
// rollback timer (Updater).
//
// Discovery follows the unauthenticated `releases/latest` redirect rather
// than calling the GitHub REST API: the same origin the release binaries are
// downloaded from, the same mechanism install.sh already uses, and no
// 60-requests-per-hour-per-IP rate limit to worry about.
package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/semver"

	"github.com/proshik/krill/internal/buildinfo"
	"github.com/proshik/krill/internal/netguard"
)

// maxBodyDrain bounds how much of a response body Check reads before closing
// it, so a misbehaving or malicious endpoint can't make a check hang or eat
// memory.
const maxBodyDrain = 64 * 1024

// tagPattern matches a release tag: vMAJOR.MINOR.PATCH with no prerelease or
// build-metadata suffix — the only shape a published Krill release ever gets
// tagged with (see .github/workflows/release.yml).
var tagPattern = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// ValidTag reports whether tag is a Krill release tag.
func ValidTag(tag string) bool {
	return tagPattern.MatchString(tag)
}

// Newer reports whether latest is a strictly greater semantic version than
// current. Both must be valid semver for the comparison to mean anything —
// in particular the "dev" and "dev+<rev>" strings a local (non-release)
// build reports, and an empty string, never compare as newer than anything.
func Newer(current, latest string) bool {
	return semver.IsValid(current) && semver.IsValid(latest) && semver.Compare(latest, current) > 0
}

// Release is a discovered GitHub release.
type Release struct {
	Tag string // e.g. "v0.2.0"
	URL string // the human release page, e.g. "https://github.com/<repo>/releases/tag/v0.2.0"
}

// ErrThrottled is returned by CheckNow when the previous manual check ran too
// recently; the caller gets the cached release instead of a fresh lookup.
var ErrThrottled = errors.New("selfupdate: check throttled, try again later")

// ErrNoRelease is returned when the repository has no published release yet.
var ErrNoRelease = errors.New("selfupdate: repository has no published release")

// CheckState is a snapshot of the Checker's last discovery attempt.
type CheckState struct {
	// Latest is the last SUCCESSFULLY discovered release; the zero value if
	// no check has ever succeeded.
	Latest Release
	// CheckedAt is when the last attempt (success or failure) ran.
	CheckedAt time.Time
	// Err is the last attempt's error; nil after a successful check.
	Err error
}

// Checker periodically discovers the latest published Krill release on
// GitHub. The zero value is not usable; construct one with NewChecker.
type Checker struct {
	repo         string // "owner/repo"
	interval     time.Duration
	allowPrivate bool
	client       *http.Client

	// baseURL, current, initialDelay and now are overridden directly by
	// tests in this package; there are deliberately no exported setters.
	baseURL        string
	current        string
	initialDelay   time.Duration
	manualCooldown time.Duration
	now            func() time.Time

	mu         sync.Mutex
	state      CheckState
	lastManual time.Time // zero until the first non-throttled CheckNow
}

// NewChecker creates a Checker for repo ("owner/repo"), polling every
// interval once Run is started (Run does nothing when interval <= 0).
// allowPrivate is forwarded to the netguard egress guard that the HTTP
// client dials through; it should be false in production and is true only
// for tests / local development against a non-public endpoint.
func NewChecker(repo string, interval time.Duration, allowPrivate bool) *Checker {
	return &Checker{
		repo:         repo,
		interval:     interval,
		allowPrivate: allowPrivate,
		client: &http.Client{
			Transport: &http.Transport{
				DialContext:           netguard.DialContext(allowPrivate),
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 15 * time.Second,
			},
			// Discovery reads the Location header itself; it must never
			// follow the redirect.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		baseURL:        "https://github.com",
		current:        buildinfo.String(),
		initialDelay:   time.Minute,
		manualCooldown: time.Minute,
		now:            time.Now,
	}
}

// Current returns the running version the checker compares discovered
// releases against.
func (c *Checker) Current() string {
	return c.current
}

// State returns a snapshot of the last discovery attempt.
func (c *Checker) State() CheckState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// Available returns the cached latest release and whether it is newer than
// the running version. The zero Release and false when nothing has been
// discovered yet.
func (c *Checker) Available() (Release, bool) {
	c.mu.Lock()
	rel := c.state.Latest
	c.mu.Unlock()
	return rel, Newer(c.current, rel.Tag)
}

// Check performs one discovery attempt and updates the cached state: on
// success Latest and CheckedAt are updated and Err is cleared; on failure the
// previously cached Latest is preserved, CheckedAt still advances, and Err is
// set. It returns the same (Release, error) pair it just cached.
func (c *Checker) Check(ctx context.Context) (Release, error) {
	rel, err := c.discover(ctx)

	c.mu.Lock()
	c.state.CheckedAt = c.now()
	c.state.Err = err
	if err == nil {
		c.state.Latest = rel
	}
	c.mu.Unlock()

	if err != nil {
		return Release{}, err
	}
	return rel, nil
}

// CheckNow performs a manual, user-triggered check, throttled to at most one
// network call per manualCooldown: a call within the cooldown of the previous
// manual call returns the cached Latest release and ErrThrottled without
// touching the network.
func (c *Checker) CheckNow(ctx context.Context) (Release, error) {
	now := c.now()

	c.mu.Lock()
	if !c.lastManual.IsZero() && now.Sub(c.lastManual) < c.manualCooldown {
		cached := c.state.Latest
		c.mu.Unlock()
		return cached, ErrThrottled
	}
	c.lastManual = now
	c.mu.Unlock()

	return c.Check(ctx)
}

// Run waits initialDelay (or until ctx is done, whichever comes first), runs
// a Check, then runs another Check every interval until ctx is done. It
// returns immediately without checking anything when interval <= 0.
func (c *Checker) Run(ctx context.Context) {
	if c.interval <= 0 {
		return
	}

	timer := time.NewTimer(c.initialDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}
	c.runAndLog(ctx)

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.runAndLog(ctx)
		}
	}
}

// runAndLog performs one Check and logs the outcome: a warning on error, an
// info line only when the discovered tag changed AND is newer than the
// running version, nothing when the result is unchanged.
func (c *Checker) runAndLog(ctx context.Context) {
	c.mu.Lock()
	prevTag := c.state.Latest.Tag
	c.mu.Unlock()

	rel, err := c.Check(ctx)
	if err != nil {
		slog.Warn("update check failed", "err", err)
		return
	}
	if rel.Tag != prevTag && Newer(c.current, rel.Tag) {
		slog.Info("a newer Krill release is available", "current", c.current, "latest", rel.Tag)
	}
}

// discover performs the actual HTTP round trip: it follows the
// "releases/latest" redirect one hop (without the client following it) and
// parses the target tag out of the Location header.
func (c *Checker) discover(ctx context.Context) (Release, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	reqURL := strings.TrimRight(c.baseURL, "/") + "/" + c.repo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return Release{}, fmt.Errorf("selfupdate: building request: %w", err)
	}
	req.Header.Set("User-Agent", fmt.Sprintf("krill/%s (%s/%s)", c.current, runtime.GOOS, runtime.GOARCH))
	req.Header.Set("Accept", "text/html")

	resp, err := c.client.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("selfupdate: requesting releases/latest: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyDrain))
		_ = resp.Body.Close()
	}()

	switch resp.StatusCode {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
	default:
		return Release{}, fmt.Errorf("unexpected status %d from releases/latest", resp.StatusCode)
	}

	loc := resp.Header.Get("Location")
	if loc == "" {
		return Release{}, fmt.Errorf("unexpected status %d from releases/latest: missing Location header", resp.StatusCode)
	}
	locURL, err := url.Parse(loc)
	if err != nil {
		return Release{}, fmt.Errorf("selfupdate: invalid Location header %q: %w", loc, err)
	}
	target := req.URL.ResolveReference(locURL)

	return c.parseTargetPath(target.Path)
}

// parseTargetPath interprets the resolved redirect target's path. It must be
// exactly "/<repo>/releases/tag/<tag>" (the repo segment compared
// case-insensitively, since GitHub may canonicalize owner case) with a valid
// release tag, or "/<repo>/releases" (GitHub's redirect for a repository
// with no published release, reported as ErrNoRelease).
func (c *Checker) parseTargetPath(path string) (Release, error) {
	prefix := "/" + c.repo
	if len(path) < len(prefix) || !strings.EqualFold(path[:len(prefix)], prefix) {
		return Release{}, fmt.Errorf("unexpected redirect target %q", path)
	}
	rest := path[len(prefix):]

	if rest == "/releases" {
		return Release{}, ErrNoRelease
	}

	const tagPrefix = "/releases/tag/"
	if !strings.HasPrefix(rest, tagPrefix) {
		return Release{}, fmt.Errorf("unexpected redirect target %q", path)
	}
	tag, err := url.PathUnescape(strings.TrimPrefix(rest, tagPrefix))
	if err != nil {
		return Release{}, fmt.Errorf("selfupdate: invalid tag encoding in redirect target %q: %w", path, err)
	}
	if !ValidTag(tag) {
		return Release{}, fmt.Errorf("invalid release tag %q", tag)
	}

	return Release{
		Tag: tag,
		URL: strings.TrimRight(c.baseURL, "/") + "/" + c.repo + "/releases/tag/" + tag,
	}, nil
}
