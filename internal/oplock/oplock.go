// Package oplock provides process-wide named try-locks to serialize mutually
// exclusive operations (e.g. a DB-instance volume migration vs a backup of a
// database inside that instance) across packages without import cycles.
package oplock

import (
	"context"
	"strconv"
	"sync"
)

var (
	mu     sync.Mutex
	held   = map[string]chan struct{}{}
	marked = map[string]int{}
)

// TryAcquire takes the named lock if free; false when already held.
func TryAcquire(name string) bool {
	mu.Lock()
	defer mu.Unlock()
	if held[name] != nil {
		return false
	}
	held[name] = make(chan struct{})
	return true
}

// Release frees the named lock (no-op if not held).
func Release(name string) {
	mu.Lock()
	defer mu.Unlock()
	if changed := held[name]; changed != nil {
		delete(held, name)
		close(changed)
	}
}

// Held reports whether the named lock is taken. Advisory fail-fast check —
// NOT mutual exclusion (the state may change right after it returns).
func Held(name string) bool {
	mu.Lock()
	defer mu.Unlock()
	return held[name] != nil
}

// Bucket serialises changes to one bucket's principals and links (rotate,
// revoke, delete, link): each reads state, changes the S3 server and records
// the result as separate steps.
func Bucket(id int64) string { return "bucket:" + strconv.FormatInt(id, 10) }

// DBInstance is the lock name for a DB instance, keyed by its unique AppName
// (both dbservice and backup have it without extra queries).
func DBInstance(appName string) string { return "dbinst:" + appName }

// DBInstanceDeploy serialises deploys of one DB instance. Keyed by row id
// because the trigger has the id before it has loaded the row — and separate
// from DBInstance so a migration, which redeploys internally, is not blocked by
// its own deploy.
func DBInstanceDeploy(id int64) string { return "dbinst-deploy:" + strconv.FormatInt(id, 10) }

// Acquire waits without retaining entries for idle keys. Cancellation never
// takes ownership, and Release wakes all waiters to retry under the mutex.
func Acquire(ctx context.Context, name string) error {
	for {
		mu.Lock()
		if err := ctx.Err(); err != nil {
			mu.Unlock()
			return err
		}
		changed := held[name]
		if changed == nil {
			held[name] = make(chan struct{})
			mu.Unlock()
			return nil
		}
		mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

// LogicalDB serializes creating a link to a logical database with its
// deletion: deletion refuses while links exist, so a link must not commit
// between that check and the drop.
func LogicalDB(id int64) string { return "ldb:" + strconv.FormatInt(id, 10) }

// AppRouting serializes final spec application with current routing updates.
func AppRouting(id int64) string { return "app-routing:" + strconv.FormatInt(id, 10) }

// Mark registers one more holder of the named marker and returns its release.
// Unlike a lock a marker has any number of holders; it lives in its own name
// space, so a lock and a marker may share a name.
func Mark(name string) (unmark func()) {
	mu.Lock()
	marked[name]++
	mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			mu.Lock()
			if marked[name]--; marked[name] <= 0 {
				delete(marked, name)
			}
			mu.Unlock()
		})
	}
}

// Marked reports whether the named marker has a holder.
func Marked(name string) bool {
	mu.Lock()
	defer mu.Unlock()
	return marked[name] > 0
}

// AppRestore names an app's volume restores. Each restore marks it (Mark)
// from its scale-down to the end of its scale-back, taking the lock of the
// same name only while it marks; the status heal holds the lock from its read
// of the service to its write and skips a marked app, so it never takes a
// restore's zero for a stop. Restores of one app do not wait for each other.
func AppRestore(id int64) string { return "app-restore:" + strconv.FormatInt(id, 10) }

// CloneProject serializes environment cloning with project/environment deletion.
func CloneProject(id int64) string { return "clone-project:" + strconv.FormatInt(id, 10) }

// AppPlacement serializes placement persistence/labels with final deployment.
func AppPlacement(id int64) string { return "app-placement:" + strconv.FormatInt(id, 10) }
