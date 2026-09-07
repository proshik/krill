package deployflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/proshik/krill/internal/krillcli/client"
	"github.com/proshik/krill/internal/krillcli/dockercli"
	"github.com/proshik/krill/internal/krillcli/imageref"
	"github.com/proshik/krill/internal/krillcli/project"
)

// API is the part of the Krill client this flow uses. Narrow on purpose: the
// flow's tests supply a fake, and a narrow interface is a short fake.
type API interface {
	Whoami(ctx context.Context) (client.Whoami, error)
	AppStatus(ctx context.Context, ref string) (client.AppStatus, error)
	Deployments(ctx context.Context, ref string, limit int) ([]client.Deployment, error)
	Deployment(ctx context.Context, id int64) (client.DeploymentDetail, error)
	Deploy(ctx context.Context, ref, tag string) (client.Accepted, error)
}

// Docker is the part of the docker CLI this flow uses. Stream is deliberately
// absent: only the unimplemented upload path needs it, and putting it here
// would make every fake implement a method nothing calls.
type Docker interface {
	Run(ctx context.Context, args ...string) error
	Output(ctx context.Context, args ...string) (string, error)
}

// Options is one invocation of `krill-cli deploy`.
type Options struct {
	Ref        string // project/environment/app, or a numeric id
	Repository string
	Platform   string
	Tag        string
	Dockerfile string // absolute or relative to Context
	Context    string
	BuildArgs  map[string]string
	Delivery   string

	SkipBuild bool
	SkipPush  bool
	NoWatch   bool
	DryRun    bool
	NoCache   bool

	// AllowImageMismatch proceeds even when krill.yaml and the server
	// disagree about the repository — for a mirror, or a deliberate move.
	AllowImageMismatch bool
	// AllowMissingImage proceeds with --skip-push even when the tag cannot be
	// found in the registry, for a registry whose manifest endpoint this
	// client cannot query.
	AllowMissingImage bool
	// NoWaitForLock exits instead of waiting out somebody else's deployment.
	NoWaitForLock bool
	// CanWriteHint is false when the stored context is known to be a
	// read-only token, which lets the flow refuse without a single request.
	CanWriteHint bool

	Timeout time.Duration
}

// Flow carries the collaborators. Sleep is injectable so tests do not wait.
type Flow struct {
	API    API
	Docker Docker
	Out    io.Writer
	Sleep  func(ctx context.Context, d time.Duration) error
}

// Run executes the whole deploy and returns a *Failure carrying an exit code.
func (f *Flow) Run(ctx context.Context, o Options) error {
	if f.Sleep == nil {
		f.Sleep = sleep
	}
	if o.Timeout <= 0 {
		// Matches the deployer's own 10-minute job timeout. Deploys are
		// serialized by ONE worker for the whole server, so an image app can
		// be queued behind somebody's dockerfile build; a shorter wait would
		// time out on deploys that were always going to succeed.
		o.Timeout = 10 * time.Minute
	}

	// A read-only token cannot deploy, and the server checks the level before
	// it resolves the app — so there is no way to learn this later that does
	// not come after the build. Refuse now, with no request at all.
	if !o.CanWriteHint {
		return fail(ExitAuth, "this context holds a read-level token, which cannot deploy — create a write token in the Krill UI under Settings, API tokens")
	}

	app, tag, err := f.check(ctx, o)
	if err != nil {
		return err
	}
	ref, err := imageref.Join(o.Repository, tag)
	if err != nil {
		return fail(ExitUsage, "%v", err)
	}

	f.printPlan(o, app, tag, ref)
	if o.DryRun {
		f.printf("\nDry run: nothing was built, pushed or deployed.\n")
		return nil
	}

	if !o.SkipBuild {
		if err := f.build(ctx, o, ref); err != nil {
			return err
		}
	}
	if err := f.ship(ctx, o, ref); err != nil {
		return err
	}
	return f.deployAndWatch(ctx, o, tag)
}

// check runs every verification that can be done before the expensive part.
func (f *Flow) check(ctx context.Context, o Options) (client.AppStatus, string, error) {
	// Refused here rather than at the point of use, because the point of use
	// is after the build: this command's own promise is that everything which
	// can fail fails before a four-minute build, and an unimplemented delivery
	// mode is the most certain failure there is.
	if o.Delivery == project.DeliveryUpload {
		return client.AppStatus{}, "", fail(ExitUsage,
			"delivery: upload is not implemented yet — this build of krill-cli can only push to a registry.\n"+
				"Set delivery: registry in krill.yaml (or pass --registry) and give Krill a repository it can pull from.")
	}

	// Confirms the token works AND that its level survives the owner's
	// current role, which the cached hint above cannot.
	who, err := f.API.Whoami(ctx)
	if err != nil {
		return client.AppStatus{}, "", f.authAware(err, "cannot authenticate to Krill")
	}
	if !who.CanWrite {
		return client.AppStatus{}, "", fail(ExitAuth,
			"this token cannot write: level %q, role %q in org %q — a write token also needs its owner to be an admin of the org",
			who.Level, who.Role, who.OrgName)
	}

	app, err := f.API.AppStatus(ctx, o.Ref)
	if err != nil {
		var ae *client.APIError
		if errors.As(err, &ae) && ae.IsNotFound() {
			return client.AppStatus{}, "", fail(ExitUsage,
				"no application %q in org %q — run `krill-cli apps` to list them.\n"+
					"(An app whose name is one of logs, env, deployments, deploy, rebuild, reload or stop can only be addressed by its numeric id.)",
				o.Ref, who.OrgName)
		}
		return client.AppStatus{}, "", f.authAware(err, "cannot read the application")
	}

	if app.SourceType != "image" {
		return client.AppStatus{}, "", fail(ExitUsage,
			"%s is a %s app: it is built by Krill from git, so there is no local image to push.\n"+
				"Use `krill-cli rebuild %s` for that, or change the app to an image source in the Krill UI.",
			app.Path, app.SourceType, o.Ref)
	}

	// The mismatch that is otherwise invisible: pushing to one repository
	// while Krill pulls from another produces a green deploy of the OLD
	// image, which looks like the build silently did nothing.
	if !o.AllowImageMismatch && app.Image != "" && !imageref.SameRepo(app.Image, o.Repository) {
		return client.AppStatus{}, "", fail(ExitUsage,
			"repository mismatch — krill.yaml builds %q but %s deploys %q.\n"+
				"Pushing would upload an image Krill never pulls. Fix either side:\n"+
				"  • edit image.repository in krill.yaml, or\n"+
				"  • change the app's Image in the Krill UI (the API cannot change it)\n"+
				"Pass --allow-image-mismatch if this is deliberate.",
			o.Repository, app.Path, app.Image)
	}

	tag := o.Tag
	if !imageref.ValidTag(tag) {
		return client.AppStatus{}, "", fail(ExitUsage,
			"%q is not usable as an image tag: use letters, digits, '.', '_' and '-', starting with a letter, digit or underscore, at most %d characters",
			tag, imageref.MaxTagLen)
	}
	return app, tag, nil
}

func (f *Flow) printPlan(o Options, app client.AppStatus, tag, ref string) {
	f.printf("\n  app       %s  (%s)\n", app.Path, app.SourceType)
	f.printf("  image     %s\n", o.Repository)
	if app.Tag != "" && app.Tag != tag {
		f.printf("  tag       %s   (currently %s)\n", tag, app.Tag)
	} else {
		f.printf("  tag       %s\n", tag)
	}
	f.printf("  platform  %s\n", o.Platform)
	f.printf("  via       %s\n\n", o.Delivery)
	if o.DryRun {
		f.printf("  build     docker %s\n", strings.Join(dockercli.BuildArgs(f.buildSpec(o, ref)), " "))
		if o.Delivery == project.DeliveryRegistry {
			f.printf("  push      docker %s\n", strings.Join(dockercli.PushArgs(ref), " "))
		}
		f.printf("  deploy    POST /api/v1/apps/%s/deploy {\"tag\":%q}\n", o.Ref, tag)
	}
}

func (f *Flow) buildSpec(o Options, ref string) dockercli.BuildSpec {
	return dockercli.BuildSpec{
		Ref:        ref,
		Platform:   o.Platform,
		Dockerfile: o.Dockerfile,
		Context:    o.Context,
		BuildArgs:  o.BuildArgs,
		NoCache:    o.NoCache,
	}
}

func (f *Flow) build(ctx context.Context, o Options, ref string) error {
	start := time.Now()
	if err := f.Docker.Run(ctx, dockercli.BuildArgs(f.buildSpec(o, ref))...); err != nil {
		return f.dockerFailure(err, "build failed")
	}

	// A successful build does not guarantee the image is in the local image
	// store: a buildx docker-container builder keeps it unless --load is
	// passed, and the next step then fails with a bare "no such image".
	if _, err := f.Docker.Output(ctx, dockercli.InspectIDArgs(ref)...); err != nil {
		return f.dockerFailure(err, "the build reported success but %s is not in your local image store", ref)
	}
	f.printf("✓ build    %s\n", dur(start))
	return nil
}

// ship gets the image to where Krill can get it.
func (f *Flow) ship(ctx context.Context, o Options, ref string) error {
	if o.SkipPush {
		// A tag that was never pushed fails as a pull error on the server,
		// minutes later and with no obvious link back to here — and by then
		// the app has been retagged to it, which this API cannot undo, so the
		// web UI's Deploy button fails on it too. Refusing costs one rerun;
		// proceeding costs a broken application row.
		if _, err := f.Docker.Output(ctx, dockercli.ManifestInspectArgs(ref)...); err != nil {
			if !o.AllowMissingImage {
				return fail(ExitUsage,
					"--skip-push was given but %s is not in the registry (%v).\n"+
						"Deploying would move the app to a tag Krill cannot pull, and nothing here can put the old one back. Either:\n"+
						"  • drop --skip-push so the image is pushed, or\n"+
						"  • pass --allow-missing-image if your registry cannot answer a manifest query",
					ref, err)
			}
			f.printf("! push     skipped; %s was not found in the registry, continuing anyway\n", ref)
		} else {
			f.printf("· push     skipped (%s already in the registry)\n", ref)
		}
		return nil
	}
	start := time.Now()
	if err := f.Docker.Run(ctx, dockercli.PushArgs(ref)...); err != nil {
		return f.dockerFailure(err, "push failed")
	}
	f.printf("✓ push     %s   %s\n", ref, dur(start))
	return nil
}

func (f *Flow) deployAndWatch(ctx context.Context, o Options, tag string) error {
	acc, err := f.postDeploy(ctx, o, tag)
	if err != nil {
		var ae *client.APIError
		if errors.As(err, &ae) && ae.IsConflict() {
			acc, err = f.resolveConflict(ctx, o, tag)
			if err != nil {
				return err
			}
		} else {
			return f.authAware(err, "could not start the deployment")
		}
	}
	f.printf("✓ deploy   queued  #%d\n", acc.DeploymentID)
	if o.NoWatch {
		return nil
	}
	return f.watch(ctx, o, acc.DeploymentID)
}

// resolveConflict waits out somebody else's deployment and then deploys once.
//
// It never resends in a loop: every accepted deploy rewrites the
// application's tag, and the API has no operation that puts the old one back.
func (f *Flow) resolveConflict(ctx context.Context, o Options, tag string) (client.Accepted, error) {
	if o.NoWaitForLock {
		return client.Accepted{}, fail(ExitBusy,
			"another deployment is already running for this application; nothing was deployed")
	}
	f.printf("· deploy   another deployment is already running — waiting for it\n")

	deps, err := f.API.Deployments(ctx, o.Ref, 1)
	if err != nil || len(deps) == 0 {
		return client.Accepted{}, fail(ExitBusy, "another deployment is running and its id could not be read; try again shortly")
	}
	if err := f.await(ctx, o, deps[0].ID, nil); err != nil {
		// The other deployment's outcome is not this command's result; only
		// its completion matters.
		var fl *Failure
		if errors.As(err, &fl) && fl.Code == ExitTimeout {
			return client.Accepted{}, err
		}
	}
	acc, err := f.postDeploy(ctx, o, tag)
	if err != nil {
		return client.Accepted{}, f.authAware(err, "could not start the deployment after waiting")
	}
	return acc, nil
}

// maxDeployRetries bounds how many times a throttled deploy POST is resent.
const maxDeployRetries = 4

// postDeploy sends the deploy request, resending only when the server refused
// to look at it.
//
// 429 and 503 are the two answers that mean "nothing happened": the rate
// limiter rejects before the handler runs, and 503 is authentication being
// temporarily unavailable. Neither enqueued anything, so resending cannot
// produce a second deployment — which matters here more than usual, because
// every accepted deploy rewrites the application's tag and there is no
// operation that puts the old one back. Every other status is returned as-is:
// a conflict is handled by the caller, and nothing else improves by repeating.
func (f *Flow) postDeploy(ctx context.Context, o Options, tag string) (client.Accepted, error) {
	for attempt := 0; ; attempt++ {
		acc, err := f.API.Deploy(ctx, o.Ref, tag)
		if err == nil {
			return acc, nil
		}
		var ae *client.APIError
		if !errors.As(err, &ae) || !ae.IsRetryable() || attempt >= maxDeployRetries {
			return client.Accepted{}, err
		}
		wait := ae.RetryAfter
		if wait <= 0 {
			wait = time.Duration(attempt+1) * 2 * time.Second
		}
		why := "the server is temporarily unavailable"
		if ae.IsRateLimited() {
			why = "the rate limit for this token is used up"
		}
		f.printf("· deploy   %s; retrying in %s\n", why, wait)
		if serr := f.Sleep(ctx, wait); serr != nil {
			return client.Accepted{}, fail(ExitTimeout, "interrupted before the deployment was started")
		}
	}
}

func (f *Flow) watch(ctx context.Context, o Options, id int64) error {
	if err := f.await(ctx, o, id, f.Out); err != nil {
		return err
	}
	// A deployment can report done while the container starts and dies, so
	// the rollout is confirmed against the service, not the job.
	st, err := f.API.AppStatus(ctx, o.Ref)
	if err != nil {
		// Not success. The deployment finished, but whether the service came
		// up is exactly what this call was for, and an unknown answer must not
		// be reported as a green one — a CI job would mark a dead service
		// deployed. ExitTimeout is the code that already means "unknown".
		return fail(ExitTimeout,
			"deployment #%d finished, but the service state could not be read (%v).\n"+
				"The rollout is unconfirmed — check it with `krill-cli status %s`.",
			id, err, o.Ref)
	}
	running, desired, ok := parseReplicas(st.Replicas)
	switch {
	case ok && desired > 0 && running >= desired:
		f.printf("✓ rollout  %s running\n", st.Replicas)
		f.printDomains(st)
		return nil
	case !ok:
		return fail(ExitTimeout,
			"deployment #%d finished, but the service reports its replicas as %q, which this client cannot read.\n"+
				"The rollout is unconfirmed — check it with `krill-cli status %s`.",
			id, st.Replicas, o.Ref)
	}
	// Two different causes land here and the log is what tells them apart, so
	// the message names both rather than asserting the more dramatic one: a
	// container that started and exited, and one that is still starting after
	// Krill stopped waiting for it (KRILL_CONVERGE_TIMEOUT on the server,
	// which a slow healthcheck start_period routinely outlasts).
	return fail(ExitNotRunning,
		"the deployment finished but the service reports %s replicas.\n"+
			"Either the container started and exited, or it is still starting and Krill stopped waiting.\n"+
			"Run `krill-cli logs %s` to tell which, then `krill-cli status %s` to re-check.",
		st.Replicas, o.Ref, o.Ref)
}

// printDomains lists the app's domains without inventing a scheme for them.
//
// The API sends host names only — it carries neither the per-domain "exposed"
// flag nor whether TLS is on — and a new app's automatic domain is created
// unexposed, with Traefik told not to route it at all. Printing an https URL
// for that produces a link that cannot work, which reads as a broken deploy.
func (f *Flow) printDomains(st client.AppStatus) {
	if len(st.Domains) == 0 {
		return
	}
	f.printf("\n")
	for _, d := range st.Domains {
		f.printf("  domain   %s\n", d)
	}
}

// Follow watches an existing deployment to completion, printing new log
// output as it arrives.
//
// It shares await with the deploy flow rather than reimplementing the loop, so
// rejoining a deployment behaves identically to watching one: the same
// widening poll, the same Retry-After handling, the same bounded wait, and the
// same refusal to call a lost connection a failed deployment.
func (f *Flow) Follow(ctx context.Context, id int64, timeout time.Duration) error {
	if f.Sleep == nil {
		f.Sleep = sleep
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	return f.await(ctx, Options{Timeout: timeout}, id, f.Out)
}

// maxWatchTransportFailures bounds how many consecutive unreachable polls are
// tolerated before the watch gives up. With the widening poll schedule this is
// roughly half a minute of trying.
const maxWatchTransportFailures = 5

// await polls one deployment to a terminal state, streaming new log output to
// logOut when it is non-nil.
func (f *Flow) await(ctx context.Context, o Options, id int64, logOut io.Writer) error {
	deadline := time.Now().Add(o.Timeout)
	start := time.Now()
	anchor := ""
	var retryAfter time.Duration
	transportFailures := 0
	var lastTransportErr error

	for attempt := 0; ; attempt++ {
		// Checked at the TOP so every path through the loop is bounded by the
		// timeout. Checking only after a successful poll means the branches
		// that `continue` — a throttled server, an unreachable one — never
		// reach it, and the watch runs forever on a deploy the user asked to
		// wait two minutes for.
		if time.Now().After(deadline) {
			return f.timedOut(o, id, lastTransportErr)
		}
		delay := PollDelay(attempt, time.Since(start))
		if retryAfter > delay {
			// The server said how long to wait; guessing shorter just earns
			// another 429.
			delay = retryAfter
		}
		retryAfter = 0
		if err := f.Sleep(ctx, delay); err != nil {
			return fail(ExitTimeout, "interrupted while waiting for deployment #%d", id)
		}

		d, err := f.API.Deployment(ctx, id)
		if err != nil {
			var ae *client.APIError
			if errors.As(err, &ae) {
				// A shared budget means somebody else's traffic can push this
				// over; skipping a poll is the correct response, not failing.
				if ae.IsRetryable() {
					retryAfter = ae.RetryAfter
					continue
				}
				return f.authAware(err, "lost track of deployment #%d", id)
			}
			// Not an answer from Krill at all — a dropped link, a proxy
			// hiccup. The deployment is running on the SERVER and is not
			// affected by this machine losing the connection, so reporting a
			// failed deployment here would be a lie; keep polling, and if it
			// never comes back report it as unknown rather than as failed.
			lastTransportErr = err
			transportFailures++
			if transportFailures > maxWatchTransportFailures {
				return f.timedOut(o, id, err)
			}
			continue
		}
		transportFailures, lastTransportErr = 0, nil

		if logOut != nil && d.LogTail != "" {
			out, elided := NewTail(anchor, d.LogTail)
			if elided {
				fmt.Fprintf(logOut, "  … earlier output scrolled out of the server's log window …\n")
			}
			if out != "" {
				fmt.Fprint(logOut, indent(out))
			}
			anchor = LastLine(d.LogTail)
		}

		switch d.Status {
		case "done":
			return nil
		case "error":
			return fail(ExitFailed, "deployment #%d failed", id)
		}
	}
}

// timedOut is the one "we stopped watching" message, which is never a failure
// verdict: the deployment is the server's and may still succeed.
func (f *Flow) timedOut(o Options, id int64, transport error) error {
	if transport != nil {
		return fail(ExitTimeout,
			"lost contact with the server while watching deployment #%d (%v).\n"+
				"The deployment is unaffected and may still succeed — rejoin with `krill-cli deployment %d --watch`",
			id, transport, id)
	}
	return fail(ExitTimeout,
		"deployment #%d is still running after %s — it may still succeed.\nRejoin with `krill-cli deployment %d --watch`",
		id, o.Timeout, id)
}

// authAware maps a transport or API error onto the right exit code, so CI can
// tell a broken credential from a broken build.
func (f *Flow) authAware(err error, format string, args ...any) error {
	var ae *client.APIError
	if errors.As(err, &ae) {
		switch {
		case ae.IsUnauthorized():
			return fail(ExitAuth, "%s: the token was rejected — issue a new one in the Krill UI and run `krill-cli login`", fmt.Sprintf(format, args...))
		case ae.IsForbidden():
			return fail(ExitAuth, "%s: %s", fmt.Sprintf(format, args...), ae.Message)
		}
	}
	return fail(ExitFailed, "%s: %v", fmt.Sprintf(format, args...), err)
}

// dockerFailure attaches the actionable hint for a known docker failure.
func (f *Flow) dockerFailure(err error, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if hint := dockercli.Hint(err.Error()); hint != "" {
		return fail(ExitFailed, "%s: %v\n\n  → %s", msg, err, hint)
	}
	return fail(ExitFailed, "%s: %v", msg, err)
}

func (f *Flow) printf(format string, args ...any) {
	if f.Out != nil {
		fmt.Fprintf(f.Out, format, args...)
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func dur(start time.Time) string { return time.Since(start).Round(100 * time.Millisecond).String() }

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = "  │ " + lines[i]
	}
	return strings.Join(lines, "\n") + "\n"
}

// parseReplicas reads the server's "running/desired" string. It is "?/N" when
// docker has no service, which is not a number and not an error either.
func parseReplicas(s string) (running, desired int, ok bool) {
	before, after, found := strings.Cut(s, "/")
	if !found {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(before, "%d", &running); err != nil {
		return 0, 0, false
	}
	if _, err := fmt.Sscanf(after, "%d", &desired); err != nil {
		return 0, 0, false
	}
	return running, desired, true
}
