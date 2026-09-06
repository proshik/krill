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

// Docker is the part of the docker CLI this flow uses.
type Docker interface {
	Run(ctx context.Context, args ...string) error
	Output(ctx context.Context, args ...string) (string, error)
	Stream(ctx context.Context, args ...string) (io.ReadCloser, error)
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
				"Use `krill-cli rebuild` for that, or change the app to an image source in the Krill UI.",
			app.Path, app.SourceType)
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
	if o.Delivery == project.DeliveryUpload {
		return fail(ExitUsage,
			"delivery: upload is not available in this build of krill-cli — use delivery: registry")
	}
	if o.SkipPush {
		// A tag that was never pushed fails as a pull error on the server,
		// minutes later and with no obvious link back to here.
		if _, err := f.Docker.Output(ctx, dockercli.ManifestInspectArgs(ref)...); err != nil {
			f.printf("! push skipped, and %s was not found in the registry — the deploy will fail to pull it\n", ref)
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
	acc, err := f.API.Deploy(ctx, o.Ref, tag)
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
	acc, err := f.API.Deploy(ctx, o.Ref, tag)
	if err != nil {
		return client.Accepted{}, f.authAware(err, "could not start the deployment after waiting")
	}
	return acc, nil
}

func (f *Flow) watch(ctx context.Context, o Options, id int64) error {
	if err := f.await(ctx, o, id, f.Out); err != nil {
		return err
	}
	// A deployment can report done while the container starts and dies, so
	// the rollout is confirmed against the service, not the job.
	st, err := f.API.AppStatus(ctx, o.Ref)
	if err != nil {
		f.printf("✓ rollout  deployment finished; could not read the service state: %v\n", err)
		return nil
	}
	running, desired, ok := parseReplicas(st.Replicas)
	if ok && running >= desired && desired > 0 {
		f.printf("✓ rollout  %s running\n", st.Replicas)
		for _, d := range st.Domains {
			f.printf("\nhttps://%s\n", d)
		}
		return nil
	}
	return fail(ExitNotRunning,
		"the deployment finished but the service reports %s replicas — the container started and exited.\nRun `krill-cli logs %s` to see why.",
		st.Replicas, o.Ref)
}

// await polls one deployment to a terminal state, streaming new log output to
// logOut when it is non-nil.
func (f *Flow) await(ctx context.Context, o Options, id int64, logOut io.Writer) error {
	deadline := time.Now().Add(o.Timeout)
	start := time.Now()
	anchor := ""

	for attempt := 0; ; attempt++ {
		if err := f.Sleep(ctx, PollDelay(attempt, time.Since(start))); err != nil {
			return fail(ExitTimeout, "interrupted while waiting for deployment #%d", id)
		}
		d, err := f.API.Deployment(ctx, id)
		if err != nil {
			var ae *client.APIError
			// A shared budget means somebody else's traffic can push this
			// over; skipping a poll is the correct response, not failing.
			if errors.As(err, &ae) && ae.IsRetryable() {
				continue
			}
			return f.authAware(err, "lost track of deployment #%d", id)
		}

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
		if time.Now().After(deadline) {
			return fail(ExitTimeout,
				"deployment #%d is still running after %s — it may still succeed.\nRejoin with `krill-cli deployment %d --watch`",
				id, o.Timeout, id)
		}
	}
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
