package cli

import (
	"errors"
	"fmt"

	"github.com/proshik/krill/internal/krillcli/client"
	"github.com/spf13/cobra"
)

func asAPIError(err error, target **client.APIError) bool { return errors.As(err, target) }

func newStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop [APP]",
		Short: "Scale the application to zero replicas",
		Long:  "Scale the application to zero replicas. `krill-cli deploy` brings it back; nothing is deleted.",
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
			if err := s.api.Reload(cmd.Context(), ref); err != nil {
				return err
			}
			fmt.Printf("Reloaded %s.\n", ref)
			return nil
		},
	}
}

func newEnvCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env [APP]",
		Short: "List environment variable names (never their values)",
		Long: `List the application's environment variable names and where each comes from.

Values are never returned by the API, deliberately: anything a tool reads can
end up in a log, a transcript or a model provider's servers.`,
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
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, value, ok := cutOne(args[0], '=')
			if !ok {
				return fmt.Errorf("expected KEY=VALUE, got %q", args[0])
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
		Args:  cobra.RangeArgs(1, 2),
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
		Args:  cobra.NoArgs,
		Run: func(*cobra.Command, []string) {
			fmt.Println(userAgent())
		},
	}
}
