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
	mu   sync.Mutex
	held = map[string]chan struct{}{}
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

// AppRouting serializes final spec application with current routing updates.
func AppRouting(id int64) string { return "app-routing:" + strconv.FormatInt(id, 10) }

// CloneProject serializes environment cloning with project/environment deletion.
func CloneProject(id int64) string { return "clone-project:" + strconv.FormatInt(id, 10) }

// AppPlacement serializes placement persistence/labels with final deployment.
func AppPlacement(id int64) string { return "app-placement:" + strconv.FormatInt(id, 10) }
