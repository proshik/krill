package envclone

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync"

	"github.com/jackc/pgx/v5"
	db "github.com/proshik/krill/internal/database/gen"
	"github.com/proshik/krill/internal/dbservice"
	"github.com/proshik/krill/internal/oplock"
)

var (
	ErrRequestConflict = errors.New("clone request belongs to another source or destination")
	ErrNameConflict    = errors.New("clone destination name is already reserved")
	ErrBusy            = errors.New("clone operation is already running")
	ErrState           = errors.New("clone operation cannot transition from its current state")
	ErrOutcomeUnknown  = errors.New("clone commit outcome must be checked before cleanup")
)

var requestKeyPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func NewRequestKey() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ResourceIntent contains new identities, source-instance locators and cleanup progress.
// Keeping this typed (rather than storing a snapshot/map) prevents accidentally
// journaling source env/build secrets or an instance's superuser password.
type ResourceIntent struct {
	Databases []DatabaseIntent `json:"databases"`
	// Only this confirmed cleanup prefix is mutable; resource identities are not.
	CleanedDatabases int `json:"cleaned_databases,omitempty"`
}

type DatabaseIntent struct {
	InstanceID      int64                   `json:"instance_id"`
	InstanceAppName string                  `json:"instance_app_name"`
	Database        dbservice.CloneDatabase `json:"database"`
}

type journalDB interface {
	db.DBTX
	Begin(context.Context) (pgx.Tx, error)
}

type Journal struct{ pool journalDB }

func NewJournal(pool journalDB) *Journal { return &Journal{pool: pool} }

// Acquire excludes request and recovery executors in the supported single
// control-plane process, without occupying the SQL pool during external calls.
// A process crash releases this guard. Durable intent/state prevents resuming
// unfinished provisioning; target-PG abort fences stop delayed old execs.
// Running multiple control-plane processes against one state DB is unsupported.
func (j *Journal) Acquire(ctx context.Context, key string) (func(), error) {
	if !requestKeyPattern.MatchString(key) {
		return nil, ErrRequestConflict
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name := "environment-clone:" + key
	if !oplock.TryAcquire(name) {
		return nil, ErrBusy
	}
	var once sync.Once
	return func() { once.Do(func() { oplock.Release(name) }) }, nil
}

// Reserve binds the key forever, but holds the slug only while work is pending.
// Authorization and source snapshot reads belong to the coordinator. The
// returned deadline, never a new caller deadline on replay, governs all execs.
func (j *Journal) Reserve(ctx context.Context, p db.InsertEnvironmentCloneParams) (db.EnvironmentClone, error) {
	if !requestKeyPattern.MatchString(p.RequestKey) || p.UserID <= 0 || p.OrganizationID <= 0 || p.ProjectID <= 0 || p.SourceID <= 0 || p.DestinationSlug == "" || p.Deadline.IsZero() {
		return db.EnvironmentClone{}, ErrRequestConflict
	}
	tx, err := j.pool.Begin(ctx)
	if err != nil {
		return db.EnvironmentClone{}, err
	}
	defer rollback(tx)
	q := db.New(tx)
	if err := q.InsertEnvironmentClone(ctx, p); err != nil {
		return db.EnvironmentClone{}, err
	}
	op, err := q.LockEnvironmentClone(ctx, p.RequestKey)
	if err != nil {
		return op, err
	}
	if op.UserID != p.UserID || op.OrganizationID != p.OrganizationID || op.ProjectID != p.ProjectID || op.SourceID != p.SourceID || op.DestinationName != p.DestinationName || op.DestinationSlug != p.DestinationSlug {
		return db.EnvironmentClone{}, ErrRequestConflict
	}
	if op.State == "preparing" {
		var taken bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT FROM environments WHERE project_id=$1 AND slug=$2)", p.ProjectID, p.DestinationSlug).Scan(&taken); err != nil {
			return op, err
		}
		if taken {
			return op, ErrNameConflict
		}
		if _, err := q.ReserveEnvironmentCloneSlug(ctx, db.ReserveEnvironmentCloneSlugParams{ProjectID: p.ProjectID, Slug: p.DestinationSlug, RequestKey: p.RequestKey}); err != nil {
			return op, err
		}
		var owner string
		if err := tx.QueryRow(ctx, "SELECT request_key FROM environment_clone_reservations WHERE project_id=$1 AND slug=$2", p.ProjectID, p.DestinationSlug).Scan(&owner); err != nil {
			return op, err
		}
		if owner != p.RequestKey {
			return op, ErrNameConflict
		}
	}
	return op, tx.Commit(ctx)
}

func (j *Journal) Outcome(ctx context.Context, key string) (db.EnvironmentClone, error) {
	return db.New(j.pool).GetEnvironmentClone(ctx, key)
}

// RecordIntent is write-once and precedes every external mutation. An operation
// without external resources still records an empty typed intent before publish.
func (j *Journal) RecordIntent(ctx context.Context, key string, intent ResourceIntent) error {
	op, err := j.Outcome(ctx, key)
	if err != nil {
		return err
	}
	if intent.CleanedDatabases != 0 {
		return ErrState
	}
	for _, resource := range intent.Databases {
		c := resource.Database
		if resource.InstanceID <= 0 || resource.InstanceAppName == "" || c.Operation != key || !c.Deadline.Equal(op.Deadline) || !dbservice.ValidIdent(c.DBName) || !dbservice.ValidIdent(c.Username) {
			return errors.New("invalid clone database intent")
		}
	}
	payload, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	n, err := db.New(j.pool).RecordEnvironmentCloneIntent(ctx, db.RecordEnvironmentCloneIntentParams{RequestKey: key, ResourceIntent: string(payload)})
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrState
	}
	return nil
}

// StartCleanup is a durable fence against publication. It must succeed BEFORE
// deleting anything externally. Its conditional update serializes with Publish;
// a published operation can never be switched into cleanup, even after a lost
// commit reply. A failed/ambiguous update is not permission to perform cleanup.
func (j *Journal) StartCleanup(ctx context.Context, key string) error {
	n, err := db.New(j.pool).SetEnvironmentCloneCleanup(ctx, key)
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrState
	}
	return nil
}

// AdvanceCleanup records exactly one externally confirmed cleanup under the
// operation execution guard. CAS preserves identities and rejects stale writes;
// the cleanup state fence prevents publication or progress on a published clone.
func (j *Journal) AdvanceCleanup(ctx context.Context, op db.EnvironmentClone) (string, error) {
	var intent ResourceIntent
	if op.State != "cleanup" || json.Unmarshal([]byte(op.ResourceIntent), &intent) != nil || intent.CleanedDatabases < 0 || intent.CleanedDatabases >= len(intent.Databases) || intent.CleanedDatabases >= 2147483647 {
		return "", ErrState
	}
	payload, err := db.New(j.pool).AdvanceEnvironmentCloneCleanup(ctx, db.AdvanceEnvironmentCloneCleanupParams{
		RequestKey: op.RequestKey, ExpectedIntent: op.ResourceIntent, CleanedDatabases: int32(intent.CleanedDatabases + 1),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrState
	}
	return payload, err
}

func (j *Journal) CompleteCleanup(ctx context.Context, key string) error {
	tx, err := j.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(tx)
	q := db.New(tx)
	n, err := q.CompleteEnvironmentCloneCleanup(ctx, key)
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrState
	}
	if err := q.ReleaseEnvironmentCloneSlug(ctx, key); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Publish runs final existence/tenant checks and inserts supplied by the
// coordinator in the SAME transaction as the success marker. The callback must
// use captured sanitized values and must never perform external operations.
// Any commit error is conservatively ambiguous: callers must read Outcome on a
// detached context and must not automatically compensate.
func (j *Journal) Publish(ctx context.Context, key string, insert func(pgx.Tx) (int64, error)) (int64, error) {
	tx, err := j.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	q := db.New(tx)
	op, err := q.LockEnvironmentClone(ctx, key)
	if err != nil {
		return 0, err
	}
	if op.State != "preparing" || op.ResourceIntent == "" {
		return 0, ErrState
	}
	targetID, err := insert(tx)
	if err != nil {
		return 0, err
	}
	if targetID <= 0 {
		return 0, errors.New("invalid clone publication target")
	}
	n, err := q.PublishEnvironmentClone(ctx, db.PublishEnvironmentCloneParams{RequestKey: key, TargetID: &targetID})
	if err != nil {
		return 0, err
	}
	if n != 1 {
		return 0, ErrState
	}
	if err := q.ReleaseEnvironmentCloneSlug(ctx, key); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrOutcomeUnknown, err)
	}
	return targetID, nil
}
