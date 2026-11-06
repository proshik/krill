package drivers

import (
	"errors"
	"fmt"
)

// registry looks up a Driver by engine name.
type registry struct {
	m     map[string]Driver
	order []string // insertion order, for a stable List()
}

// New builds a registry from a stable-ordered list of drivers.
func New(ds ...Driver) *registry {
	r := &registry{m: map[string]Driver{}}
	for _, d := range ds {
		r.m[d.Engine()] = d
		r.order = append(r.order, d.Engine())
	}
	return r
}

// Get looks up a driver by engine name.
func (r *registry) Get(e string) (Driver, bool) {
	d, ok := r.m[e]
	return d, ok
}

// ErrUnknownEngine is returned by Lookup for an engine no driver handles: a row
// written by a newer Krill (rolled back to this one), or a damaged one.
var ErrUnknownEngine = errors.New("unsupported database engine")

// Lookup returns the driver for engine, or an error naming it. Code that acts
// on an instance (deploy, park, proxy, delete) uses this, never MustGet: a
// nil driver panics, and those paths run in goroutines whose panic takes the
// whole process down.
func (r *registry) Lookup(e string) (Driver, error) {
	if d, ok := r.m[e]; ok {
		return d, nil
	}
	return nil, fmt.Errorf("%w %q (written by a newer Krill?)", ErrUnknownEngine, e)
}

// MustGet looks up a driver by engine name, returning nil if not registered.
// Callers that reach here with an unknown engine have a data-integrity bug
// (the engine CHECK constraint only allows registered values); a nil Driver
// panics loudly on first use, rather than silently no-op'ing.
func (r *registry) MustGet(e string) Driver { return r.m[e] }

// Register adds (or replaces) a driver in the registry. Exposed mainly for
// tests that need to exercise multi-target behavior (e.g. reconcileProxy's
// N-target loop) ahead of a real N-target driver landing; production drivers
// are wired once via New(...) below.
func (r *registry) Register(d Driver) {
	if _, exists := r.m[d.Engine()]; !exists {
		r.order = append(r.order, d.Engine())
	}
	r.m[d.Engine()] = d
}

// List returns every registered driver in stable (registration) order.
func (r *registry) List() []Driver {
	out := make([]Driver, 0, len(r.order))
	for _, e := range r.order {
		out = append(out, r.m[e])
	}
	return out
}

// Registry is the process-wide driver set: postgres + redis + dragonfly + minio + valkey.
var Registry = New(&postgresDriver{}, &redisDriver{}, &dragonflyDriver{}, &minioDriver{}, &valkeyDriver{})
