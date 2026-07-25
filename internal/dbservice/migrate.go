package dbservice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/oplock"
)

// Migration errors surfaced to the handler for user-facing flashes.
var (
	ErrInstanceBusy = errors.New("instance has a migration in progress")
	ErrSameNode     = errors.New("instance is already on that node")
	ErrNodeNotFound = errors.New("node not found in swarm")
	ErrSourceVolume = errors.New("source volume not found on its node")
)

const (
	defaultMigrateTimeout  = 30 * time.Minute
	migrateQuiesceTimeout  = 60 * time.Second
	migrateConvergeTimeout = 90 * time.Second
	migrateRecoverTimeout  = 2 * time.Minute
)

// MigrateInstanceNode moves a DB instance to another node WITH its data:
// stop → archive the volume on the source node → stream through this process
// → restore on the target node → repoint metadata → redeploy. Async like
// DeployInstance; progress goes to the instance's log feed. deleteSource
// removes the source volume after a successful migration.
func (s *Service) MigrateInstanceNode(id int64, targetHostname string, deleteSource bool) {
	go func() {
		t := s.migrateTimeout
		if t == 0 {
			t = defaultMigrateTimeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), t)
		defer cancel()
		if err := s.migrateInstanceNode(ctx, id, targetHostname, deleteSource); err != nil {
			slog.Error("db instance migration failed", "err", err, "instance_id", id, "target", targetHostname)
			if s.notifier != nil {
				s.notifier.MigrateFailed(ctx, id, err.Error())
			}
		}
	}()
}

// hostOrManager renders a node hostname for logs ("" = the control-plane).
func hostOrManager(hostname string) string {
	if hostname == "" {
		return "control-plane"
	}
	return hostname
}

// resolveNodeID looks up a swarm node ID by hostname ("" = the control-plane
// leader/manager). Shared by the migration orchestrator and
// RemoveInstanceContainers' node-aware volume cleanup.
func resolveNodeID(nodes []docker.SwarmNode, hostname string) (string, bool) {
	if hostname == "" { // control-plane
		for _, n := range nodes {
			if n.Leader {
				return n.ID, true
			}
		}
		return "", false
	}
	for _, n := range nodes {
		if n.Hostname == hostname {
			return n.ID, true
		}
	}
	return "", false
}

// migrateInstanceNode is the synchronous body (split out for tests).
//
// INVARIANT: the volume on the node recorded in node_hostname (or, when the
// service is running, on the node its task actually runs on — metadata may be
// stale from the old metadata-only "set node") is the data's source of truth
// and is NEVER mutated here. Only the TARGET volume is ever pre-cleaned; the
// source is removed strictly after a fully successful migration and only when
// deleteSource is set.
func (s *Service) migrateInstanceNode(ctx context.Context, id int64, target string, deleteSource bool) error {
	inst, err := s.store.GetInstance(ctx, id)
	if err != nil {
		return err
	}
	lock := oplock.DBInstance(inst.AppName)
	if !oplock.TryAcquire(lock) {
		return ErrInstanceBusy
	}
	defer oplock.Release(lock)

	feed := InstanceFeedID(id)
	var out interface{ Write([]byte) (int, error) } = nopWriter{}
	if s.hub != nil {
		s.hub.Open(feed)
		out = s.hub.Writer(feed)
		defer s.hub.Close(feed)
	}

	st, err := s.engine.ServiceState(ctx, inst.AppName)
	if err != nil {
		err = fmt.Errorf("service state: %w", err)
		fmt.Fprintf(out, "❌ %v\n", err)
		return err
	}
	hot := st.Found && st.Desired > 0

	nodes, err := s.engine.Nodes(ctx)
	if err != nil {
		err = fmt.Errorf("list nodes: %w", err)
		fmt.Fprintf(out, "❌ %v\n", err)
		return err
	}
	nodeID := func(hostname string) (string, bool) { return resolveNodeID(nodes, hostname) }
	dstNode, ok := nodeID(target)
	if !ok {
		err := fmt.Errorf("%w: %q", ErrNodeNotFound, target)
		fmt.Fprintf(out, "❌ %v\n", err)
		return err
	}
	// Where the data actually is: a running task's node beats the metadata
	// (the pre-migration "set node" was metadata-only and may have drifted).
	srcHostname := inst.NodeHostname
	if hot {
		if tasks, terr := s.engine.ServiceTasks(ctx, inst.AppName); terr == nil {
			for _, t := range tasks {
				if t.State == "running" {
					if srcHostname != "" && t.NodeName != srcHostname {
						fmt.Fprintf(out, "⚠ metadata says node %q but the task runs on %q — using the task's node\n", srcHostname, t.NodeName)
					}
					srcHostname = t.NodeName
					break
				}
			}
		}
	}
	srcNode, ok := nodeID(srcHostname)
	if !ok {
		err := fmt.Errorf("%w: source %q", ErrNodeNotFound, srcHostname)
		fmt.Fprintf(out, "❌ %v\n", err)
		return err
	}
	if srcNode == dstNode {
		fmt.Fprintf(out, "❌ %v\n", ErrSameNode)
		return ErrSameNode
	}

	vol := volumeName(inst.AppName)
	if exists, verr := s.engine.VolumeExistsOn(ctx, vol, srcNode); verr != nil {
		err := fmt.Errorf("check source volume: %w", verr)
		fmt.Fprintf(out, "❌ %v\n", err)
		return err
	} else if !exists {
		err := fmt.Errorf("%w: %s on %s", ErrSourceVolume, vol, hostOrManager(srcHostname))
		fmt.Fprintf(out, "❌ %v\n", err)
		return err
	}

	if err := s.store.SetInstanceStatus(ctx, id, "migrating"); err != nil {
		err = fmt.Errorf("set status: %w", err)
		fmt.Fprintf(out, "❌ %v\n", err)
		return err
	}
	prevStatus := "idle"
	if hot {
		prevStatus = "running"
	}
	// recover restores service+status on the SOURCE after a failure BEFORE the
	// metadata switch. Detached ctx: recovery must run even on job timeout.
	recover := func(stage string, cause error) error {
		fmt.Fprintf(out, "❌ %s: %v — rolling back to %s\n", stage, cause, hostOrManager(inst.NodeHostname))
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), migrateRecoverTimeout)
		defer rcancel()
		status := prevStatus
		if hot {
			if serr := s.engine.ServiceScale(rctx, inst.AppName, 1); serr != nil {
				slog.Error("migration rollback: scale up failed", "err", serr, "instance_id", id)
				status = "error"
			}
		}
		_ = s.store.SetInstanceStatus(rctx, id, status)
		return fmt.Errorf("%s: %w", stage, cause)
	}

	if hot {
		fmt.Fprintf(out, "→ stopping %s\n", inst.AppName)
		if err := s.engine.ServiceScale(ctx, inst.AppName, 0); err != nil {
			return recover("stop service", err)
		}
		if err := s.waitServiceRunning(ctx, inst.AppName, 0, migrateQuiesceTimeout); err != nil {
			return recover("wait for stop", err)
		}
	}

	// Pre-clean: restore must untar into a FRESH volume (untarring over
	// existing content merges). A leftover target volume is an earlier failed
	// attempt or an old empty deploy — by the invariant, never the source.
	if exists, verr := s.engine.VolumeExistsOn(ctx, vol, dstNode); verr != nil {
		return recover("check target volume", verr)
	} else if exists {
		fmt.Fprintf(out, "→ removing stale target volume on %s\n", hostOrManager(target))
		if err := s.engine.VolumeRemoveOn(ctx, vol, dstNode); err != nil {
			return recover("remove stale target volume", err)
		}
	}

	fmt.Fprintf(out, "→ copying %s: %s → %s\n", vol, hostOrManager(srcHostname), hostOrManager(target))
	pr, pw := io.Pipe()
	archErr := make(chan error, 1)
	go func() {
		aerr := s.engine.VolumeArchive(ctx, vol, pw, srcNode)
		pw.CloseWithError(aerr)
		archErr <- aerr
	}()
	rerr := s.engine.VolumeRestore(ctx, vol, pr, dstNode)
	// If VolumeRestore returned without draining pr (e.g. it failed fast on its
	// own precondition), the archiver's pw.Write can still be blocked waiting
	// for a reader that will never come — CloseWithError unblocks it
	// immediately instead of hanging forever. A harmless no-op when VolumeRestore
	// already drained to EOF (the writer has necessarily finished by then).
	_ = pr.CloseWithError(rerr)
	aerr := <-archErr
	if aerr != nil || rerr != nil {
		return recover("copy volume", errors.Join(aerr, rerr))
	}

	if err := s.store.SetInstanceNode(ctx, id, target); err != nil {
		return recover("update node metadata", err)
	}

	if hot {
		fmt.Fprintf(out, "→ starting on %s\n", hostOrManager(target))
		if err := s.deployCore(ctx, id, out); err != nil {
			return s.revertToSource(ctx, id, inst, out, "deploy on target", err)
		}
		if err := s.waitServiceRunning(ctx, inst.AppName, 1, migrateConvergeTimeout); err != nil {
			return s.revertToSource(ctx, id, inst, out, "converge on target", err)
		}
	} else {
		_ = s.store.SetInstanceStatus(ctx, id, "idle")
	}

	if deleteSource {
		fmt.Fprintf(out, "→ removing source volume on %s\n", hostOrManager(srcHostname))
		if err := s.removeVolumeOn(ctx, vol, srcNode); err != nil {
			fmt.Fprintf(out, "⚠ source volume not removed: %v (remove it manually)\n", err)
		}
	} else {
		fmt.Fprintf(out, "ℹ source volume kept on %s (free rollback; remove it manually when confident)\n", hostOrManager(srcHostname))
	}
	fmt.Fprintf(out, "✅ migrated %s to %s\n", inst.AppName, hostOrManager(target))
	return nil
}

// revertToSource undoes a migration after the metadata switch: repoint the
// instance back at the source node and redeploy there (the source volume is
// untouched — still the source of truth). The half-written target volume is
// left for inspection; the next attempt pre-cleans it.
func (s *Service) revertToSource(ctx context.Context, id int64, inst Instance, out io.Writer, stage string, cause error) error {
	fmt.Fprintf(out, "❌ %s: %v — reverting to %s\n", stage, cause, hostOrManager(inst.NodeHostname))
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), migrateRecoverTimeout)
	defer rcancel()
	if err := s.store.SetInstanceNode(rctx, id, inst.NodeHostname); err != nil {
		slog.Error("migration revert: set node failed", "err", err, "instance_id", id)
		_ = s.store.SetInstanceStatus(rctx, id, "error")
		return fmt.Errorf("%s: %w", stage, cause)
	}
	if err := s.deployCore(rctx, id, out); err != nil {
		slog.Error("migration revert: redeploy on source failed", "err", err, "instance_id", id)
		_ = s.store.SetInstanceStatus(rctx, id, "error")
	}
	return fmt.Errorf("%s: %w", stage, cause)
}

// waitServiceRunning polls until the service's Running count reaches want
// (0 = quiesced, 1 = converged) or the timeout elapses.
func (s *Service) waitServiceRunning(ctx context.Context, name string, want int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		st, err := s.engine.ServiceState(ctx, name)
		if err == nil {
			if want == 0 && (!st.Found || st.Running == 0) {
				return nil
			}
			if want > 0 && st.Running >= want {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("service %s did not reach running=%d in %s", name, want, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// removeVolumeOn is the node-aware sibling of removeVolume (same retry loop:
// right after a stop the volume may briefly be "in use").
func (s *Service) removeVolumeOn(ctx context.Context, name, nodeID string) error {
	var err error
	for i := 0; i < 12; i++ {
		if err = s.engine.VolumeRemoveOn(ctx, name, nodeID); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return err
}
