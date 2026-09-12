package orgnet

import (
	"context"
	"errors"
	"time"

	"github.com/proshik/krill/internal/docker"
)

// ServiceStater is the one slice of docker.Engine the migration needs: a bulk
// read of service state, so neither helper below costs an API call per service.
type ServiceStater interface {
	ServiceStates(ctx context.Context, names []string) (map[string]docker.ServiceState, error)
}

// ErrWaitTimeout is returned by WaitServicesRunning when the services are still
// not up at the deadline. It is a warning for the caller, not a failure: the
// deploys were submitted either way.
var ErrWaitTimeout = errors.New("timed out waiting for services to come back up")

// RunningAppsFilter keeps only the applications that have a live Swarm service
// meant to be running, for use as Migrator.FilterApps.
//
// An app the operator deliberately stopped (scaled to zero replicas) must not
// come back up because the control plane was upgraded, and an app that never
// deployed has nothing to move — redeploying it would only leave a failed
// deployment row in its history. Both are picked up by their next real deploy,
// which reads the organization network from the store like any other.
func RunningAppsFilter(eng ServiceStater) func(context.Context, []int64) ([]int64, error) {
	return func(ctx context.Context, ids []int64) ([]int64, error) {
		if len(ids) == 0 {
			return nil, nil
		}
		names := make([]string, 0, len(ids))
		for _, id := range ids {
			names = append(names, docker.ServiceName(id))
		}
		states, err := eng.ServiceStates(ctx, names)
		if err != nil {
			return nil, err
		}
		keep := make([]int64, 0, len(ids))
		for _, id := range ids {
			if st := states[docker.ServiceName(id)]; st.Found && st.Desired > 0 {
				keep = append(keep, id)
			}
		}
		return keep, nil
	}
}

// WaitServicesRunning polls until every named service reports at least one
// running task, the context ends, or timeout elapses (ErrWaitTimeout).
//
// This is what makes "databases before apps" real rather than nominal: a
// database redeployment is an asynchronous goroutine, so without waiting, an
// app could converge in the new network before its database arrived there,
// crash-loop on an unresolvable hostname and be rolled back onto the old one.
func WaitServicesRunning(ctx context.Context, eng ServiceStater, names []string, poll, timeout time.Duration) error {
	if len(names) == 0 {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		states, err := eng.ServiceStates(ctx, names)
		if err != nil {
			return err
		}
		allUp := true
		for _, n := range names {
			if st := states[n]; !st.Found || st.Running < 1 {
				allUp = false
				break
			}
		}
		if allUp {
			return nil
		}
		if !time.Now().Before(deadline) {
			return ErrWaitTimeout
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}
