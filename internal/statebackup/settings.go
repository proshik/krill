package statebackup

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"filippo.io/age"

	"github.com/proshik/krill/internal/backup"
)

// DefaultRetention is how many scheduled dumps are kept unless set otherwise.
const DefaultRetention = 14

var (
	ErrInvalidRecipient  = errors.New("statebackup: the age public key must be an X25519 recipient (age1...)")
	ErrBucketRequired    = errors.New("statebackup: the bucket is required")
	ErrSecretKeyRequired = errors.New("statebackup: enter the secret key again")
	ErrRetention         = errors.New("statebackup: keep between 1 and 1000 backups")
	ErrInvalidSchedule   = errors.New("statebackup: invalid schedule")
)

// Settings is the stored configuration of the state backup.
type Settings struct {
	Enabled                             bool
	Endpoint, Bucket, Region, AccessKey string
	SecretKey                           string // plaintext after Load; "" from LoadView
	HasSecretKey                        bool   // set by LoadView and Load
	Prefix, Recipient, Schedule         string
	Retention                           int
	UpdatedAt                           time.Time
}

// Configured reports whether a run can be made at all.
func (s Settings) Configured() bool { return s.Enabled && s.Bucket != "" && s.Recipient != "" }

// Destination is the S3 target of the settings.
func (s Settings) Destination() backup.Destination {
	return backup.Destination{
		Endpoint: s.Endpoint, Bucket: s.Bucket, Region: s.Region,
		AccessKey: s.AccessKey, SecretKey: s.SecretKey,
	}
}

// Input is the settings form.
type Input struct {
	Endpoint, Bucket, Region, AccessKey, SecretKey, Prefix, Recipient string
	Preset, CustomCron                                                string
	Retention                                                         int
}

const recipientLinePrefix = "public key:"

// NormalizeRecipient trims a pasted age public key and accepts the whole
// "# public key: age1..." line that age-keygen prints. Only X25519 recipients
// are accepted; anything else, a private identity included, is refused with an
// error that never repeats the input.
func NormalizeRecipient(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimSpace(strings.TrimPrefix(s, "#"))
	if len(s) >= len(recipientLinePrefix) && strings.EqualFold(s[:len(recipientLinePrefix)], recipientLinePrefix) {
		s = strings.TrimSpace(s[len(recipientLinePrefix):])
	}
	r, err := age.ParseX25519Recipient(s)
	if err != nil {
		return "", ErrInvalidRecipient
	}
	return r.String(), nil
}

// Prepare validates in against what is stored and returns the row to save.
// Enabled is carried over from stored: saving the form never turns the
// feature on or off.
func Prepare(stored Settings, in Input) (Settings, error) {
	bucket := strings.TrimSpace(in.Bucket)
	if bucket == "" {
		return Settings{}, ErrBucketRequired
	}
	if in.Retention < 1 || in.Retention > 1000 {
		return Settings{}, ErrRetention
	}
	schedule, err := backup.CronForPreset(in.Preset, strings.TrimSpace(in.CustomCron))
	if err != nil {
		return Settings{}, fmt.Errorf("%w: %v", ErrInvalidSchedule, err)
	}
	recipient, err := NormalizeRecipient(in.Recipient)
	if err != nil {
		return Settings{}, err
	}
	endpoint := strings.TrimSpace(in.Endpoint)

	out := Settings{
		Enabled:  stored.Enabled,
		Endpoint: endpoint, Bucket: bucket,
		Region:    strings.TrimSpace(in.Region),
		AccessKey: strings.TrimSpace(in.AccessKey),
		Prefix:    strings.TrimSpace(in.Prefix),
		Recipient: recipient, Schedule: schedule, Retention: in.Retention,
	}
	switch {
	case in.SecretKey != "":
		out.SecretKey, out.HasSecretKey = in.SecretKey, true
	case stored.HasSecretKey && sameServer(stored.Endpoint, endpoint):
		// A kept key must never go to another host.
		out.SecretKey, out.HasSecretKey = stored.SecretKey, true
	default:
		return Settings{}, ErrSecretKeyRequired
	}
	return out, nil
}

// sameServer reports whether two endpoints name the same scheme, host and
// effective port. Two empty endpoints (AWS) are the same server; an endpoint
// that does not parse is never the same as anything.
func sameServer(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	ua, ea := url.Parse(a)
	ub, eb := url.Parse(b)
	if ea != nil || eb != nil || ua.Hostname() == "" || ub.Hostname() == "" {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) &&
		strings.EqualFold(ua.Hostname(), ub.Hostname()) &&
		effectivePort(ua) == effectivePort(ub)
}

func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}
