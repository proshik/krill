package api

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/envtext"
	"github.com/proshik/krill/internal/webhook"
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

	if tag != "" && app.SourceType != "image" {
		return DeployAccepted{}, Invalid("tag applies to image apps only; this is a dockerfile app — use rebuild to build a new image from source")
	}
	// Validate the tag with the same rule the CI deploy-hook applies to its
	// ?tag= parameter (webhook.ValidTag): an LLM caller is exactly the client
	// likely to send "v1.2.3 " with a stray space, or a whole image reference
	// like "ghcr.io/acme/bot:v1" where only the tag belongs. Both must be
	// refused BEFORE the write, because the damage here is persistent: the row
	// would keep a broken image reference that every later deployment reuses —
	// including a human pressing Deploy in the web UI — and this API offers no
	// way to put the old tag back.
	if tag != "" && !webhook.ValidTag(tag) {
		return DeployAccepted{}, Invalid(fmt.Sprintf("invalid image tag %q: a tag is the part after the colon (e.g. \"v1.2.3\", \"latest\"), at most 128 characters of letters, digits, '.', '_' and '-' — not a full image reference and with no spaces", tag))
	}

	// Checked before the retag, not after: a server with no deployer can do
	// nothing with a new tag, and failing afterwards would leave the app
	// pointing at an image that was never deployed.
	if s.dep == nil {
		return DeployAccepted{}, fmt.Errorf("deploy: no deployer configured")
	}

	// Checked before the retag for the same reason the nil-deployer check is,
	// and it is the more likely of the two to fire: Enqueue refuses a second
	// deployment while one is already in flight, and learning that only
	// afterwards would leave the application pointing at a tag nothing ever
	// deployed. That is not a transient failure the caller can shrug off —
	// the retag is persistent, this API offers no way to put the old tag
	// back, and every later deployment reuses it, including one a human
	// starts from the web UI.
	if cErr := s.inFlightConflict(ctx, app.ID, "deploy"); cErr != nil {
		return DeployAccepted{}, cErr
	}

	if tag != "" {
		if err := s.q.UpdateApplicationImage(ctx, db.UpdateApplicationImageParams{
			ID:    app.ID,
			Image: app.Image,
			Tag:   tag,
		}); err != nil {
			return DeployAccepted{}, fmt.Errorf("deploy: update image: %w", err)
		}
	}

	deployID := s.dep.Enqueue(app.ID, "manual")
	if deployID == 0 {
		// The check above closes the ordinary conflict, but Enqueue also
		// returns 0 with a full queue, on shutdown, and when it cannot write
		// the deployment row — none of which is a conflict, and all of which
		// would otherwise leave the tag moved with no deployment behind it.
		// Put it back: a persistent retag nothing deployed is the one outcome
		// this operation must never produce, because the next Deploy from the
		// web UI would ship it.
		s.restoreTag(ctx, app, tag)
		if cErr := s.inFlightConflict(ctx, app.ID, "deploy"); cErr != nil {
			return DeployAccepted{}, cErr
		}
		return DeployAccepted{}, fmt.Errorf("deploy: could not enqueue a deployment (queue full or server shutting down)")
	}
	return DeployAccepted{DeploymentID: deployID, Status: "running"}, nil
}

// restoreTag undoes a retag whose deployment never got queued.
//
// Best-effort by construction: if this write fails too, the row keeps the new
// tag and only a log line records it. A stronger guarantee would need the tag
// to travel WITH the job rather than through the application row — the
// deployer reads applications.tag when the worker picks the job up, so two
// deploys racing here still resolve to whichever tag was written last. That is
// a deployer-shaped change, not an API-shaped one; this narrows the window to
// a failed enqueue and says so rather than pretending the race is closed.
func (s *Service) restoreTag(ctx context.Context, app db.Application, attempted string) {
	if attempted == "" || attempted == app.Tag {
		return
	}
	if err := s.q.UpdateApplicationImage(ctx, db.UpdateApplicationImageParams{
		ID:    app.ID,
		Image: app.Image,
		Tag:   app.Tag,
	}); err != nil {
		slog.Error("deploy: could not restore the previous image tag after a failed enqueue",
			"app_id", app.ID, "attempted_tag", attempted, "previous_tag", app.Tag, "err", err)
	}
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
		if cErr := s.inFlightConflict(ctx, app.ID, "rebuild"); cErr != nil {
			return DeployAccepted{}, cErr
		}
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

// inFlightConflict reports a deploy that cannot start because one is already
// running for the app. The deployer enforces the cap itself; this turns its
// bare rejection into an answer the caller can act on instead of the generic
// "could not enqueue", which would send an agent into exactly the retry loop
// the cap exists to stop.
func (s *Service) inFlightConflict(ctx context.Context, appID int64, op string) error {
	n, err := s.q.CountRunningDeploymentsByApplication(ctx, appID)
	if err != nil {
		// Reporting "no conflict" on a failed read is the wrong default for
		// the pre-retag call: it would let an unreadable database wave through
		// exactly the persistent retag the check exists to prevent. Fail
		// closed — the caller retries, and a retry costs nothing.
		return fmt.Errorf("%s: could not check for a deployment already in progress: %w", op, err)
	}
	if n == 0 {
		return nil
	}
	return Conflict(op + ": a deployment for this application is already in progress; poll krill_deployment_status and start another once it finishes")
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

	// envtext.KeyOf is the same predicate the deploy path uses to decide what a
	// variable line is, so an edit here cannot disagree with what the container
	// will actually receive.
	matchIdx := -1
	matches := 0
	for i, line := range lines {
		k, ok := envtext.KeyOf(line)
		if !ok {
			continue
		}
		if k == key {
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
//
// The edit is persisted only — the running container keeps the OLD value
// until the app is deployed again (env vars are baked into the Swarm service
// spec at deploy time). Callers that need the change live must follow with
// Deploy. This deliberately does NOT redeploy on its own: a write that
// silently restarts a production service would be the worse surprise, and it
// would make setting three variables cost three rolling updates.
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
	// The value is appended verbatim to a KEY=VALUE line, so a newline in it
	// would not store a multi-line value — it would inject extra LINES into
	// env_text. deploy.parseEnvText would then read the first line as this
	// key and every following line as a separate (bogus) variable, and if one
	// of those repeats an existing key the file becomes a duplicate that both
	// editEnvLine (Conflict) and the web UI's saveEnv refuse to touch until a
	// human hand-edits it. Multi-line secrets (PEM keys, service-account JSON)
	// are an ordinary thing for an agent to try, so refuse them explicitly
	// instead of silently corrupting the file. remove ignores value entirely.
	if !remove && strings.ContainsAny(value, "\n\r") {
		return Invalid(fmt.Sprintf("invalid value for env key %q: must be a single line (no newline or carriage return) — this operation changes exactly one KEY=VALUE line; for a multi-line value such as a PEM key, store it encoded (e.g. base64) on one line", key))
	}

	if !remove && app.MetricsEnabled && key == app.MetricsTokenEnv {
		return Conflict(fmt.Sprintf("%s is the app's metrics token variable; rename it on the Metrics tab or pick another key", key))
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
