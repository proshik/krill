package dockercli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Runner is the seam between the deploy flow and the docker binary. The whole
// flow takes one of these, so its tests never need docker installed.
type Runner interface {
	// Run executes docker and streams both output streams to the terminal.
	// Used for build and push, where BuildKit's progress rendering IS the
	// user interface and capturing it would throw that away.
	Run(ctx context.Context, args ...string) error

	// Output executes docker and captures stdout, for query commands.
	Output(ctx context.Context, args ...string) (string, error)

	// Stream executes docker and returns its stdout. Closing the returned
	// reader waits for the process and reports a non-zero exit.
	Stream(ctx context.Context, args ...string) (io.ReadCloser, error)
}

// Available reports whether docker is on PATH.
func Available() bool {
	_, err := exec.LookPath("docker")
	return err == nil
}

// Exec is the real Runner.
type Exec struct {
	// Stdout and Stderr receive streamed output; nil means the process's own.
	Stdout, Stderr io.Writer
}

func (e Exec) cmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "docker", args...)
	// BuildKit is what makes --platform and the modern progress output work;
	// it is the default on current engines but not on every one still in use.
	cmd.Env = append(cmd.Environ(), "DOCKER_BUILDKIT=1")
	return cmd
}

func (e Exec) Run(ctx context.Context, args ...string) error {
	cmd := e.cmd(ctx, args...)
	cmd.Stdout = orStd(e.Stdout, os.Stdout)
	cmd.Stderr = orStd(e.Stderr, os.Stderr)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s: %w", args[0], err)
	}
	return nil
}

func (e Exec) Output(ctx context.Context, args ...string) (string, error) {
	cmd := e.cmd(ctx, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// docker's own message is the useful part; the exit status is not.
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("docker %s: %s", args[0], msg)
		}
		return "", fmt.Errorf("docker %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (e Exec) Stream(ctx context.Context, args ...string) (io.ReadCloser, error) {
	cmd := e.cmd(ctx, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("docker %s: %w", args[0], err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("docker %s: %w", args[0], err)
	}
	return &procReader{r: pipe, cmd: cmd, stderr: &stderr, verb: args[0]}, nil
}

// procReader ties the lifetime of a subprocess to the reader draining it, so
// a caller that only handles the reader still learns about a non-zero exit.
type procReader struct {
	r      io.ReadCloser
	cmd    *exec.Cmd
	stderr *strings.Builder
	verb   string
}

func (p *procReader) Read(b []byte) (int, error) { return p.r.Read(b) }

func (p *procReader) Close() error {
	// Drain whatever is left: closing the pipe on a process still writing
	// would hand it EPIPE and turn a clean shutdown into an error.
	_, _ = io.Copy(io.Discard, p.r)
	if err := p.cmd.Wait(); err != nil {
		if msg := strings.TrimSpace(p.stderr.String()); msg != "" {
			return fmt.Errorf("docker %s: %s", p.verb, msg)
		}
		return fmt.Errorf("docker %s: %w", p.verb, err)
	}
	return nil
}

func orStd(w io.Writer, def io.Writer) io.Writer {
	if w == nil {
		return def
	}
	return w
}
