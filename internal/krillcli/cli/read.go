package cli

import (
	"fmt"
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
		Args:  cobra.NoArgs,
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
		Args:  cobra.MaximumNArgs(1),
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
			fmt.Printf("%s  (%s)\n", st.Path, st.SourceType)
			fmt.Printf("  status     %s\n", st.Status)
			fmt.Printf("  replicas   %s\n", st.Replicas)
			if st.Image != "" {
				image := st.Image
				if st.Tag != "" {
					image += ":" + st.Tag
				}
				fmt.Printf("  image      %s\n", image)
			}
			if st.Node != "" {
				fmt.Printf("  node       %s\n", st.Node)
			}
			if st.LastDeployID != 0 {
				fmt.Printf("  last       #%d %s %s\n", st.LastDeployID, st.LastDeployStat, st.LastDeployAt)
			}
			for _, d := range st.Domains {
				fmt.Printf("  domain     %s\n", d)
			}
			return nil
		},
	}
}

func newLogsCmd() *cobra.Command {
	var tail int
	var level string
	cmd := &cobra.Command{
		Use:   "logs [APP]",
		Short: "Print a tail of the application's runtime log",
		Long: `Print a tail of the application's runtime log.

There is no follow mode. Live streaming exists in Krill only over a WebSocket
that authenticates with a browser session cookie, which an API token cannot
produce — so this prints a snapshot. Re-run it, or watch a deploy with
` + "`krill-cli deployment ID --watch`" + `.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := connect(false)
			if err != nil {
				return err
			}
			ref, err := s.appRef(args)
			if err != nil {
				return err
			}
			lines, err := s.api.Logs(cmd.Context(), ref, tail, level)
			if err != nil {
				return err
			}
			if g.asJSON {
				return printJSON(lines)
			}
			for _, l := range lines {
				fmt.Printf("%-20s %-5s %s\n", shortTime(l.Time), strings.ToUpper(l.Level), l.Message)
			}
			if len(lines) == 0 {
				fmt.Println("(no log lines — a container that never started logs nothing; check `krill-cli status`)")
			}
			return nil
		},
	}
	cmd.Flags().IntVarP(&tail, "tail", "n", 200, "how many lines (max 1000)")
	cmd.Flags().StringVar(&level, "level", "", "minimum level: trace, debug, info, warn, error, fatal")
	return cmd
}

func newDeploymentsCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "deployments [APP]",
		Short: "List recent deployments, newest first",
		Args:  cobra.MaximumNArgs(1),
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
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("deployment id must be a number, got %q", args[0])
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
				fmt.Printf("#%d %s (%s) started %s\n\n%s\n", d.ID, d.Status, d.Trigger, shortTime(d.StartedAt), d.LogTail)
				return nil
			}
			return followDeployment(cmd, s, id)
		},
	}
	cmd.Flags().BoolVar(&watch, "watch", false, "poll until the deployment finishes")
	return cmd
}

// followDeployment reuses the same widening poll and log-diffing the deploy
// flow uses, so rejoining a deploy looks exactly like watching one.
func followDeployment(cmd *cobra.Command, s *session, id int64) error {
	start := time.Now()
	anchor := ""
	for attempt := 0; ; attempt++ {
		time.Sleep(deployflow.PollDelay(attempt, time.Since(start)))
		d, err := s.api.Deployment(cmd.Context(), id)
		if err != nil {
			var ae *client.APIError
			if asAPIError(err, &ae) && ae.IsRetryable() {
				continue
			}
			return err
		}
		if out, elided := deployflow.NewTail(anchor, d.LogTail); out != "" {
			if elided {
				fmt.Println("  … earlier output scrolled out of the server's log window …")
			}
			fmt.Print(out)
		}
		anchor = deployflow.LastLine(d.LogTail)
		switch d.Status {
		case "done":
			fmt.Printf("\n#%d done\n", id)
			return nil
		case "error":
			return fmt.Errorf("deployment #%d failed", id)
		}
	}
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
