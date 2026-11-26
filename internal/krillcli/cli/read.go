package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/proshik/krill/internal/krillcli/client"
	"github.com/proshik/krill/internal/krillcli/deployflow"
	"github.com/spf13/cobra"
)

// reservedVerbs are the path segments the API reads as an operation rather
// than as part of an application reference. An app whose name is one of these
// can only be addressed by its numeric id, and the listing says so at the
// moment it would matter.
var reservedVerbs = map[string]bool{
	"logs": true, "env": true, "deployments": true,
	"deploy": true, "rebuild": true, "reload": true, "stop": true,
}

func newAppsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "apps",
		Short: "List the applications in your organization",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := connect(false)
			if err != nil {
				return err
			}
			apps, err := s.api.ListApps(cmd.Context())
			if err != nil {
				return err
			}
			if g.asJSON {
				return printJSON(apps)
			}
			if len(apps) == 0 {
				fmt.Println("No applications. Create one in the Krill web UI.")
				return nil
			}
			fmt.Printf("%-6s %-38s %-11s %-9s %s\n", "ID", "PATH", "SOURCE", "STATUS", "IMAGE")
			for _, a := range apps {
				image := a.Image
				if a.Tag != "" {
					image += ":" + a.Tag
				}
				fmt.Printf("%-6d %-38s %-11s %-9s %s\n", a.ID, a.Path, a.SourceType, a.Status, image)
				if name := lastSegment(a.Path); reservedVerbs[name] {
					fmt.Printf("       ↳ named after an API verb — address this one by its id (%d)\n", a.ID)
				}
			}
			return nil
		},
	}
}

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status [APP]",
		Short: "Show one application's live status",
		Long: `Show one application's live status: replicas, node, image, domains and the
last deployment, and — when Swarm answers — its newest tasks (what
` + "`docker service ps`" + ` shows), how many tasks ended since the last deploy,
container health and memory/CPU. A part the server could not read is left out.

With --json the server's answer is printed as it came. While the last
deployment is still running, the human view also asks for that deployment once
to say whether it is applied and Swarm is still converging.`,
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
			st, err := s.api.AppStatus(cmd.Context(), ref)
			if err != nil {
				return err
			}
			if g.asJSON {
				return printJSON(st)
			}
			settling := false
			if st.LastDeployID != 0 && st.LastDeployStat == "running" {
				d, derr := s.api.Deployment(cmd.Context(), st.LastDeployID)
				if derr != nil {
					fmt.Fprintf(os.Stderr, "warning: could not read deployment #%d: %v\n", st.LastDeployID, derr)
				} else {
					settling = d.Settling
				}
			}
			printStatus(os.Stdout, st, settling)
			return nil
		},
	}
}

func newLogsCmd() *cobra.Command {
	var (
		tail     int
		level    string
		since    string
		until    string
		contains string
		waitFor  string
		timeout  time.Duration
	)
	cmd := &cobra.Command{
		Use:   "logs [APP]",
		Short: "Print the application's runtime log, or wait for a line",
		Long: `Print the application's runtime log: the most recent lines across all of its
tasks, merged by time, oldest first.

  --since/--until   a window: an RFC3339 time or a duration ago (10m, 2h);
                    --until needs --since
  --contains TEXT   only lines whose message contains TEXT, ignoring case
  --level LEVEL     drop lines whose detected level is below LEVEL; a line
                    with no detected level is always kept

--wait-for TEXT waits instead, on the server, for a NEW line containing TEXT
(written after the call; earlier lines are not searched — read them with
--since and --contains), for at most --timeout (default 60s, at most 120s).
It cannot be combined with --since, --until, --contains or --level; with it,
--tail is how many lines before the match come back (default 20, max 200).
It exits 0 when the line was seen and 4 when it was not: a timeout, the log
ending, or a broken live read — none of which proves the line never came.

There is no follow mode. Live streaming exists in Krill only over a WebSocket
that authenticates with a browser session cookie, which an API token cannot
produce. Watch a deploy with ` + "`krill-cli deployment ID --watch`" + `.`,
		Args: usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			waiting := waitFor != ""
			if waiting && (since != "" || until != "" || contains != "" || level != "") {
				return usageErr(errors.New("--wait-for waits for new lines only and cannot be combined with --since, --until, --contains or --level; " +
					"search earlier lines with --since and --contains first"))
			}
			if !waiting && cmd.Flags().Changed("timeout") {
				return usageErr(errors.New("--timeout applies to --wait-for only"))
			}
			if waiting && (timeout <= 0 || timeout > maxLogWait) {
				return usageErr(fmt.Errorf("--timeout must be between 1s and %s (the server's limit), got %s", maxLogWait, timeout))
			}
			s, err := connect(false)
			if err != nil {
				return err
			}
			ref, err := s.appRef(args)
			if err != nil {
				return err
			}
			if waiting {
				n := 0
				if cmd.Flags().Changed("tail") {
					n = tail
				}
				return waitLog(cmd, s, ref, waitFor, n, timeout)
			}
			lines, err := s.api.Logs(cmd.Context(), ref, client.LogsQuery{
				Tail: tail, Level: level, Since: since, Until: until, Contains: contains,
			})
			if err != nil {
				return err
			}
			if g.asJSON {
				return printJSON(lines)
			}
			printLogLines(os.Stdout, lines)
			if len(lines) == 0 {
				if since != "" || contains != "" || level != "" {
					fmt.Println("(no matching log lines)")
				} else {
					fmt.Println("(no log lines — a container that never started logs nothing; check `krill-cli status`)")
				}
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&tail, "tail", "n", 200, "how many lines (max 1000); with --wait-for, lines before the match (default 20, max 200)")
	cmd.Flags().StringVar(&level, "level", "", "minimum level: trace, debug, info, warn, error, fatal")
	cmd.Flags().StringVar(&since, "since", "", "start of the window: RFC3339 time or a duration ago (10m, 2h)")
	cmd.Flags().StringVar(&until, "until", "", "end of the window, same forms as --since; needs --since")
	cmd.Flags().StringVar(&contains, "contains", "", "only lines containing this text, ignoring case")
	cmd.Flags().StringVar(&waitFor, "wait-for", "", "wait for a new line containing this text, ignoring case")
	cmd.Flags().DurationVar(&timeout, "timeout", 60*time.Second, "how long --wait-for waits (max 2m)")
	return cmd
}

// maxLogWait is the server's own limit on a log wait.
const maxLogWait = 120 * time.Second

// waitLog runs `logs --wait-for` and turns its outcome into an exit code:
// seen is 0, anything else is ExitTimeout — "stopped waiting, the answer is
// unknown", which is what a missed line after a timeout or a broken read is.
func waitLog(cmd *cobra.Command, s *session, ref, text string, tail int, timeout time.Duration) error {
	res, err := s.api.WaitLog(cmd.Context(), ref, text, tail, timeout)
	if err != nil {
		return err
	}
	if g.asJSON {
		if err := printJSON(res); err != nil {
			return err
		}
	} else {
		printLogLines(os.Stdout, res.Lines)
	}
	if res.Found {
		if !g.asJSON {
			fmt.Fprintf(os.Stderr, "found %q (following since %s)\n", text, orUnknown(shortTime(res.FollowingSince)))
		}
		return nil
	}
	why := fmt.Sprintf("no new line containing %q (stopped by %s", text, res.StoppedBy)
	if res.FollowingSince != "" {
		why += ", following since " + shortTime(res.FollowingSince)
	} else {
		why += ", the live read never opened"
	}
	why += ")"
	if res.Incomplete {
		why += "; the live read broke, so this is no proof the line never came"
	}
	return &deployflow.Failure{Code: deployflow.ExitTimeout, Err: fmt.Errorf("%s.\n"+
		"Lines written before the wait began are not searched — check them with --since and --contains", why)}
}

func printLogLines(w io.Writer, lines []client.LogLine) {
	for _, l := range lines {
		fmt.Fprintf(w, "%-20s %-5s %s\n", shortTime(l.Time), strings.ToUpper(l.Level), l.Message)
	}
}

func newDeploymentsCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "deployments [APP]",
		Short: "List recent deployments, newest first",
		Args:  usageArgs(cobra.MaximumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := connect(false)
			if err != nil {
				return err
			}
			ref, err := s.appRef(args)
			if err != nil {
				return err
			}
			deps, err := s.api.Deployments(cmd.Context(), ref, limit)
			if err != nil {
				return err
			}
			if g.asJSON {
				return printJSON(deps)
			}
			fmt.Printf("%-7s %-9s %-9s %-21s %s\n", "ID", "STATUS", "TRIGGER", "STARTED", "IMAGE")
			for _, d := range deps {
				fmt.Printf("#%-6d %-9s %-9s %-21s %s\n", d.ID, d.Status, d.Trigger, shortTime(d.StartedAt), d.ImageTag)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "how many to list (max 50)")
	return cmd
}

func newDeploymentCmd() *cobra.Command {
	var watch bool
	cmd := &cobra.Command{
		Use:   "deployment ID",
		Short: "Show one deployment, optionally following it to completion",
		Long: `Show one deployment and the tail of its build log.

--watch waits for it to finish, on the server: each request is held while the
deployment runs (up to two minutes) and answered the moment it ends, so a long
build costs a few requests rather than one every few seconds. With --json,
--watch prints no log while waiting and prints the final deployment as JSON;
the exit code says how it ended (0 done, 1 failed, 4 still running when
--timeout ran out — "settling" means applied, Swarm still converging).`,
		Args: usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return usageErr(fmt.Errorf("deployment id must be a number, got %q", args[0]))
			}
			s, err := connect(false)
			if err != nil {
				return err
			}
			if !watch {
				d, err := s.api.Deployment(cmd.Context(), id)
				if err != nil {
					return err
				}
				if g.asJSON {
					return printJSON(d)
				}
				status := d.Status
				if d.Settling {
					status += " (applied; Swarm is still converging)"
				}
				fmt.Printf("#%d %s (%s) started %s\n\n%s\n", d.ID, status, d.Trigger, shortTime(d.StartedAt), d.LogTail)
				return nil
			}
			return followDeployment(cmd, s, id)
		},
	}
	cmd.Flags().BoolVar(&watch, "watch", false, "poll until the deployment finishes")
	cmd.Flags().DurationVar(&watchTimeout, "timeout", 10*time.Minute, "how long to keep watching")
	return cmd
}

// watchTimeout bounds --watch. Ten minutes matches the server's own job
// timeout, so the client gives up at the point the deployment itself would.
var watchTimeout = 10 * time.Minute

// followDeployment rejoins a running deployment.
//
// It calls the deploy flow's own watcher rather than repeating its loop: the
// copy that used to live here had no deadline at all (so a throttled or
// unreachable server looped forever) and slept with time.Sleep, which ignores
// Ctrl-C. One implementation, one set of properties.
//
// With --json nothing but the final deployment goes to stdout: the log is not
// streamed (which also lets each request wait the server's full two minutes)
// and notes go to stderr.
func followDeployment(cmd *cobra.Command, s *session, id int64) error {
	if g.asJSON {
		flow := &deployflow.Flow{API: s.api, Out: os.Stderr}
		d, err := flow.Follow(cmd.Context(), id, watchTimeout, false)
		if d.ID != 0 {
			if perr := printJSON(d); perr != nil && err == nil {
				err = perr
			}
		}
		return err
	}
	flow := &deployflow.Flow{API: s.api, Out: os.Stdout}
	if _, err := flow.Follow(cmd.Context(), id, watchTimeout, true); err != nil {
		return err
	}
	fmt.Printf("\n#%d done\n", id)
	return nil
}

func lastSegment(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// shortTime renders an RFC3339 timestamp for a terminal, leaving anything it
// cannot parse untouched.
func shortTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("2006-01-02 15:04:05")
}
