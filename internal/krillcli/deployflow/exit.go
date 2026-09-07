package deployflow

import (
	"errors"
	"fmt"
)

// Exit codes. CI needs to tell these apart: "your build is broken" and "the
// server was busy" call for different reactions, and a timeout is not a
// success.
const (
	ExitOK = 0
	// ExitFailed — the deployment itself failed on the server.
	ExitFailed = 1
	// ExitUsage — configuration or arguments are wrong; nothing was attempted.
	ExitUsage = 2
	// ExitBusy — another deployment is in flight and --no-wait-for-lock was set.
	ExitBusy = 3
	// ExitTimeout — still running when the watch gave up. The deploy may yet
	// succeed; this is "unknown", not "failed".
	ExitTimeout = 4
	// ExitNotRunning — the deployment reported done but the service is not
	// running its replicas. The container started and died.
	ExitNotRunning = 5
	// ExitAuth — the token was rejected, or lacks the level for this.
	ExitAuth = 6
)

// Failure is an error carrying the exit code the process should use.
type Failure struct {
	Code int
	Err  error
}

func (f *Failure) Error() string { return f.Err.Error() }
func (f *Failure) Unwrap() error { return f.Err }

func fail(code int, format string, args ...any) *Failure {
	return &Failure{Code: code, Err: fmt.Errorf(format, args...)}
}

// CodeOf reports the exit code an error should produce. Anything that is not
// a Failure is an unexpected error and exits 1.
func CodeOf(err error) int {
	if err == nil {
		return ExitOK
	}
	var f *Failure
	if errors.As(err, &f) {
		return f.Code
	}
	return ExitFailed
}
