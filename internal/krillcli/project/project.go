// Package project reads krill.yaml, the committed per-repository file that
// says which Krill application this source tree deploys to and how to build
// it.
//
// The file carries no secrets — the server URL and API token live in the
// per-machine config — and strict decoding is what keeps it that way: an
// unknown key is an error, so a `token:` somebody adds "just for CI" fails
// loudly instead of being silently ignored and then committed.
package project

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/proshik/krill/internal/krillcli/dockercli"
	"github.com/proshik/krill/internal/krillcli/imageref"
	"gopkg.in/yaml.v3"
)

// FileName is the file this package looks for.
const FileName = "krill.yaml"

// Delivery modes.
const (
	DeliveryRegistry = "registry"
	DeliveryUpload   = "upload"
)

// DefaultPlatform is the platform builds target when krill.yaml does not say.
//
// It is a fixed value, NOT the host's. Almost every Krill install runs on an
// amd64 VPS while a good share of developers are on arm64 Macs, and building
// for the host produces an image that loads fine and dies on start with
// "exec format error" — a failure that surfaces minutes later, in the
// container log, far from its cause. Defaulting to the common server
// architecture makes the rare case (an arm64 server) the one that has to
// write a line, and that line is visible in a committed file. The API cannot
// settle this for us: it reports a node's name, never its architecture.
const DefaultPlatform = "linux/amd64"

// Config is krill.yaml.
type Config struct {
	App      string `yaml:"app"`
	Image    Image  `yaml:"image"`
	Build    Build  `yaml:"build"`
	Tag      Tag    `yaml:"tag"`
	Delivery string `yaml:"delivery"`
	// Context pins this repository to one login context, so a shell pointed
	// at production cannot ship a staging-only project by accident.
	Context string `yaml:"context"`

	// Dir is where the file was found; build paths are relative to it.
	Dir string `yaml:"-"`
	// Path is the file itself, for error messages.
	Path string `yaml:"-"`
}

type Image struct {
	Repository string `yaml:"repository"`
	Platform   string `yaml:"platform"`
}

type Build struct {
	Context    string            `yaml:"context"`
	Dockerfile string            `yaml:"dockerfile"`
	Args       map[string]string `yaml:"args"`
}

type Tag struct {
	Strategy     string `yaml:"strategy"`
	Prefix       string `yaml:"prefix"`
	RequireClean bool   `yaml:"require_clean"`
}

// ErrNotFound means no krill.yaml was found in this directory or above it.
var ErrNotFound = errors.New("no " + FileName + " found")

// Find walks up from start to the filesystem root looking for krill.yaml and
// returns the NEAREST one. Nearest, not outermost, so a monorepo holding one
// file per service does the right thing when run from inside any of them.
func Find(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, FileName)
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ErrNotFound
		}
		dir = parent
	}
}

// Load reads and validates the file at path.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var c Config
	dec := yaml.NewDecoder(f)
	// The whole point: an unrecognized key is a mistake worth reporting, not
	// something to skip. It catches "platfrom:", it catches a stray "token:",
	// and it catches a field from a newer CLI that this one cannot honour.
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}

	c.Path = path
	c.Dir = filepath.Dir(path)
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Image.Platform == "" {
		c.Image.Platform = DefaultPlatform
	}
	if c.Build.Context == "" {
		c.Build.Context = "."
	}
	if c.Build.Dockerfile == "" {
		c.Build.Dockerfile = "Dockerfile"
	}
	if c.Tag.Strategy == "" {
		c.Tag.Strategy = "git"
	}
	if c.Delivery == "" {
		c.Delivery = DeliveryRegistry
	}
}

// Validate reports the first problem, phrased against the file rather than
// against internal state.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.App) == "" {
		return errors.New(`"app" is required: the project/environment/app path, e.g. acme/production/bot`)
	}
	if err := validAppRef(c.App); err != nil {
		return err
	}
	if strings.TrimSpace(c.Image.Repository) == "" {
		return errors.New(`"image.repository" is required: the repository Krill pulls from, e.g. ghcr.io/acme/bot`)
	}
	if _, err := imageref.NormalizeRepo(c.Image.Repository); err != nil {
		return fmt.Errorf(`"image.repository": %w`, err)
	}
	if c.Delivery != DeliveryRegistry && c.Delivery != DeliveryUpload {
		return fmt.Errorf(`"delivery" must be %q or %q, got %q`, DeliveryRegistry, DeliveryUpload, c.Delivery)
	}
	if c.Tag.Strategy != "git" && c.Tag.Strategy != "timestamp" {
		return fmt.Errorf(`"tag.strategy" must be "git" or "timestamp", got %q`, c.Tag.Strategy)
	}
	if c.Tag.Prefix != "" && !imageref.ValidTag(c.Tag.Prefix+"x") {
		return fmt.Errorf(`"tag.prefix" %q cannot start an image tag: use letters, digits or underscore`, c.Tag.Prefix)
	}
	for k := range c.Build.Args {
		if err := dockercli.ValidateBuildArgKey(k); err != nil {
			return fmt.Errorf(`"build.args": %w`, err)
		}
	}
	if filepath.IsAbs(c.Build.Dockerfile) {
		return fmt.Errorf(`"build.dockerfile" must be relative to the project directory, got %q`, c.Build.Dockerfile)
	}
	return nil
}

// validAppRef accepts the two forms the server accepts, and rejects the
// almost-right two-segment path that is the common typo.
func validAppRef(ref string) error {
	if _, err := strconv.ParseInt(ref, 10, 64); err == nil {
		return nil
	}
	if n := strings.Count(ref, "/"); n != 2 {
		return fmt.Errorf(`"app" must be project/environment/app (three parts) or a numeric id, got %q`, ref)
	}
	for _, part := range strings.Split(ref, "/") {
		if strings.TrimSpace(part) == "" {
			return fmt.Errorf(`"app" has an empty path segment: %q`, ref)
		}
	}
	return nil
}

// BuildContextPath and DockerfilePath resolve the build inputs against the
// directory holding krill.yaml, so the CLI behaves the same run from the
// project root or from a subdirectory.
func (c *Config) BuildContextPath() string { return filepath.Join(c.Dir, c.Build.Context) }

func (c *Config) DockerfilePath() string {
	return filepath.Join(c.BuildContextPath(), c.Build.Dockerfile)
}
