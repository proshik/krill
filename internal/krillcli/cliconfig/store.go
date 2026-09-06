// Package cliconfig stores which Krill servers the user is logged in to.
//
// It holds the credential half of the configuration; the project half lives
// in the repository's krill.yaml, which is committed and must never carry a
// token.
package cliconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FileName is the store inside the config directory.
const FileName = "config.json"

// EnvHome overrides the whole config directory. Beyond letting a user move
// it, this is what makes the package testable: point it at t.TempDir() and
// nothing touches the developer's real login.
const EnvHome = "KRILL_CLI_HOME"

// Context is one Krill server the user is logged in to.
type Context struct {
	Server string `json:"server"`
	Token  string `json:"token"`

	// Everything below is a cache of the whoami taken at login, kept for
	// display and for one fast local refusal (see Resolved.CanWrite). It is
	// never trusted for authorization: the server re-resolves a token's
	// rights on every request from the owner's CURRENT role, so this copy
	// goes stale the moment somebody is demoted.
	UserID    int64  `json:"user_id,omitempty"`
	OrgID     int64  `json:"org_id,omitempty"`
	OrgName   string `json:"org_name,omitempty"`
	Level     string `json:"level,omitempty"`
	CheckedAt string `json:"checked_at,omitempty"`
}

// Store is the config file.
type Store struct {
	Version  int                `json:"version"`
	Current  string             `json:"current"`
	Contexts map[string]Context `json:"contexts"`
}

// Dir returns the configuration directory.
//
// It is ~/.config/krill (honouring XDG_CONFIG_HOME), NOT os.UserConfigDir().
// On macOS that function returns ~/Library/Application Support/krill, which
// is somewhere nobody looks when a token has gone stale — whereas every
// neighbouring tool a developer already has (~/.docker, ~/.kube,
// ~/.config/gh) keeps its config somewhere they can cat it.
func Dir() (string, error) {
	if h := os.Getenv(EnvHome); h != "" {
		return h, nil
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "krill"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine your home directory: %w", err)
	}
	return filepath.Join(home, ".config", "krill"), nil
}

// Path returns the config file path.
func Path() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, FileName), nil
}

// Load reads the store. A missing file is an empty store, not an error: that
// is the state before the first login.
func Load() (*Store, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return &Store{Version: 1, Contexts: map[string]Context{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	var s Store
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", p, err)
	}
	if s.Contexts == nil {
		s.Contexts = map[string]Context{}
	}
	s.Version = 1
	return &s, nil
}

// Save writes the store atomically with owner-only permissions.
//
// Atomic because the file holds every context: a login interrupted halfway
// through a truncating write would leave the user logged out of servers they
// never touched.
func Save(s *Store) error {
	p, err := Path()
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	// Same directory, so the rename cannot cross a filesystem boundary.
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, p)
}

// Names returns the context names in a stable order.
func (s *Store) Names() []string {
	out := make([]string, 0, len(s.Contexts))
	for n := range s.Contexts {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Set adds or replaces a context, making it current when it is the only one.
func (s *Store) Set(name string, c Context) {
	if s.Contexts == nil {
		s.Contexts = map[string]Context{}
	}
	s.Contexts[name] = c
	if s.Current == "" || len(s.Contexts) == 1 {
		s.Current = name
	}
}

// Remove deletes a context and repoints Current if it pointed there.
func (s *Store) Remove(name string) error {
	if _, ok := s.Contexts[name]; !ok {
		return fmt.Errorf("no context named %q", name)
	}
	delete(s.Contexts, name)
	if s.Current == name {
		s.Current = ""
		if names := s.Names(); len(names) > 0 {
			s.Current = names[0]
		}
	}
	return nil
}

// Use makes name current.
func (s *Store) Use(name string) error {
	if _, ok := s.Contexts[name]; !ok {
		return fmt.Errorf("no context named %q; run `krill-cli context` to list them", name)
	}
	s.Current = name
	return nil
}

// InsecurePermissions reports whether the config file is readable by anyone
// other than its owner. Worth a warning, never worth refusing to run: a
// refusal would strand somebody with no way forward.
func InsecurePermissions() (bool, string, error) {
	p, err := Path()
	if err != nil {
		return false, "", err
	}
	st, err := os.Stat(p)
	if err != nil {
		return false, "", err
	}
	return st.Mode().Perm()&0o077 != 0, p, nil
}

func trimmed(s string) string { return strings.TrimSpace(s) }
