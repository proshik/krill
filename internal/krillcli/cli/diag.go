package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/proshik/krill/internal/krillcli/client"
	"github.com/proshik/krill/internal/krillcli/deployflow"
	"github.com/spf13/cobra"
)

func newWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show who the token acts as and which Krill answered",
		Long: `Show the organization, the token's level, the owner's live role, whether the
token may write, and which Krill installation answered (its public URL and
version).

App ids repeat across installations, so with several contexts check the
instance here before a write.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := connect(false)
			if err != nil {
				return err
			}
			who, err := s.api.Whoami(cmd.Context())
			if err != nil {
				return err
			}
			if g.asJSON {
				return printJSON(who)
			}
			printWhoami(os.Stdout, s, who)
			return nil
		},
	}
}

func printWhoami(w io.Writer, s *session, who client.Whoami) {
	ctxName := s.resolved.Name
	if ctxName == "" {
		ctxName = "(KRILL_TOKEN)"
	}
	fmt.Fprintf(w, "context   %s  (%s)\n", ctxName, displayURL(s.api.Server()))
	switch inst := who.Instance; {
	case inst == nil || inst.Version == "":
		fmt.Fprintf(w, "instance  not reported — this Krill is older than the agent API diagnostics\n")
	case inst.URL == "":
		fmt.Fprintf(w, "instance  %s  (no public URL configured)\n", inst.Version)
	default:
		fmt.Fprintf(w, "instance  %s  %s\n", inst.URL, inst.Version)
	}
	fmt.Fprintf(w, "org       %s  (id %d)\n", who.OrgName, who.OrgID)
	fmt.Fprintf(w, "token     level %s · role %s · can write: %s\n", who.Level, who.Role, yesNo(who.CanWrite))
}

// displayURL is a server URL without the parts that can carry a credential:
// a base URL may hold a proxy's user and password, and the bearer token
// travels separately, so a request with them succeeds and the URL would be
// printed as typed.
func displayURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(unreadable server URL)"
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
}

func newConfigCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "config [APP]",
		Short: "Show the application's saved settings, without secrets",
		Long: `Show the application's saved settings: source, resources and placement,
healthcheck timing, ports, volumes, domains, database/server/bucket links, and
which settings sections are saved but not applied yet (pending).

Never env values, commands, passwords or tokens. The app is a numeric id or a
project/environment/app path.`,
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := connect(false)
			if err != nil {
				return err
			}
			ref, err := s.appRef(args)
			if err != nil {
				return err
			}
			c, err := s.api.AppConfig(cmd.Context(), ref)
			if err != nil {
				return err
			}
			if g.asJSON {
				return printJSON(c)
			}
			printConfig(os.Stdout, c)
			return nil
		},
	}
}

func printConfig(w io.Writer, c client.AppConfig) {
	fmt.Fprintf(w, "%s  (id %d)\n", c.Path, c.ID)
	src := c.Source
	switch src.Type {
	case "image":
		image := src.Image
		if src.Tag != "" {
			image += ":" + src.Tag
		}
		line := "image " + image
		if src.Registry != "" {
			line += "  (registry " + src.Registry + ")"
		}
		fmt.Fprintf(w, "  source       %s\n", line)
	default:
		line := src.Type
		if src.GitURL != "" {
			line += " " + src.GitURL
		}
		if src.Branch != "" {
			line += "  branch " + src.Branch
		}
		if src.Dockerfile != "" {
			line += "  " + src.Dockerfile
		}
		fmt.Fprintf(w, "  source       %s\n", line)
	}
	if len(src.BuildArgs) > 0 {
		fmt.Fprintf(w, "  build args   %s\n", strings.Join(src.BuildArgs, ", "))
	}
	if len(src.BuildSecrets) > 0 {
		fmt.Fprintf(w, "  build secrets %s\n", strings.Join(src.BuildSecrets, ", "))
	}

	r := c.Resources
	fmt.Fprintf(w, "  replicas     %d\n", r.Replicas)
	fmt.Fprintf(w, "  memory       %s\n", limitText(r.MemoryLimit, r.MemoryLimitSource))
	fmt.Fprintf(w, "  cpu          %s\n", limitText(r.CPULimit, r.CPULimitSource))
	restart := r.RestartCondition
	if r.RestartMaxAttempts > 0 {
		restart += fmt.Sprintf(" (at most %d attempts)", r.RestartMaxAttempts)
	}
	fmt.Fprintf(w, "  restart      %s\n", restart)
	placement := r.Placement
	if len(r.PlacementNodes) > 0 {
		placement += " " + strings.Join(r.PlacementNodes, ", ")
	}
	fmt.Fprintf(w, "  placement    %s\n", placement)

	h := c.Healthcheck
	if !h.Configured {
		fmt.Fprintf(w, "  healthcheck  none\n")
	} else {
		var parts []string
		for _, p := range [][2]string{{"interval", h.Interval}, {"timeout", h.Timeout}, {"start period", h.StartPeriod}} {
			if p[1] != "" {
				parts = append(parts, p[0]+" "+p[1])
			}
		}
		if h.Retries > 0 {
			parts = append(parts, fmt.Sprintf("retries %d", h.Retries))
		}
		fmt.Fprintf(w, "  healthcheck  %s\n", orText(strings.Join(parts, " · "), "configured"))
	}

	if c.Port > 0 {
		fmt.Fprintf(w, "  port         %d\n", c.Port)
	}
	for _, p := range c.Ports {
		fmt.Fprintf(w, "  published    %d → %d/%s\n", p.HostPort, p.ContainerPort, p.Protocol)
	}
	for _, v := range c.Volumes {
		fmt.Fprintf(w, "  volume       %s → %s\n", v.Name, v.MountPath)
	}
	for _, d := range c.Domains {
		var flags []string
		if d.Primary {
			flags = append(flags, "primary")
		}
		flags = append(flags, exposedText(d.Exposed), tlsText(d.TLS))
		if len(d.Paths) > 0 {
			flags = append(flags, "paths "+strings.Join(d.Paths, ","))
		}
		if d.RedirectToPrimary {
			flags = append(flags, "redirects to primary")
		}
		if len(d.Protection) > 0 {
			flags = append(flags, "protected by "+strings.Join(d.Protection, ", "))
		}
		fmt.Fprintf(w, "  domain       %s  %s\n", d.Host, strings.Join(flags, " · "))
	}
	for _, l := range c.Links {
		target := l.Kind + " " + l.Target
		if l.Server != "" {
			target += " on " + l.Server
		}
		if l.Field != "" {
			target += " (" + l.Field + ")"
		}
		fmt.Fprintf(w, "  link         %s ← %s\n", l.Var, target)
	}
	switch {
	case !c.Pending.Known:
		fmt.Fprintf(w, "  pending      unknown — nothing to compare with (no recorded deploy, or one in flight)\n")
	case len(c.Pending.Sections) == 0:
		fmt.Fprintf(w, "  pending      none — the running deployment has every saved setting\n")
	default:
		fmt.Fprintf(w, "  pending      %s — saved, not applied yet; a deploy applies them\n", strings.Join(c.Pending.Sections, ", "))
	}
}

func limitText(v, source string) string {
	if v == "" {
		return "no limit"
	}
	if source == "" {
		return v
	}
	return v + " (" + source + ")"
}

func newDomainsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "domains [APP]",
		Short: "Check each domain's DNS and the certificate the gateway serves",
		Long: `Check each of the application's domains from the Krill server: what DNS
resolves it to, whether that is one of the addresses Krill knows for its
gateway, and for an HTTPS domain which certificate the gateway serves for it.

"does not point at the gateway" means DNS answers with none of the addresses
Krill knows. Behind NAT or a private network the public address can be none of
them, so check which address should receive the traffic before calling the
record wrong. "Traefik default certificate" means none was issued for the host
yet. The app is a numeric id or a project/environment/app path.`,
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := connect(false)
			if err != nil {
				return err
			}
			ref, err := s.appRef(args)
			if err != nil {
				return err
			}
			ds, err := s.api.DomainStatus(cmd.Context(), ref)
			if err != nil {
				return err
			}
			if g.asJSON {
				return printJSON(ds)
			}
			printDomains(os.Stdout, ds)
			return nil
		},
	}
}

func printDomains(w io.Writer, ds client.DomainStatus) {
	fmt.Fprintf(w, "%s\n", ds.App)
	if !ds.GatewayKnown {
		fmt.Fprintf(w, "  (Krill knows none of its gateway's addresses, so whether DNS points there is not checked)\n")
	}
	if len(ds.Domains) == 0 {
		fmt.Fprintf(w, "  no domains\n")
		return
	}
	for _, d := range ds.Domains {
		fmt.Fprintf(w, "\n  %s  %s · %s\n", d.Host, exposedText(d.Exposed), tlsText(d.TLS))
		switch {
		case d.DNS.Error != "":
			fmt.Fprintf(w, "    dns    error: %s\n", d.DNS.Error)
		default:
			line := orText(strings.Join(d.DNS.Addresses, ", "), "no addresses")
			if d.PointsAtGateway != nil {
				if *d.PointsAtGateway {
					line += "  → points at the gateway"
				} else {
					line += "  → does NOT point at the gateway"
				}
			}
			fmt.Fprintf(w, "    dns    %s\n", line)
		}
		if c := d.Certificate; c != nil {
			fmt.Fprintf(w, "    cert   %s\n", certText(*c))
		}
	}
}

func certText(c client.CertCheck) string {
	switch {
	case c.Error != "":
		return "handshake failed: " + c.Error
	case c.DefaultCert:
		return "Traefik default certificate — none issued for this host yet"
	}
	line := orText(c.Subject, "certificate")
	if c.Issuer != "" {
		line += " by " + c.Issuer
	}
	if c.NotAfter != "" {
		line += fmt.Sprintf(", until %s (%d days)", shortTime(c.NotAfter), c.DaysLeft)
	}
	if c.Trusted {
		return line + ", trusted"
	}
	if c.VerifyError != "" {
		return line + ", NOT trusted: " + c.VerifyError
	}
	return line + ", NOT trusted"
}

func newBackupsCmd() *cobra.Command {
	var app, env string
	cmd := &cobra.Command{
		Use:   "backups",
		Short: "List the configured backups and their last results",
		Long: `List the organization's configured backups — database (pg_dump), volume and
bucket — with schedule, retention, whether one is running, the last run's time,
status and error, and the last success. --app narrows to one app's volumes and
the databases and buckets it is linked to; --env to one project/environment.

last_error is Krill's summary of what went wrong; the full error is on the
backup's page in the web UI.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if app != "" && env != "" {
				return usageErr(errors.New("filter by --app or by --env, not both"))
			}
			s, err := connect(false)
			if err != nil {
				return err
			}
			list, err := s.api.Backups(cmd.Context(), client.BackupFilter{App: app, Environment: env})
			if err != nil {
				return err
			}
			if g.asJSON {
				return printJSON(list)
			}
			printBackups(os.Stdout, list)
			return nil
		},
	}
	cmd.Flags().StringVar(&app, "app", "", "only this app's backups: numeric id or project/environment/app")
	cmd.Flags().StringVar(&env, "env", "", "only this environment's backups: project/environment")
	cmd.AddCommand(newBackupRunCmd())
	return cmd
}

func printBackups(w io.Writer, list []client.BackupInfo) {
	if len(list) == 0 {
		fmt.Fprintln(w, "No backups configured. Set them up in the Krill web UI.")
		return
	}
	fmt.Fprintf(w, "%-9s %-5s %-36s %-14s %-4s %-20s %s\n", "KIND", "ID", "TARGET", "SCHEDULE", "KEEP", "LAST RUN", "STATUS")
	for _, b := range list {
		status := orText(b.LastStatus, "-")
		switch {
		case b.Running:
			status = "running"
		case !b.Enabled:
			status += " (disabled)"
		}
		fmt.Fprintf(w, "%-9s %-5d %-36s %-14s %-4d %-20s %s\n",
			b.Kind, b.ID, b.Target, b.Schedule, b.Retention, orText(shortTime(b.LastRunAt), "never"), status)
		if b.LastError != "" {
			fmt.Fprintf(w, "          ↳ %s\n", b.LastError)
		}
		if b.LastStatus != "ok" && b.LastSuccessAt != "" {
			fmt.Fprintf(w, "          ↳ last success %s\n", shortTime(b.LastSuccessAt))
		}
	}
}

func newBackupRunCmd() *cobra.Command {
	var wait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "run KIND ID",
		Short: "Start a configured backup now (write)",
		Long: `Start an existing backup now, like "Back up now" in the web UI. KIND is
database, volume or bucket and ID the backup's id, both as ` + "`krill-cli backups`" + `
lists them. Needs a write-level token.

The run applies the backup's retention like a scheduled one, so it can delete
the oldest stored copies.

It returns once the run has started. --wait follows it until it is no longer
running and reports THIS run's result — the list entry whose last run equals
the time this run started: exit 0 ok, 1 failed or recorded nothing, 4 still
running when --timeout ran out or a later run overwrote the result.`,
		Args: usageArgs(cobra.ExactArgs(2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind := args[0]
			id, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil {
				return usageErr(fmt.Errorf("backup id must be a number, got %q", args[1]))
			}
			// Checked before the run starts: a mistyped duration must not
			// leave a backup running (and applying retention) behind an
			// error about a flag.
			if wait && timeout <= 0 {
				return usageErr(fmt.Errorf("--timeout must be positive, got %s", timeout))
			}
			s, err := connect(false)
			if err != nil {
				return err
			}
			started, err := s.api.RunBackup(cmd.Context(), kind, id)
			if err != nil {
				return err
			}
			if !wait {
				if g.asJSON {
					return printJSON(started)
				}
				fmt.Printf("Started %s backup %d at %s.\n", kind, id, shortTime(started.StartedAt))
				fmt.Printf("Follow it with `krill-cli backups`, or start it with --wait next time.\n")
				return nil
			}
			note := os.Stdout
			if g.asJSON {
				note = os.Stderr
			}
			fmt.Fprintf(note, "Started %s backup %d at %s; waiting for it.\n", kind, id, shortTime(started.StartedAt))
			b, err := awaitBackup(cmd.Context(), backupWaiter{
				List: func(ctx context.Context) ([]client.BackupInfo, error) {
					return s.api.Backups(ctx, client.BackupFilter{})
				},
				Sleep: sleepCtx,
				Now:   time.Now,
			}, started, timeout)
			if g.asJSON && b.ID != 0 {
				if perr := printJSON(b); perr != nil && err == nil {
					err = perr
				}
			}
			if err == nil && !g.asJSON {
				fmt.Printf("✓ %s backup %d ok\n", kind, id)
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "wait for this run's result")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "how long --wait waits")
	return cmd
}

// backupWaiter is what awaitBackup needs, injectable for tests.
type backupWaiter struct {
	List  func(ctx context.Context) ([]client.BackupInfo, error)
	Sleep func(ctx context.Context, d time.Duration) error
	Now   func() time.Time
}

// awaitBackup polls the backup list until the started run is over and
// returns its entry with the run's verdict as the error.
//
// The list has no server-side wait, so this uses the deploy watch's poll
// schedule (tight at first, every ten seconds after a minute), which keeps a
// long backup well inside the per-token budget. The result belongs to this
// run only when the entry's last_run_at equals started_at: a later one is
// another run's (this one's outcome is unknown), an earlier or missing one
// means this run recorded nothing, which is treated as a failure.
func awaitBackup(ctx context.Context, w backupWaiter, started client.BackupStarted, timeout time.Duration) (client.BackupInfo, error) {
	begin := w.Now()
	deadline := begin.Add(timeout)
	startedAt, err := time.Parse(time.RFC3339Nano, started.StartedAt)
	if err != nil {
		return client.BackupInfo{}, fmt.Errorf("the server's started_at %q is not a time: %w", started.StartedAt, err)
	}
	// Every request and pause ends at the deadline (plus a moment for an
	// answer in flight), so a stalled list or a long Retry-After cannot carry
	// the wait past --timeout.
	wctx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancel()
	timedOut := func() error {
		if ctx.Err() != nil {
			return &deployflow.Failure{Code: deployflow.ExitTimeout, Err: errors.New("interrupted while waiting for the backup")}
		}
		return &deployflow.Failure{Code: deployflow.ExitTimeout, Err: fmt.Errorf(
			"%s backup %d is still running after %s — it may still succeed; check it with `krill-cli backups`",
			started.Kind, started.ID, timeout)}
	}
	transport := 0
	var last client.BackupInfo
	for attempt := 0; ; attempt++ {
		if !w.Now().Before(deadline) {
			return last, timedOut()
		}
		delay := deployflow.PollDelay(attempt, w.Now().Sub(begin))
		list, err := w.List(wctx)
		if err != nil {
			if wctx.Err() != nil {
				return last, timedOut()
			}
			var ae *client.APIError
			switch {
			case errors.As(err, &ae) && ae.IsRetryable():
				delay = max(delay, ae.RetryAfter)
			case errors.As(err, &ae) && !deployflow.ProxyGaveUp(err):
				return last, err
			default:
				// A dropped link or a proxy that gave up (502/504): the run
				// on the server is unaffected.
				transport++
				if transport > 5 {
					return last, &deployflow.Failure{Code: deployflow.ExitTimeout, Err: fmt.Errorf(
						"lost contact with the server while waiting for the backup (%v); the run is unaffected — check it with `krill-cli backups`", err)}
				}
			}
		} else {
			transport = 0
			found := false
			for _, b := range list {
				if b.Kind == started.Kind && b.ID == started.ID {
					last, found = b, true
					break
				}
			}
			if !found {
				return last, fmt.Errorf("%s backup %d is no longer listed — it was deleted while running", started.Kind, started.ID)
			}
			if !last.Running {
				return last, backupVerdict(last, startedAt)
			}
		}
		if err := w.Sleep(wctx, min(delay, deadline.Sub(w.Now()))); err != nil {
			return last, timedOut()
		}
	}
}

// backupVerdict reads a finished entry against the run that was started.
func backupVerdict(b client.BackupInfo, startedAt time.Time) error {
	ran, err := time.Parse(time.RFC3339Nano, b.LastRunAt)
	switch {
	case err != nil || ran.Before(startedAt):
		return &deployflow.Failure{Code: deployflow.ExitFailed, Err: fmt.Errorf(
			"%s backup %d finished without recording this run (last run %s) — treat it as failed; the backup's page in the web UI has the details",
			b.Kind, b.ID, orText(b.LastRunAt, "never"))}
	case ran.After(startedAt):
		return &deployflow.Failure{Code: deployflow.ExitTimeout, Err: fmt.Errorf(
			"%s backup %d ran again (at %s) before this run's result was read, so this run's outcome is unknown",
			b.Kind, b.ID, b.LastRunAt)}
	case b.LastStatus != "ok":
		msg := fmt.Sprintf("%s backup %d failed", b.Kind, b.ID)
		if b.LastError != "" {
			msg += ": " + b.LastError
		}
		return &deployflow.Failure{Code: deployflow.ExitFailed, Err: errors.New(msg + " (the full error is on the backup's page in the web UI)")}
	}
	return nil
}

// printStatus is the human view of `status`.
func printStatus(w io.Writer, st client.AppStatus, settling bool) {
	fmt.Fprintf(w, "%s  (%s)\n", st.Path, st.SourceType)
	fmt.Fprintf(w, "  status     %s\n", st.Status)
	fmt.Fprintf(w, "  replicas   %s\n", st.Replicas)
	if st.Image != "" {
		image := st.Image
		if st.Tag != "" {
			image += ":" + st.Tag
		}
		fmt.Fprintf(w, "  image      %s\n", image)
	}
	if st.Node != "" {
		fmt.Fprintf(w, "  node       %s\n", st.Node)
	}
	if st.LastDeployID != 0 {
		last := fmt.Sprintf("#%d %s %s", st.LastDeployID, st.LastDeployStat, shortTime(st.LastDeployAt))
		if settling {
			last += "  (applied; Swarm is still converging)"
		}
		fmt.Fprintf(w, "  last       %s\n", last)
	}
	for _, d := range st.Domains {
		fmt.Fprintf(w, "  domain     %s\n", d)
	}
	if st.FailedSinceDeploy != nil {
		line := fmt.Sprintf("%d", *st.FailedSinceDeploy)
		if st.FailedSince != "" {
			line += " since " + shortTime(st.FailedSince)
		}
		if *st.FailedSinceDeploy > 0 {
			line += "  (tasks that crashed, were refused or exited — a lower bound)"
		}
		fmt.Fprintf(w, "  failed     %s\n", line)
	}
	for _, h := range st.Health {
		line := fmt.Sprintf("%s  task %s", h.Status, shortID(h.TaskID))
		if h.Node != "" {
			line += " on " + h.Node
		}
		if h.FailingStreak > 0 {
			line += fmt.Sprintf(", failing streak %d, last exit %d", h.FailingStreak, h.LastExitCode)
		}
		fmt.Fprintf(w, "  health     %s\n", line)
	}
	for _, u := range st.Usage {
		line := "mem " + bytesText(u.MemBytes)
		if u.MemLimitBytes > 0 {
			line += " / " + bytesText(u.MemLimitBytes)
		}
		if u.CPUPct != nil {
			line += fmt.Sprintf(" · cpu %.1f%%", *u.CPUPct)
		}
		line += "  task " + shortID(u.TaskID)
		if u.Node != "" {
			line += " on " + u.Node
		}
		fmt.Fprintf(w, "  usage      %s\n", line)
	}
	if len(st.Tasks) > 0 {
		fmt.Fprintf(w, "  tasks      (newest first)\n")
		for _, t := range st.Tasks {
			line := fmt.Sprintf("%-12s %-10s → %-9s %-16s %s", shortID(t.ID), t.State, t.DesiredState, orText(t.Node, "-"), shortTime(t.UpdatedAt))
			if t.ExitCode != 0 {
				line += fmt.Sprintf("  exit %d", t.ExitCode)
			}
			if t.Error != "" {
				line += "  " + t.Error
			} else if t.Message != "" && t.State != "running" {
				line += "  " + t.Message
			}
			fmt.Fprintf(w, "    %s\n", strings.TrimRight(line, " "))
		}
	}
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func bytesText(n int64) string {
	const mib = 1 << 20
	if n >= 1<<30 {
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	}
	return fmt.Sprintf("%.0f MiB", float64(n)/mib)
}

func exposedText(on bool) string {
	if on {
		return "exposed"
	}
	return "internal"
}

func tlsText(on bool) string {
	if on {
		return "https"
	}
	return "http"
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func orText(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func orUnknown(s string) string { return orText(s, "unknown") }

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
