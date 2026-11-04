package envclone

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/proshik/krill/internal/appmetrics"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/oplock"
	"github.com/proshik/krill/internal/org"
)

var (
	ErrUnavailable    = errors.New("clone dependency is unavailable")
	ErrCleanupPending = errors.New("clone was not published; resource cleanup is pending")
	ErrFailed         = errors.New("environment clone failed")
)

const cloneTimeout = 3 * time.Minute

type Request struct {
	Key, Name                          string
	UserID, OrgID, ProjectID, SourceID int64
}

type Service struct {
	pool         *pgxpool.Pool
	journal      *Journal
	dbsvc        *dbservice.Service
	engine       docker.Engine
	baseDomain   string
	ctx          context.Context
	cancel       context.CancelFunc
	mu           sync.Mutex
	stopped      bool
	active       int
	work         sync.WaitGroup
	recoveryOnce sync.Once
}

func New(pool *pgxpool.Pool, svc *dbservice.Service, engine docker.Engine, baseDomain string) *Service {
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{pool: pool, journal: NewJournal(pool), dbsvc: svc, engine: engine, baseDomain: baseDomain, ctx: ctx, cancel: cancel}
}

func (s *Service) InFlight() int { s.mu.Lock(); defer s.mu.Unlock(); return s.active }

func (s *Service) enter(ctx context.Context) (context.Context, func(), error) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil, nil, ErrUnavailable
	}
	s.active++
	s.work.Add(1)
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, cloneTimeout)
	stop := context.AfterFunc(s.ctx, cancel)
	return ctx, func() {
		stop()
		cancel()
		s.mu.Lock()
		s.active--
		s.mu.Unlock()
		s.work.Done()
	}, nil
}

func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.stopped = true
	s.cancel()
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.work.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Clone is synchronous. No app, non-PG service, volume or node label is deployed
// here. Placement labels are applied by the ordinary explicit Deploy path.
func (s *Service) Clone(parent context.Context, r Request) (int64, error) {
	ctx, leave, err := s.enter(parent)
	if err != nil {
		return 0, err
	}
	defer leave()
	r.Name = strings.TrimSpace(r.Name)
	slug := org.Slugify(r.Name)
	if slug == "" {
		return 0, ErrNameConflict
	}
	release, err := s.journal.Acquire(ctx, r.Key)
	if err != nil {
		return 0, err
	}
	defer release()
	projectLock := oplock.CloneProject(r.ProjectID)
	if !oplock.TryAcquire(projectLock) {
		return 0, ErrBusy
	}
	defer oplock.Release(projectLock)
	// Live authorization applies even to replays; an old key grants no authority.
	if err := s.checkCaller(ctx, r); err != nil {
		return 0, err
	}
	op, err := s.journal.Reserve(ctx, db.InsertEnvironmentCloneParams{RequestKey: r.Key, UserID: r.UserID, OrganizationID: r.OrgID, ProjectID: r.ProjectID, SourceID: r.SourceID, DestinationName: r.Name, DestinationSlug: slug, Deadline: time.Now().Add(cloneTimeout)})
	if err != nil {
		return 0, err
	}
	if op.State == "succeeded" {
		return s.target(ctx, op)
	}
	if op.State != "preparing" || op.ResourceIntent != "" {
		return 0, ErrCleanupPending
	} // never resume interrupted business work
	snap, err := ReadSnapshot(ctx, s.pool, r.OrgID, r.ProjectID, r.SourceID)
	if err != nil {
		return 0, s.abort(ctx, op, err)
	}
	// Lock all source instances in ID order. Keep them through final publication
	// and compensation, so stop/delete/migration cannot invalidate ownership checks.
	ids := map[int64]string{}
	for _, d := range snap.Databases {
		ids[d.InstanceID] = d.InstanceAppName
	}
	for _, in := range snap.Instances {
		ids[in.SourceID] = in.AppName
	}
	ordered := make([]int64, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	locks := []string{}
	defer func() {
		for i := len(locks) - 1; i >= 0; i-- {
			oplock.Release(locks[i])
		}
	}()
	instances := map[int64]dbservice.Instance{}
	for _, id := range ordered {
		row, e := db.New(s.pool).GetDBInstance(ctx, id)
		if e != nil {
			return 0, s.abort(ctx, op, metadataError(e))
		}
		if row.OrganizationID != r.OrgID || row.AppName != ids[id] {
			return 0, s.abort(ctx, op, ErrNotFound)
		}
		if row.Status == "migrating" {
			return 0, s.abort(ctx, op, ErrUnavailable)
		}
		lock := oplock.DBInstance(row.AppName)
		if !oplock.TryAcquire(lock) {
			return 0, s.abort(ctx, op, ErrBusy)
		}
		locks = append(locks, lock)
		// Only PG provisioning needs the existing server's password; never decrypt
		// or retain a source non-PG password just to copy its configuration.
		if row.Engine == "postgres" {
			in, e := dbservice.NewDBStore(db.New(s.pool)).GetInstance(ctx, id)
			if errors.Is(e, pgx.ErrNoRows) {
				return 0, s.abort(ctx, op, ErrNotFound)
			}
			if e != nil {
				return 0, s.abort(ctx, op, ErrUnavailable)
			}
			if in.AppName != ids[id] {
				return 0, s.abort(ctx, op, ErrNotFound)
			}
			if in.Status == "migrating" || s.dbsvc == nil || s.engine == nil || !s.dbsvc.InstanceRunning(ctx, in) {
				return 0, s.abort(ctx, op, ErrUnavailable)
			}
			if err := s.dbsvc.CheckCloneIsolation(ctx, in, snap.ProtectedDatabases[id], ""); err != nil {
				return 0, s.abort(ctx, op, err)
			}
			instances[id] = in
		}
	}
	if err := s.validateNodes(ctx, snap); err != nil {
		return 0, s.abort(ctx, op, err)
	}
	intent := ResourceIntent{}
	passwords := map[int64]string{}
	for _, d := range snap.Databases {
		suffix := r.Key + strconv.FormatInt(d.SourceID, 36)
		name, e := DatabaseName(d.Name, suffix)
		if e != nil {
			return 0, s.abort(ctx, op, e)
		}
		pw, e := appmetrics.NewToken()
		if e != nil {
			return 0, s.abort(ctx, op, e)
		}
		passwords[d.SourceID] = pw
		intent.Databases = append(intent.Databases, DatabaseIntent{InstanceID: d.InstanceID, InstanceAppName: d.InstanceAppName, Database: dbservice.CloneDatabase{Operation: r.Key, DBName: name, Username: name, Deadline: op.Deadline}})
	}
	if err := s.journal.RecordIntent(ctx, r.Key, intent); err != nil {
		return 0, s.abortLocked(ctx, op, err, instances)
	}
	op, err = s.journal.Outcome(ctx, r.Key)
	if err != nil {
		return 0, ErrCleanupPending
	}
	for i, d := range intent.Databases {
		if err := s.dbsvc.ProvisionCloneDatabase(ctx, instances[d.InstanceID], d.Database, passwords[snap.Databases[i].SourceID]); err != nil {
			return 0, s.abortLocked(ctx, op, ErrFailed, instances)
		}
	}
	for _, d := range intent.Databases {
		if err := s.dbsvc.CheckCloneIsolation(ctx, instances[d.InstanceID], snap.ProtectedDatabases[d.InstanceID], d.Database.Username); err != nil {
			return 0, s.abortLocked(ctx, op, err, instances)
		}
	}
	target, err := s.journal.Publish(ctx, r.Key, func(tx pgx.Tx) (int64, error) { return s.publish(ctx, tx, r, snap, intent, passwords) })
	if errors.Is(err, ErrOutcomeUnknown) {
		check, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		outcome, e := s.journal.Outcome(check, r.Key)
		if e != nil {
			return 0, ErrOutcomeUnknown
		}
		if outcome.State == "succeeded" {
			return s.target(check, outcome)
		}
		if outcome.State != "preparing" && outcome.State != "cleanup" {
			return 0, ErrOutcomeUnknown
		}
	}
	if err != nil {
		return 0, s.abortLocked(ctx, op, err, instances)
	}
	return target, nil
}

func (s *Service) checkCaller(ctx context.Context, r Request) error {
	q := db.New(s.pool)
	p, e := q.GetProject(ctx, r.ProjectID)
	if e != nil {
		return metadataError(e)
	}
	if p.OrganizationID != r.OrgID {
		return ErrNotFound
	}
	env, e := q.GetEnvironment(ctx, r.SourceID)
	if e != nil {
		return metadataError(e)
	}
	if env.ProjectID != r.ProjectID {
		return ErrNotFound
	}
	member, e := q.GetMembership(ctx, db.GetMembershipParams{OrganizationID: r.OrgID, UserID: r.UserID})
	if e != nil {
		return metadataError(e)
	}
	if member.Role != "admin" && member.Role != "owner" {
		return ErrNotFound
	}
	return nil
}

func (s *Service) target(ctx context.Context, op db.EnvironmentClone) (int64, error) {
	if op.TargetID == nil {
		return 0, ErrState
	}
	env, err := db.New(s.pool).GetEnvironment(ctx, *op.TargetID)
	if err != nil {
		return 0, metadataError(err)
	}
	if env.ProjectID != op.ProjectID {
		return 0, ErrNotFound
	}
	return env.ID, nil
}

func (s *Service) validateNodes(ctx context.Context, snap Snapshot) error {
	needed := false
	for _, a := range snap.Apps {
		needed = needed || a.Config.PlacementNodes != ""
	}
	for _, in := range snap.Instances {
		needed = needed || in.NodeHostname != ""
	}
	if !needed {
		return nil
	}
	if s.engine == nil {
		return ErrUnavailable
	}
	nodes, err := s.engine.Nodes(ctx)
	if err != nil {
		return ErrUnavailable
	}
	ids, hosts := map[string]bool{}, map[string]bool{}
	for _, n := range nodes {
		ids[n.ID] = true
		hosts[n.Hostname] = true
	}
	for _, a := range snap.Apps {
		for _, id := range strings.Split(a.Config.PlacementNodes, ",") {
			if id != "" && !ids[id] {
				return ErrUnavailable
			}
		}
	}
	for _, in := range snap.Instances {
		if in.NodeHostname != "" && !hosts[in.NodeHostname] {
			return ErrUnavailable
		}
	}
	return nil
}

func (s *Service) abort(ctx context.Context, op db.EnvironmentClone, cause error) error {
	return s.abortLocked(ctx, op, cause, nil)
}

// held contains PG instances locked by Clone. Recovery supplies nil and takes
// each instance lock itself. Never infer SQL completion from an Exec error.
func (s *Service) abortLocked(ctx context.Context, op db.EnvironmentClone, cause error, held map[int64]dbservice.Instance) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	if err := s.journal.StartCleanup(cleanup, op.RequestKey); err != nil {
		return ErrCleanupPending
	}
	// RecordIntent may have committed despite its reply failing; reload it.
	current, err := s.journal.Outcome(cleanup, op.RequestKey)
	if err != nil {
		return ErrCleanupPending
	}
	if err := s.cleanupResources(cleanup, current, held); err != nil {
		return ErrCleanupPending
	}
	if err := s.journal.CompleteCleanup(cleanup, op.RequestKey); err != nil {
		return ErrCleanupPending
	}
	switch {
	case errors.Is(cause, ErrNotFound), errors.Is(cause, ErrBusy), errors.Is(cause, ErrUnavailable), errors.Is(cause, ErrCredentialHost), errors.Is(cause, dbservice.ErrCloneIsolation):
		return cause
	}
	return ErrFailed
}

func (s *Service) cleanupResources(ctx context.Context, op db.EnvironmentClone, held map[int64]dbservice.Instance) error {
	if op.ResourceIntent == "" {
		return nil
	}
	var intent ResourceIntent
	if err := json.Unmarshal([]byte(op.ResourceIntent), &intent); err != nil {
		return ErrFailed
	}
	if op.State != "cleanup" || intent.CleanedDatabases < 0 || intent.CleanedDatabases > len(intent.Databases) {
		return ErrState
	}
	for _, d := range intent.Databases[intent.CleanedDatabases:] {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.dbsvc == nil {
			return ErrUnavailable
		}
		// A persisted migration survives process-lock loss on restart. Even held
		// instances must not be cleaned while that durable state needs recovery.
		row, err := db.New(s.pool).GetDBInstance(ctx, d.InstanceID)
		if err != nil || row.AppName != d.InstanceAppName || row.OrganizationID != op.OrganizationID || row.Status == "migrating" {
			return ErrUnavailable
		}
		in, ok := held[d.InstanceID]
		release := func() {}
		if !ok {
			key := oplock.DBInstance(row.AppName)
			if !oplock.TryAcquire(key) {
				return ErrBusy
			}
			release = func() { oplock.Release(key) }
			in, err = dbservice.NewDBStore(db.New(s.pool)).GetInstance(ctx, d.InstanceID)
			if err != nil || in.Status == "migrating" {
				release()
				return ErrUnavailable
			}
		}
		err = s.dbsvc.CleanupCloneDatabase(ctx, in, d.Database)
		release()
		if err != nil {
			return err
		}
		// Cancellation may arrive just after SQL cleanup/fencing succeeded.
		// Persist that proof with a short detached write, still under the
		// operation guard. On an ambiguous write, stop and reread next pass;
		// replaying proven cleanup is safe, skipping unproven cleanup is not.
		progress, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		payload, err := s.journal.AdvanceCleanup(progress, op)
		cancel()
		if err != nil {
			return err
		}
		op.ResourceIntent = payload
	}
	return nil
}

func (s *Service) StartRecovery() {
	s.recoveryOnce.Do(func() {
		s.mu.Lock()
		if s.stopped {
			s.mu.Unlock()
			return
		}
		s.work.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.work.Done()
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for {
				s.Recover(s.ctx)
				select {
				case <-s.ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	})
}

// Recover aborts only, never resumes cloning. Pending unreachable work does not
// count as busy between bounded attempts and therefore does not block updates.
func (s *Service) Recover(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	rows, err := db.New(s.pool).ListEnvironmentCloneRecovery(ctx)
	if err != nil {
		return
	}
	for _, op := range rows {
		if ctx.Err() != nil {
			return
		}
		release, err := s.journal.Acquire(ctx, op.RequestKey)
		if err != nil {
			continue
		}
		work, leave, err := s.enter(ctx)
		if err != nil {
			release()
			return
		}
		// Advance ordering before external work. Cancellation cannot leave an
		// unreachable oldest operation at the front of every subsequent pass.
		if err := db.New(s.pool).TouchEnvironmentCloneRecovery(work, op.RequestKey); err != nil {
			slog.Warn("environment clone recovery ordering failed", "operation", op.RequestKey)
			leave()
			release()
			continue
		}
		attempt, stopAttempt := context.WithTimeout(work, 15*time.Second)
		if op.State == "preparing" || op.State == "cleanup" {
			err = s.abort(work, op, ErrFailed)
			// ErrFailed denotes an established clean abort here.
			if errors.Is(err, ErrFailed) {
				err = nil
			}
		} else {
			err = s.prune(attempt, op)
		}
		if err != nil {
			slog.Warn("environment clone cleanup pending", "operation", op.RequestKey)
		}
		stopAttempt()
		leave()
		release()
	}
}

func (s *Service) prune(ctx context.Context, op db.EnvironmentClone) error {
	var intent ResourceIntent
	if op.ResourceIntent != "" {
		if err := json.Unmarshal([]byte(op.ResourceIntent), &intent); err != nil {
			return err
		}
	}
	seen := map[int64]bool{}
	for _, d := range intent.Databases {
		if seen[d.InstanceID] {
			continue
		}
		seen[d.InstanceID] = true
		row, err := db.New(s.pool).GetDBInstance(ctx, d.InstanceID)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		} // server gone; no credentials retained for deleted servers
		if err != nil || row.AppName != d.InstanceAppName || row.Status == "migrating" {
			return ErrUnavailable
		}
		key := oplock.DBInstance(row.AppName)
		if !oplock.TryAcquire(key) {
			return ErrBusy
		}
		in, err := dbservice.NewDBStore(db.New(s.pool)).GetInstance(ctx, d.InstanceID)
		if err == nil && in.Status != "migrating" && s.dbsvc != nil {
			err = s.dbsvc.PruneCloneFences(ctx, in, op.Deadline)
		} else {
			err = ErrUnavailable
		}
		oplock.Release(key)
		if err != nil {
			return err
		}
	}
	_, err := db.New(s.pool).MarkEnvironmentCloneFencesPruned(ctx, op.RequestKey)
	return err
}
