package observability

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/secret"
)

const (
	maxUserLen     = 256
	maxPasswordLen = 4096
)

var (
	ErrInvalidURL        = errors.New("observability: the address must be an http(s) URL without credentials, a fragment or spaces")
	ErrInvalidUser       = errors.New("observability: the user name must not contain spaces, quotes or backslashes")
	ErrInvalidPassword   = errors.New("observability: the password is too long")
	ErrNothingConfigured = errors.New("observability: neither a metrics nor a logs address is set")
	ErrNotSaved          = errors.New("observability: the settings have not been saved yet")
	ErrPasswordRequired  = errors.New("observability: the address changed; enter the password again")
	ErrClearNotConfirmed = errors.New("observability: the address is empty; tick the confirmation to remove these settings")
	ErrClearWithAddress  = errors.New("observability: the confirmation is ticked but the address is not empty; clear it to remove these settings")
)

// Target is one destination the agent pushes to.
type Target struct {
	URL      string
	User     string // "" = no basic auth
	Password string // plaintext; "" = none
}

// Configured reports whether the pipeline for this target is on.
func (t Target) Configured() bool { return t.URL != "" }

// Settings is the stored configuration with passwords decrypted.
type Settings struct {
	Enabled bool
	Metrics Target // Prometheus remote_write
	Logs    Target // Loki push API
}

// TargetInput is one target as the settings form submits it.
type TargetInput struct {
	URL, User string
	Password  string // "" keeps the stored password
	// Clear confirms an intentional wipe of an already-configured target
	// whose URL field was submitted empty. Without it, mergeTarget refuses
	// the save (ErrClearNotConfirmed): an address left blank by an
	// unintentionally empty form submission must not silently erase a
	// working configuration.
	Clear bool
}

// Input is the settings form.
type Input struct{ Metrics, Logs TargetInput }

// Store is the slice of the generated queries the settings need.
type Store interface {
	GetObservabilitySettings(ctx context.Context) (db.ObservabilitySetting, error)
	SaveObservabilitySettings(ctx context.Context, arg db.SaveObservabilitySettingsParams) error
	SetObservabilityEnabled(ctx context.Context, enabled bool) (int64, error)
}

// safeText reports whether s can be written into the agent's configuration
// as a string literal: no whitespace or control characters, no quote and no
// backslash. Rejecting them up front keeps the generated file unambiguous.
func safeText(s string) bool {
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '"' || r == '\\' {
			return false
		}
	}
	return true
}

// NormalizeURL validates a push address. "" is allowed and turns the
// pipeline off. Credentials belong in the user and password fields: in the
// URL they would end up in the generated configuration in clear text.
func NormalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if !safeText(raw) {
		return "", ErrInvalidURL
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return "", ErrInvalidURL
	}
	return u.String(), nil
}

func checkUser(u string) error {
	if len(u) > maxUserLen || !safeText(u) {
		return ErrInvalidUser
	}
	return nil
}

// Load reads the settings. An instance that never saved any reads as off.
func Load(ctx context.Context, q Store) (Settings, error) {
	row, err := q.GetObservabilitySettings(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return Settings{}, nil
	}
	if err != nil {
		return Settings{}, err
	}
	mp, err := secret.Dec(row.MetricsPassword)
	if err != nil {
		return Settings{}, fmt.Errorf("metrics password: %w", err)
	}
	lp, err := secret.Dec(row.LogsPassword)
	if err != nil {
		return Settings{}, fmt.Errorf("logs password: %w", err)
	}
	return Settings{
		Enabled: row.Enabled,
		Metrics: Target{URL: row.MetricsUrl, User: row.MetricsUser, Password: mp},
		Logs:    Target{URL: row.LogsUrl, User: row.LogsUser, Password: lp},
	}, nil
}

// LoadForReconcile is Load for the reconciler: a disabled configuration reads
// as the zero Settings without decrypting anything. Otherwise a password that
// no longer decrypts (a rotated KRILL_SECRET_KEY) would fail every pass, and
// turning the agent off would never remove it.
func LoadForReconcile(ctx context.Context, q Store) (Settings, error) {
	row, err := q.GetObservabilitySettings(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return Settings{}, nil
	}
	if err != nil {
		return Settings{}, err
	}
	if !row.Enabled {
		return Settings{}, nil
	}
	return Load(ctx, q)
}

// Save stores the form. It works on the stored (encrypted) passwords rather
// than on Load's result, so a password that no longer decrypts can still be
// kept or replaced.
func Save(ctx context.Context, q Store, in Input) error {
	row, err := q.GetObservabilitySettings(ctx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	mURL, mUser, mPass, err := mergeTarget(in.Metrics, row.MetricsUrl, row.MetricsPassword)
	if err != nil {
		return err
	}
	lURL, lUser, lPass, err := mergeTarget(in.Logs, row.LogsUrl, row.LogsPassword)
	if err != nil {
		return err
	}
	if row.Enabled && mURL == "" && lURL == "" {
		return ErrNothingConfigured
	}
	return q.SaveObservabilitySettings(ctx, db.SaveObservabilitySettingsParams{
		MetricsUrl: mURL, MetricsUser: mUser, MetricsPassword: mPass,
		LogsUrl: lURL, LogsUser: lUser, LogsPassword: lPass,
	})
}

// mergeTarget validates one target and returns what to store. Without a URL
// nothing is kept; without a user there is no basic auth and so no password;
// an empty password keeps storedPassword (still encrypted), but only while
// the address still points at the server storedURL did — a kept password must
// never be sent to a different host.
//
// Emptying a URL that WAS configured requires in.Clear: an accidental blank
// submission (a field the operator never meant to touch, cleared by the
// browser or simply left untouched on a form that renders it blank) must
// refuse rather than silently wipe a working target. Conversely, ticking
// Clear without also clearing the URL field is refused too, rather than
// silently ignored and the (unrelated) address saved as if Clear had never
// been ticked — the operator's checked box must mean something.
func mergeTarget(in TargetInput, storedURL, storedPassword string) (u, user, password string, err error) {
	if u, err = NormalizeURL(in.URL); err != nil {
		return "", "", "", err
	}
	if in.Clear && u != "" {
		return "", "", "", ErrClearWithAddress
	}
	if u == "" && storedURL != "" && !in.Clear {
		return "", "", "", ErrClearNotConfirmed
	}
	user = strings.TrimSpace(in.User)
	if err = checkUser(user); err != nil {
		return "", "", "", err
	}
	if len(in.Password) > maxPasswordLen {
		return "", "", "", ErrInvalidPassword
	}
	if u == "" || user == "" {
		return u, "", "", nil
	}
	if in.Password == "" {
		if storedPassword != "" && !sameServer(u, storedURL) {
			return "", "", "", ErrPasswordRequired
		}
		return u, user, storedPassword, nil
	}
	return u, user, secret.Enc(in.Password), nil
}

// sameServer reports whether two push addresses reach the same server: the
// same scheme, host and effective port (the scheme's default when absent).
func sameServer(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil {
		return false
	}
	return ua.Scheme == ub.Scheme &&
		strings.EqualFold(ua.Hostname(), ub.Hostname()) &&
		effectivePort(ua) == effectivePort(ub)
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch u.Scheme {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

// SetEnabled turns the agent on or off. Turning it on needs saved settings
// with at least one address, and passwords that decrypt: the agent's
// configuration is built from them.
func SetEnabled(ctx context.Context, q Store, on bool) error {
	if on {
		if _, err := q.GetObservabilitySettings(ctx); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotSaved
		} else if err != nil {
			return err
		}
		s, err := Load(ctx, q)
		if err != nil {
			return err
		}
		if !s.Metrics.Configured() && !s.Logs.Configured() {
			return ErrNothingConfigured
		}
	}
	n, err := q.SetObservabilityEnabled(ctx, on)
	if err != nil {
		return err
	}
	if n == 0 && on {
		return ErrNotSaved
	}
	return nil
}
