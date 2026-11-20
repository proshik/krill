package statebackup

import (
	"errors"
	"strings"
	"testing"

	"filippo.io/age"
)

func newRecipient(t *testing.T) (recipient, identity string) {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	return id.Recipient().String(), id.String()
}

func TestNormalizeRecipient(t *testing.T) {
	rec, ident := newRecipient(t)

	for name, in := range map[string]string{
		"plain":            rec,
		"whitespace":       "  " + rec + " \n",
		"public key line":  "# public key: " + rec,
		"key line padded":  "\n  # public key: " + rec + "  \r\n",
		"key line no hash": "public key: " + rec,
	} {
		got, err := NormalizeRecipient(in)
		if err != nil || got != rec {
			t.Errorf("%s: got %q, %v; want %q", name, got, err, rec)
		}
	}

	for name, in := range map[string]string{
		"empty": "",
		"ssh":   "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl user@host",
		"junk":  "age1notakey",
	} {
		if _, err := NormalizeRecipient(in); !errors.Is(err, ErrInvalidRecipient) {
			t.Errorf("%s: err = %v, want ErrInvalidRecipient", name, err)
		}
	}

	// A private identity pasted by mistake is refused and never echoed.
	_, err := NormalizeRecipient(ident)
	if !errors.Is(err, ErrInvalidRecipient) {
		t.Fatalf("identity: err = %v", err)
	}
	if strings.Contains(err.Error(), ident) || strings.Contains(err.Error(), "AGE-SECRET-KEY") {
		t.Fatalf("error text leaks the identity: %v", err)
	}
}

func validInput(t *testing.T) Input {
	rec, _ := newRecipient(t)
	return Input{
		Endpoint: "https://s3.a.example", Bucket: "b", Region: "r", AccessKey: "ak",
		SecretKey: "sk", Recipient: rec, Preset: "off", Retention: 14,
	}
}

func TestPrepareValidation(t *testing.T) {
	in := validInput(t)

	bad := in
	bad.Bucket = "  "
	if _, err := Prepare(Settings{}, bad); !errors.Is(err, ErrBucketRequired) {
		t.Errorf("bucket: %v", err)
	}
	for _, n := range []int{0, -1, 1001} {
		bad = in
		bad.Retention = n
		if _, err := Prepare(Settings{}, bad); !errors.Is(err, ErrRetention) {
			t.Errorf("retention %d: %v", n, err)
		}
	}
	bad = in
	bad.Preset, bad.CustomCron = "custom", "bad"
	if _, err := Prepare(Settings{}, bad); !errors.Is(err, ErrInvalidSchedule) {
		t.Errorf("schedule: %v", err)
	}
	bad = in
	bad.Recipient = "ssh-ed25519 AAAA"
	if _, err := Prepare(Settings{}, bad); !errors.Is(err, ErrInvalidRecipient) {
		t.Errorf("recipient: %v", err)
	}

	ok := in
	ok.Preset = "daily"
	ok.Recipient = "  # public key: " + in.Recipient + "\n"
	got, err := Prepare(Settings{}, ok)
	if err != nil {
		t.Fatal(err)
	}
	if got.Schedule != "0 3 * * *" || got.Recipient != in.Recipient || got.Enabled {
		t.Errorf("got %+v", got)
	}

	// Enabled is carried over, never set.
	got, err = Prepare(Settings{Enabled: true}, ok)
	if err != nil || !got.Enabled {
		t.Errorf("enabled not carried: %+v, %v", got, err)
	}
}

func TestPrepareSecretKeyBinding(t *testing.T) {
	in := validInput(t)
	in.SecretKey = ""
	stored := Settings{Endpoint: "https://s3.a.example:443/x", HasSecretKey: true, SecretKey: "stored"}

	cases := []struct {
		endpoint string
		keep     bool
	}{
		{"https://s3.a.example/y", true},
		{"https://S3.A.example:443", true},
		{"https://s3.b.example", false},
		{"http://s3.a.example", false},
		{"https://s3.a.example:9000", false},
		{"", false},
	}
	for _, c := range cases {
		x := in
		x.Endpoint = c.endpoint
		got, err := Prepare(stored, x)
		if c.keep {
			if err != nil || got.SecretKey != "stored" || !got.HasSecretKey {
				t.Errorf("%q: got %+v, %v; want key kept", c.endpoint, got, err)
			}
		} else if !errors.Is(err, ErrSecretKeyRequired) {
			t.Errorf("%q: err = %v, want ErrSecretKeyRequired", c.endpoint, err)
		}
	}

	// A given secret is always used.
	x := in
	x.Endpoint, x.SecretKey = "https://s3.b.example", "new"
	got, err := Prepare(stored, x)
	if err != nil || got.SecretKey != "new" {
		t.Errorf("given secret: %+v, %v", got, err)
	}

	// AWS (empty endpoint) on both sides keeps the key.
	x = in
	x.Endpoint = ""
	got, err = Prepare(Settings{HasSecretKey: true, SecretKey: "stored"}, x)
	if err != nil || got.SecretKey != "stored" {
		t.Errorf("aws: %+v, %v", got, err)
	}

	// Nothing stored and nothing given.
	if _, err := Prepare(Settings{}, in); !errors.Is(err, ErrSecretKeyRequired) {
		t.Errorf("no key at all: %v", err)
	}
}
