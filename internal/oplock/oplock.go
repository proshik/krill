// Package oplock provides process-wide named try-locks to serialize mutually
// exclusive operations (e.g. a DB-instance volume migration vs a backup of a
// database inside that instance) across packages without import cycles.
package oplock

import "sync"

var (
	mu   sync.Mutex
	held = map[string]bool{}
)

// TryAcquire takes the named lock if free; false when already held.
func TryAcquire(name string) bool {
	mu.Lock()
	defer mu.Unlock()
	if held[name] {
		return false
	}
	held[name] = true
	return true
}

// Release frees the named lock (no-op if not held).
func Release(name string) {
	mu.Lock()
	defer mu.Unlock()
	delete(held, name)
}

// Held reports whether the named lock is taken. Advisory fail-fast check —
// NOT mutual exclusion (the state may change right after it returns).
func Held(name string) bool {
	mu.Lock()
	defer mu.Unlock()
	return held[name]
}

// DBInstance is the lock name for a DB instance, keyed by its unique AppName
// (both dbservice and backup have it without extra queries).
func DBInstance(appName string) string { return "dbinst:" + appName }
