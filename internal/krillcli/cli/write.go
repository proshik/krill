package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop [APP]",
		Short: "Scale the application to zero replicas",
		Long:  "Scale the application to zero replicas. `krill-cli deploy` brings it back; nothing is deleted.",
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
			if err := s.api.Stop(cmd.Context(), ref); err != nil {
				return err
			}
			fmt.Printf("Stopped %s. Deploy it again to bring it back.\n", ref)
			return nil
		},
	}
}

func newReloadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reload [APP]",
		Short: "Restart the running tasks in place",
		Long: `Restart the running tasks in place: same image, no build, no pull.

Reload does NOT pick up environment changes. Variables are baked into the
service definition when a deployment is created, so a variable set with
` + "`krill-cli env set`" + ` takes effect on the next deploy, not on a reload.`,
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
			if err := s.api.Reload(cmd.Context(), ref); err != nil {
				return err
			}
			fmt.Printf("Reloaded %s.\n", ref)
			return nil
		},
	}
}

// newRebuildCmd exists because the deploy flow tells dockerfile apps to run
// it. It is the operation Krill offers for an app whose image it builds
// itself: there is no local image to push, so `deploy` cannot help.
func newRebuildCmd() *cobra.Command {
	var watch bool
	cmd := &cobra.Command{
		Use:   "rebuild [APP]",
		Short: "Rebuild a dockerfile app from its git source, on the server",
		Long: `Rebuild a dockerfile app from source, with no build cache.

This is the server-side build: Krill clones the app's git repository and runs
docker build ON THE SERVER. It is the opposite of ` + "`krill-cli deploy`" + `,
which builds on this machine and ships the result — and it is what a dockerfile
app needs, because such an app has no image of its own to push.

Image apps have no source to rebuild and are refused.`,
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
			acc, err := s.api.Rebuild(cmd.Context(), ref)
			if err != nil {
				return err
			}
			fmt.Printf("Rebuild of %s queued as #%d.\n", ref, acc.DeploymentID)
			if !watch {
				fmt.Printf("Follow it with `krill-cli deployment %d --watch`.\n", acc.DeploymentID)
				return nil
			}
			return followDeployment(cmd, s, acc.DeploymentID)
		},
	}
	cmd.Flags().BoolVar(&watch, "watch", false, "follow the build to completion")
	return cmd
}

func newEnvCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env [APP]",
		Short: "List environment variable names (never their values)",
		Long: `List the application's environment variable names and where each comes from.

Values are never returned by the API, deliberately: anything a tool reads can
end up in a log, a transcript or a model provider's servers.`,
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
			keys, err := s.api.Env(cmd.Context(), ref)
			if err != nil {
				return err
			}
			if g.asJSON {
				return printJSON(keys)
			}
			for _, k := range keys {
				fmt.Printf("%-32s %s\n", k.Key, k.Source)
			}
			return nil
		},
	}

	set := &cobra.Command{
		Use:   "set KEY=VALUE [APP]",
		Short: "Set or add one variable",
		Args:  usageArgs(cobra.RangeArgs(1, 2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, value, ok := cutOne(args[0], '=')
			if !ok {
				return usageErr(fmt.Errorf("expected KEY=VALUE, got %q", args[0]))
			}
			s, err := connect(false)
			if err != nil {
				return err
			}
			ref, err := s.appRef(args[1:])
			if err != nil {
				return err
			}
			if err := s.api.SetEnv(cmd.Context(), ref, key, value, false); err != nil {
				return err
			}
			// Saying this every time is the point: the variable is written
			// but the running container keeps the old value, and a silent
			// no-op here is a long debugging session later.
			fmt.Printf("Set %s on %s.\nThe running container keeps the old value until the next deploy — run `krill-cli deploy`.\n", key, ref)
			return nil
		},
	}

	rm := &cobra.Command{
		Use:   "rm KEY [APP]",
		Short: "Remove one variable",
		Args:  usageArgs(cobra.RangeArgs(1, 2)),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := connect(false)
			if err != nil {
				return err
			}
			ref, err := s.appRef(args[1:])
			if err != nil {
				return err
			}
			if err := s.api.SetEnv(cmd.Context(), ref, args[0], "", true); err != nil {
				return err
			}
			fmt.Printf("Removed %s from %s. It takes effect on the next deploy.\n", args[0], ref)
			return nil
		},
	}

	cmd.AddCommand(set, rm)
	return cmd
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Args:  usageArgs(cobra.NoArgs),
		Run: func(*cobra.Command, []string) {
			fmt.Println(userAgent())
		},
	}
}
