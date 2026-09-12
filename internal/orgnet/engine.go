package orgnet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// instanceStatusStopped is the db_instances.status of a stopped (or never
// deployed) instance: StopInstance writes it after scaling the service to zero.
const instanceStatusStopped = "idle"

// InstanceRow is the part of a database instance row the instance filter reads.
type InstanceRow struct {
	AppName string // the Swarm service name
	Status  string // db_instances.status
}

// ParkedInstance is a database instance that moves onto its organization
// network without being started.
type ParkedInstance struct {
	ID int64
	// Replicas is the service's current desired count, kept as is: 0 for a
	// stopped instance. The migration never changes whether a service runs.
	Replicas uint64
}

// InstancePlan splits an organization's database instances by how they move.
type InstancePlan struct {
	// Running instances are redeployed and waited on until they are back up.
	Running []int64
	// Parked instances are stopped (or recorded as stopped): their service is
	// rewritten onto the new network at its current replica count, never
	// started, and never waited on.
	Parked []ParkedInstance
}

// RunningInstancesFilter plans how each database instance moves, for use as
// Migrator.PlanInstances. It is the instance counterpart of RunningAppsFilter,
// with one difference that matters: a stopped app is started again by a full
// Deploy, which reads the organization network, but a stopped database instance
// is started by scaling its existing service. A stopped instance left out of the
// migration would therefore come back up on the OLD shared network. So instead
// of being skipped, a stopped instance is parked.
//
//   - No Swarm service: skipped. Nothing was ever deployed (or its first deploy
//     failed before creating one), and the next Deploy creates it in the
//     organization network.
//   - Desired replicas 0, or the row says stopped: parked. Redeploying it would
//     resurrect a database the operator stopped and record it running — and one
//     that cannot start would time out the wait on every boot, re-migrating the
//     whole tenant each time.
//   - Otherwise: running.
func RunningInstancesFilter(eng ServiceStater, lookup func(ctx context.Context, id int64) (InstanceRow, error)) func(context.Context, []int64) (InstancePlan, error) {
	return func(ctx context.Context, ids []int64) (InstancePlan, error) {
		if len(ids) == 0 {
			return InstancePlan{}, nil
		}
		rows := make(map[int64]InstanceRow, len(ids))
		names := make([]string, 0, len(ids))
		for _, id := range ids {
			row, err := lookup(ctx, id)
			if err != nil {
				return InstancePlan{}, fmt.Errorf("load database instance %d: %w", id, err)
			}
			rows[id] = row
			names = append(names, row.AppName)
		}
		states, err := eng.ServiceStates(ctx, names)
		if err != nil {
			return InstancePlan{}, err
		}
		var plan InstancePlan
		for _, id := range ids {
			row := rows[id]
			st := states[row.AppName]
			switch {
			case !st.Found:
			case st.Desired == 0 || row.Status == instanceStatusStopped:
				plan.Parked = append(plan.Parked, ParkedInstance{ID: id, Replicas: uint64(st.Desired)})
			default:
				plan.Running = append(plan.Running, id)
			}
		}
		return plan, nil
	}
}

// WaitServicesRunning polls until every named service reports at least one
// running task, the context ends, or timeout elapses (ErrWaitTimeout).
//
// This is what makes "databases before apps" real rather than nominal: a
// database redeployment is an asynchronous goroutine, so without waiting, an
// app could converge in the new network before its database arrived there,
// crash-loop on an unresolvable hostname and be rolled back onto the old one.
//
// A failed state read is not a verdict on the services: it is logged and polled
// again until the deadline, so one transient Docker error does not cost the
// organization its pass.
func WaitServicesRunning(ctx context.Context, eng ServiceStater, names []string, poll, timeout time.Duration) error {
	if len(names) == 0 {
		return nil
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		states, err := eng.ServiceStates(ctx, names)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = err
			slog.Warn("waiting for services: could not read their state, retrying", "err", err)
		} else {
			lastErr = nil
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
		}
		if !time.Now().Before(deadline) {
			if lastErr != nil {
				return fmt.Errorf("%w (last state read failed: %v)", ErrWaitTimeout, lastErr)
			}
			return ErrWaitTimeout
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}
