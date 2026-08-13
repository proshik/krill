package api

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
)

// DeployAccepted is the response to Deploy/Rebuild: the operation is
// asynchronous, so both return immediately with the id of the deployment they
// just queued rather than blocking until the build/pull and rolling update
// converge (which can take minutes). Callers poll DeploymentStatus.
type DeployAccepted struct {
	DeploymentID int64  `json:"deployment_id"`
	Status       string `json:"status"`
}

// envKeyRe is the accepted shape of an env var name: a shell-safe identifier,
// matching what the web UI's own env editor already requires.
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Deploy triggers a new deployment of ref. When tag is non-empty the
// application's image is retagged first — that only makes sense for a
// source_type=image app (a dockerfile app has no image/tag of its own to
// move; its source is a git ref, and the operation that rebuilds it is
// Rebuild). Deploy never waits for convergence: it enqueues the job and
// returns the new deployment id immediately, so an agent calling this
// synchronously mid-build can poll DeploymentStatus instead of blocking for
// however long the build takes.
func (s *Service) Deploy(ctx context.Context, id Identity, ref, tag string) (DeployAccepted, error) {
	// requireWrite must run before resolveApp: resolving first would let a
	// read-only token distinguish "app not found" from "app found but
	// forbidden" and enumerate the org's applications through that difference.
	if err := requireWrite(id); err != nil {
		return DeployAccepted{}, err
	}
	app, err := s.resolveApp(ctx, id, ref)
	if err != nil {
		return DeployAccepted{}, err
	}

	if tag != "" {
		if app.SourceType != "image" {
			return DeployAccepted{}, Invalid("tag applies to image apps only; this is a dockerfile app — use rebuild to build a new image from source")
		}
		if err := s.q.UpdateApplicationImage(ctx, db.UpdateApplicationImageParams{
			ID:    app.ID,
			Image: app.Image,
			Tag:   tag,
		}); err != nil {
			return DeployAccepted{}, fmt.Errorf("deploy: update image: %w", err)
		}
	}

	if s.dep == nil {
		return DeployAccepted{}, fmt.Errorf("deploy: no deployer configured")
	}
	deployID := s.dep.Enqueue(app.ID, "manual")
	if deployID == 0 {
		return DeployAccepted{}, fmt.Errorf("deploy: could not enqueue a deployment (queue full or server shutting down)")
	}
	return DeployAccepted{DeploymentID: deployID, Status: "running"}, nil
}

// Rebuild forces a from-scratch build (docker build --no-cache) of a
// source_type=dockerfile app. It makes no sense for an image app, which has
// no source to rebuild from — the equivalent operation there is Deploy with a
// new tag. Like Deploy, it is asynchronous: it enqueues and returns the new
// deployment id without waiting for the build to finish.
func (s *Service) Rebuild(ctx context.Context, id Identity, ref string) (DeployAccepted, error) {
	if err := requireWrite(id); err != nil {
		return DeployAccepted{}, err
	}
	app, err := s.resolveApp(ctx, id, ref)
	if err != nil {
		return DeployAccepted{}, err
	}
	if app.SourceType != "dockerfile" {
		return DeployAccepted{}, Invalid("rebuild applies to dockerfile apps only; this is an image app — use deploy with a tag to move it to a new image")
	}

	if s.dep == nil {
		return DeployAccepted{}, fmt.Errorf("rebuild: no deployer configured")
	}
	deployID := s.dep.EnqueueRebuild(app.ID, "manual")
	if deployID == 0 {
		return DeployAccepted{}, fmt.Errorf("rebuild: could not enqueue a deployment (queue full or server shutting down)")
	}
	return DeployAccepted{DeploymentID: deployID, Status: "running"}, nil
}

// Reload force-restarts the application's current tasks in place — same
// image, same config, no rebuild and no pull. Useful when a linked resource
// changed (e.g. a DNS-resolved dependency) and the running process just needs
// a bounce.
func (s *Service) Reload(ctx context.Context, id Identity, ref string) error {
	if err := requireWrite(id); err != nil {
		return err
	}
	app, err := s.resolveApp(ctx, id, ref)
	if err != nil {
		return err
	}
	if s.engine == nil {
		return fmt.Errorf("reload: no docker engine configured")
	}
	if err := s.engine.ServiceRestart(ctx, docker.ServiceName(app.ID)); err != nil {
		return fmt.Errorf("reload: service restart: %w", err)
	}
	return nil
}

// Stop scales the application's service to zero replicas. The service
// definition and its history are untouched — Deploy brings it back up.
func (s *Service) Stop(ctx context.Context, id Identity, ref string) error {
	if err := requireWrite(id); err != nil {
		return err
	}
	app, err := s.resolveApp(ctx, id, ref)
	if err != nil {
		return err
	}
	if s.engine == nil {
		return fmt.Errorf("stop: no docker engine configured")
	}
	if err := s.engine.ServiceScale(ctx, docker.ServiceName(app.ID), 0); err != nil {
		return fmt.Errorf("stop: service scale: %w", err)
	}
	return nil
}

// editEnvLine makes a single targeted edit to raw env_text and returns the
// resulting text: it finds the line whose key matches, replaces just its
// value, deletes the line entirely when remove is true, or appends a new
// "key=value" line at the end when the key is not present yet — every other
// line (order, comments, blank lines, other keys) passes through unchanged.
//
// env_text is the single source of truth for an app's environment (the old
// JSONB map column was dropped for exactly this reason: parsing the whole
// file into a map and writing it back out would silently discard the user's
// comments and reordering, and would clobber any concurrent edit made through
// the web UI). A key that appears more than once in the existing text is
// already an inconsistent file — refuse to guess which occurrence was meant
// and let the caller resolve it directly instead.
func editEnvLine(text, key, value string, remove bool) (string, error) {
	lines := strings.Split(text, "\n")
	// A genuinely empty file splits into one empty-string element; drop it so
	// appending the first key doesn't leave a stray leading blank line.
	if len(lines) == 1 && lines[0] == "" {
		lines = lines[:0]
	}

	matchIdx := -1
	matches := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		k, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(k) == key {
			matches++
			matchIdx = i
		}
	}
	if matches > 1 {
		return "", Conflict(fmt.Sprintf("key %q appears more than once in env_text; edit the file directly to resolve the duplicate before setting it through the API", key))
	}

	switch {
	case remove:
		if matchIdx >= 0 {
			lines = append(lines[:matchIdx], lines[matchIdx+1:]...)
		}
	case matchIdx >= 0:
		lines[matchIdx] = key + "=" + value
	default:
		lines = append(lines, key+"="+value)
	}
	return strings.Join(lines, "\n"), nil
}

// SetEnv makes a single targeted edit to an application's environment: set
// (or add) one key's value, or remove one key, without touching any other
// line of env_text. See editEnvLine for why this must be a line-level edit
// rather than a round-trip through a parsed map.
func (s *Service) SetEnv(ctx context.Context, id Identity, ref, key, value string, remove bool) error {
	if err := requireWrite(id); err != nil {
		return err
	}
	app, err := s.resolveApp(ctx, id, ref)
	if err != nil {
		return err
	}
	if !envKeyRe.MatchString(key) {
		return Invalid(fmt.Sprintf("invalid env key %q: must match %s", key, envKeyRe.String()))
	}

	newText, err := editEnvLine(app.EnvText, key, value, remove)
	if err != nil {
		return err
	}
	if err := s.q.UpdateApplicationEnv(ctx, db.UpdateApplicationEnvParams{ID: app.ID, EnvText: newText}); err != nil {
		return fmt.Errorf("set env: update: %w", err)
	}
	return nil
}
