package cliconfig_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/krillcli/cliconfig"
)

// withHome points the store at a directory that does NOT exist yet, so Save
// is the thing that creates it and its permissions are the ones under test.
// (An existing directory is left alone on purpose: if a user points
// KRILL_CLI_HOME somewhere of their own, its mode is their business — the
// token is protected by the file's own 0600.)
func withHome(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "krill")
	t.Setenv(cliconfig.EnvHome, dir)
	return dir
}

func TestSaveIsAtomicAndOwnerOnly(t *testing.T) {
	dir := withHome(t)
	s, err := cliconfig.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	s.Set("prod", cliconfig.Context{Server: "https://krill.example.com", Token: "krill_pat_a"})
	s.Set("staging", cliconfig.Context{Server: "https://dev.example.com", Token: "krill_pat_b"})
	if err := cliconfig.Save(s); err != nil {
		t.Fatalf("save: %v", err)
	}

	p := filepath.Join(dir, cliconfig.FileName)
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// The file holds bearer tokens.
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("config mode = %o, want 0600", perm)
	}
	if dst, err := os.Stat(dir); err == nil && dst.Mode().Perm()&0o077 != 0 {
		t.Fatalf("config dir mode = %o, want no group/other access", dst.Mode().Perm())
	}
	// The temp file used for the atomic rename must not be left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

// TestSaveKeepsOtherContexts is the reason the write is atomic rather than a
// truncate in place: one login must never be able to lose the others.
func TestSaveKeepsOtherContexts(t *testing.T) {
	withHome(t)
	s, _ := cliconfig.Load()
	s.Set("prod", cliconfig.Context{Server: "https://a", Token: "t1"})
	s.Set("staging", cliconfig.Context{Server: "https://b", Token: "t2"})
	if err := cliconfig.Save(s); err != nil {
		t.Fatalf("save: %v", err)
	}

	reloaded, err := cliconfig.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	reloaded.Set("third", cliconfig.Context{Server: "https://c", Token: "t3"})
	if err := cliconfig.Save(reloaded); err != nil {
		t.Fatalf("save 2: %v", err)
	}

	final, _ := cliconfig.Load()
	if len(final.Contexts) != 3 {
		t.Fatalf("want 3 contexts, got %v", final.Names())
	}
	if final.Contexts["prod"].Token != "t1" {
		t.Fatal("an earlier context lost its token")
	}
}

func TestLoadMissingFileIsEmptyNotAnError(t *testing.T) {
	withHome(t)
	s, err := cliconfig.Load()
	if err != nil {
		t.Fatalf("a first run must not fail: %v", err)
	}
	if len(s.Contexts) != 0 || s.Current != "" {
		t.Fatalf("want an empty store, got %+v", s)
	}
}

func TestCurrentFollowsAddAndRemove(t *testing.T) {
	withHome(t)
	s, _ := cliconfig.Load()

	s.Set("prod", cliconfig.Context{Server: "https://a", Token: "t"})
	if s.Current != "prod" {
		t.Fatalf("first context should become current, got %q", s.Current)
	}
	s.Set("staging", cliconfig.Context{Server: "https://b", Token: "t"})
	if s.Current != "prod" {
		t.Fatalf("adding a second context must not steal current, got %q", s.Current)
	}
	if err := s.Use("staging"); err != nil {
		t.Fatalf("use: %v", err)
	}
	if err := s.Use("nope"); err == nil {
		t.Fatal("want an error for an unknown context")
	}
	if err := s.Remove("staging"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if s.Current != "prod" {
		t.Fatalf("removing the current context should repoint it, got %q", s.Current)
	}
	if err := s.Remove("staging"); err == nil {
		t.Fatal("removing a missing context should fail")
	}
}

func TestResolvePrecedence(t *testing.T) {
	store := &cliconfig.Store{
		Current: "prod",
		Contexts: map[string]cliconfig.Context{
			"prod":    {Server: "https://prod", Token: "tok-prod", Level: "write", OrgName: "acme"},
			"staging": {Server: "https://staging", Token: "tok-staging", Level: "read"},
		},
	}

	tests := []struct {
		name       string
		opts       cliconfig.Options
		wantServer string
		wantToken  string
		wantName   string
	}{
		{
			name:       "stored current by default",
			wantServer: "https://prod", wantToken: "tok-prod", wantName: "prod",
		},
		{
			name:       "project pin beats stored current",
			opts:       cliconfig.Options{ProjectContext: "staging"},
			wantServer: "https://staging", wantToken: "tok-staging", wantName: "staging",
		},
		{
			name:       "env context beats the project pin",
			opts:       cliconfig.Options{ProjectContext: "staging", EnvContext: "prod"},
			wantServer: "https://prod", wantToken: "tok-prod", wantName: "prod",
		},
		{
			name:       "flag beats everything",
			opts:       cliconfig.Options{ProjectContext: "prod", EnvContext: "prod", ContextFlag: "staging"},
			wantServer: "https://staging", wantToken: "tok-staging", wantName: "staging",
		},
		{
			// A CI runner with a token and a server and no config file. The
			// store below has contexts, so this case is covered separately in
			// TestResolveEnvTokenWithNoStoredContexts.
			name:       "env token with an explicit server",
			opts:       cliconfig.Options{EnvToken: "ci-token", EnvServer: "https://ci"},
			wantServer: "https://ci", wantToken: "ci-token", wantName: "prod",
		},
		{
			// Same server, different token — a short-lived CI credential
			// against an already-configured host.
			name:       "env token inherits the current server",
			opts:       cliconfig.Options{EnvToken: "ci-token"},
			wantServer: "https://prod", wantToken: "ci-token", wantName: "prod",
		},
		{
			// The regression this ordering exists for: an exported token must
			// not also decide WHICH server it is sent to. It used to
			// short-circuit every selector, so a staging token plus --context
			// staging was transmitted to whatever host happened to be current.
			name:       "env token does not override the chosen context",
			opts:       cliconfig.Options{EnvToken: "ci-token", ContextFlag: "staging"},
			wantServer: "https://staging", wantToken: "ci-token", wantName: "staging",
		},
		{
			name:       "env token does not override the project pin",
			opts:       cliconfig.Options{EnvToken: "ci-token", ProjectContext: "staging"},
			wantServer: "https://staging", wantToken: "ci-token", wantName: "staging",
		},
		{
			name:       "server flag overrides the stored server",
			opts:       cliconfig.Options{ServerFlag: "https://other"},
			wantServer: "https://other", wantToken: "tok-prod", wantName: "prod",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cliconfig.Resolve(store, tc.opts)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got.Server != tc.wantServer || got.Token != tc.wantToken || got.Name != tc.wantName {
				t.Fatalf("resolve = %+v, want server %q token %q name %q",
					got, tc.wantServer, tc.wantToken, tc.wantName)
			}
		})
	}
}

// TestResolveEnvTokenWithNoStoredContexts covers the CI shape: no config file
// on disk at all, credentials entirely from the environment.
func TestResolveEnvTokenWithNoStoredContexts(t *testing.T) {
	empty := &cliconfig.Store{Contexts: map[string]cliconfig.Context{}}

	got, err := cliconfig.Resolve(empty, cliconfig.Options{EnvToken: "ci-token", EnvServer: "https://ci"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Server != "https://ci" || got.Token != "ci-token" || got.Name != "" {
		t.Fatalf("resolve = %+v, want an unnamed context on https://ci", got)
	}

	if _, err := cliconfig.Resolve(empty, cliconfig.Options{EnvToken: "ci-token"}); err == nil {
		t.Fatal("a token with no server and no stored context should fail")
	}
}

// TestResolveEnvTokenDoesNotInventAContext pins the other half: naming a
// context that does not exist is an error even with a token exported, rather
// than silently falling back to whichever one is current.
func TestResolveEnvTokenDoesNotInventAContext(t *testing.T) {
	store := &cliconfig.Store{
		Current:  "prod",
		Contexts: map[string]cliconfig.Context{"prod": {Server: "https://prod", Token: "tok"}},
	}
	_, err := cliconfig.Resolve(store, cliconfig.Options{EnvToken: "ci-token", ContextFlag: "staging"})
	if err == nil {
		t.Fatal("want an error for an unknown context name")
	}
	if !strings.Contains(err.Error(), "staging") {
		t.Fatalf("the error should name the missing context, got %q", err)
	}
}

// TestResolveEnvTokenDropsTheCachedLevel: the stored level describes the
// stored token. Carrying it over would let a cached "read" refuse a write
// token the environment just supplied, before a single request.
func TestResolveEnvTokenDropsTheCachedLevel(t *testing.T) {
	store := &cliconfig.Store{
		Current:  "ro",
		Contexts: map[string]cliconfig.Context{"ro": {Server: "https://k", Token: "tok", Level: "read"}},
	}
	got, err := cliconfig.Resolve(store, cliconfig.Options{EnvToken: "write-token"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Level != "" {
		t.Fatalf("level = %q, want it cleared", got.Level)
	}
	if !got.CanWriteHint() {
		t.Fatal("an environment token must not be refused on the stored token's level")
	}
}

func TestResolveErrors(t *testing.T) {
	empty := &cliconfig.Store{Contexts: map[string]cliconfig.Context{}}
	if _, err := cliconfig.Resolve(empty, cliconfig.Options{}); err == nil {
		t.Fatal("want an error when not logged in")
	} else if !strings.Contains(err.Error(), "krill-cli login") {
		t.Fatalf("the error should say how to log in, got %q", err)
	}

	if _, err := cliconfig.Resolve(empty, cliconfig.Options{EnvToken: "t"}); err == nil {
		t.Fatal("want an error when a token has no server")
	}

	store := &cliconfig.Store{
		Current:  "prod",
		Contexts: map[string]cliconfig.Context{"prod": {Server: "https://prod", Token: "t"}},
	}
	if _, err := cliconfig.Resolve(store, cliconfig.Options{ContextFlag: "nope"}); err == nil {
		t.Fatal("want an error for an unknown context")
	}
	// A project pinned to a context this machine has not logged in to gets a
	// different message: the fix is a login, not a typo correction.
	_, err := cliconfig.Resolve(store, cliconfig.Options{ProjectContext: "staging"})
	if err == nil || !strings.Contains(err.Error(), "pins this project") {
		t.Fatalf("want the project-pin message, got %v", err)
	}

	noToken := &cliconfig.Store{
		Current:  "prod",
		Contexts: map[string]cliconfig.Context{"prod": {Server: "https://prod"}},
	}
	if _, err := cliconfig.Resolve(noToken, cliconfig.Options{}); err == nil {
		t.Fatal("want an error for a context with no token")
	}
}

// TestCanWriteHintOnlyRulesOut documents that the cache is a fast local NO
// and never a yes: rights are re-resolved server-side on every request.
func TestCanWriteHintOnlyRulesOut(t *testing.T) {
	if (cliconfig.Resolved{Level: "read"}).CanWriteHint() {
		t.Fatal("a cached read level must rule writing out locally")
	}
	for _, lvl := range []string{"write", ""} {
		if !(cliconfig.Resolved{Level: lvl}).CanWriteHint() {
			t.Fatalf("level %q must not be treated as read-only", lvl)
		}
	}
}
