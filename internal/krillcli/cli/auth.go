package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/proshik/krill/internal/krillcli/cliconfig"
	"github.com/proshik/krill/internal/krillcli/client"
	"github.com/spf13/cobra"
)

func newLoginCmd() *cobra.Command {
	var server, name string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Store an API token for a Krill server",
		Long: `Store an API token for a Krill server.

Create the token in the Krill web UI first: your organization's Settings ->
API tokens. A token that will deploy needs the "write" level. The plaintext
is shown once and is not recoverable.

The token is read from standard input, never from a flag — a flag would put
it in your shell history and in the process list. It is stored in
~/.config/krill/config.json with owner-only permissions.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if server == "" {
				return fmt.Errorf("--server is required, e.g. --server https://krill.example.com")
			}
			token, err := readToken()
			if err != nil {
				return err
			}
			api, err := client.New(server, token, userAgent())
			if err != nil {
				return err
			}
			// Verify before storing: a token saved without a check turns
			// every later command into the place the mistake surfaces.
			who, err := api.Whoami(cmd.Context())
			if err != nil {
				return fmt.Errorf("the server rejected this token: %w", err)
			}

			store, err := cliconfig.Load()
			if err != nil {
				return err
			}
			ctxName := name
			if ctxName == "" {
				ctxName = who.OrgName
			}
			store.Set(ctxName, cliconfig.Context{
				Server: api.Server(), Token: token,
				UserID: who.UserID, OrgID: who.OrgID, OrgName: who.OrgName,
				Level: who.Level, CheckedAt: time.Now().UTC().Format(time.RFC3339),
			})
			if err := cliconfig.Save(store); err != nil {
				return err
			}
			fmt.Printf("Logged in to %s as org %q (level %s, role %s), saved as context %q.\n",
				api.Server(), who.OrgName, who.Level, who.Role, ctxName)
			if !who.CanWrite {
				fmt.Println("Note: this token cannot deploy. It is read-level, or its owner is not an admin of the org.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&server, "server", "", "Krill base URL, e.g. https://krill.example.com")
	cmd.Flags().StringVar(&name, "name", "", "context name (default: the organization name)")
	return cmd
}

// readToken reads the token from stdin, without echo when stdin is a
// terminal.
//
// Echo is suppressed by shelling out to stty rather than by importing
// golang.org/x/term: this repository already shells out to git and docker,
// and the alternative is a new module for one line. If stty is missing the
// read still works, with a warning — better than refusing to log in.
func readToken() (string, error) {
	if env := strings.TrimSpace(os.Getenv(cliconfig.EnvToken)); env != "" {
		return env, nil
	}
	interactive := isTerminal(os.Stdin)
	if interactive {
		fmt.Print("API token (input hidden): ")
		if err := sttyEcho(false); err != nil {
			fmt.Println()
			fmt.Fprintln(os.Stderr, "warning: cannot disable terminal echo; the token will be visible as you type")
		} else {
			defer func() {
				_ = sttyEcho(true)
				fmt.Println()
			}()
		}
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("no token read from standard input")
	}
	token := strings.TrimSpace(line)
	if token == "" {
		return "", fmt.Errorf("no token given")
	}
	return token, nil
}

func sttyEcho(on bool) error {
	arg := "-echo"
	if on {
		arg = "echo"
	}
	cmd := exec.CommandContext(context.Background(), "stty", arg)
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func newContextCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "context",
		Short: "List the Krill servers you are logged in to",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			store, err := cliconfig.Load()
			if err != nil {
				return err
			}
			names := store.Names()
			if len(names) == 0 {
				fmt.Println("No contexts. Run `krill-cli login --server https://krill.example.com`.")
				return nil
			}
			for _, n := range names {
				c := store.Contexts[n]
				marker := "  "
				if n == store.Current {
					marker = "* "
				}
				fmt.Printf("%s%-16s %-40s org %s (%s)\n", marker, n, c.Server, c.OrgName, c.Level)
			}
			return nil
		},
	}

	use := &cobra.Command{
		Use:   "use NAME",
		Short: "Make a context the default",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			store, err := cliconfig.Load()
			if err != nil {
				return err
			}
			if err := store.Use(args[0]); err != nil {
				return err
			}
			if err := cliconfig.Save(store); err != nil {
				return err
			}
			fmt.Printf("Now using context %q.\n", args[0])
			return nil
		},
	}

	rm := &cobra.Command{
		Use:     "rm NAME",
		Aliases: []string{"logout"},
		Short:   "Forget a context and its token",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			store, err := cliconfig.Load()
			if err != nil {
				return err
			}
			if err := store.Remove(args[0]); err != nil {
				return err
			}
			if err := cliconfig.Save(store); err != nil {
				return err
			}
			fmt.Printf("Removed context %q. The token still exists on the server — revoke it there if it is no longer wanted.\n", args[0])
			return nil
		},
	}

	cmd.AddCommand(use, rm)
	return cmd
}
