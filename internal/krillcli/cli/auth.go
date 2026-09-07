package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
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
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if server == "" {
				return usageErr(errors.New("--server is required, e.g. --server https://krill.example.com"))
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
			if strings.TrimSpace(ctxName) == "" {
				ctxName = "default"
			}
			// The default name is the ORGANIZATION's, and every Krill install
			// seeds its first org as "Default" — so logging in to a second
			// server would land on the same name and replace the first one's
			// token with no sign that anything was lost, leaving a krill.yaml
			// pinned to that name pointing at the wrong server. Re-logging in
			// to the SAME server is the ordinary case and still just updates.
			if name == "" {
				if existing, ok := store.Contexts[ctxName]; ok && existing.Server != api.Server() {
					return fmt.Errorf(
						"context %q already points at %s, and this login is for %s.\n"+
							"Both organizations are named %q, which is why the default name collides.\n"+
							"Pass --name to say which is which, e.g. --name prod",
						ctxName, existing.Server, api.Server(), who.OrgName)
				}
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
	if stdinIsTerminal() {
		fmt.Print("API token (input hidden): ")
		restore, hidden := hideEcho()
		if !hidden {
			fmt.Println()
			fmt.Fprintln(os.Stderr, "warning: cannot disable terminal echo; the token will be visible as you type")
		} else {
			defer func() {
				restore()
				fmt.Println()
			}()
		}
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", usageErr(errors.New("no token read from standard input"))
	}
	token := strings.TrimSpace(line)
	if token == "" {
		return "", usageErr(errors.New("no token given"))
	}
	return token, nil
}

// hideEcho turns terminal echo off and returns the function that puts it back,
// reporting whether it managed to.
//
// The restore is wired to SIGINT and SIGTERM as well as to the caller's defer,
// because a defer does not run when the process is signalled — and Ctrl-C at a
// hidden prompt is the single most likely way this read ends, when somebody
// realises they have to go and copy the token. Without the handler that leaves
// the user's shell with echo off and no visible keystrokes until they think to
// type `stty sane` blind.
func hideEcho() (restore func(), ok bool) {
	if err := sttyEcho(false); err != nil {
		return func() {}, false
	}
	done := make(chan struct{})
	var once sync.Once
	restore = func() {
		once.Do(func() {
			_ = sttyEcho(true)
			close(done)
		})
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		defer signal.Stop(sig)
		select {
		case <-sig:
			restore()
			fmt.Fprintln(os.Stderr)
			// 128+SIGINT, the conventional status for "killed by Ctrl-C".
			os.Exit(130)
		case <-done:
		}
	}()
	return restore, true
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

// stdinIsTerminal reports whether stdin is an interactive terminal.
//
// It asks stty rather than looking at os.ModeCharDevice, which is true for
// /dev/null — the very thing cron, nohup and CI redirect stdin from. Believing
// the mode bit means printing a prompt nobody will answer and then failing on
// an immediate EOF, with a warning about terminal echo on top.
func stdinIsTerminal() bool {
	cmd := exec.CommandContext(context.Background(), "stty")
	cmd.Stdin = os.Stdin
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}

func newContextCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "context",
		Short: "List the Krill servers you are logged in to",
		Args:  usageArgs(cobra.NoArgs),
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
		Args:  usageArgs(cobra.ExactArgs(1)),
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
		Args:    usageArgs(cobra.ExactArgs(1)),
		RunE: func(_ *cobra.Command, args []string) error {
			store, err := cliconfig.Load()
			if err != nil {
				return err
			}
			wasCurrent := store.Current == args[0]
			if err := store.Remove(args[0]); err != nil {
				return err
			}
			if err := cliconfig.Save(store); err != nil {
				return err
			}
			fmt.Printf("Removed context %q. The token still exists on the server — revoke it there if it is no longer wanted.\n", args[0])
			// Removing the current context silently repoints it at another
			// server, and the next unpinned deploy would go there. Say so.
			if wasCurrent && store.Current != "" {
				fmt.Printf("That was the current context; %q is now current (%s).\n", store.Current, store.Contexts[store.Current].Server)
			}
			return nil
		},
	}

	cmd.AddCommand(use, rm)
	return cmd
}
