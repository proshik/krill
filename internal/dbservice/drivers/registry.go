package drivers

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

// MustGet looks up a driver by engine name, returning nil if not registered.
// Callers that reach here with an unknown engine have a data-integrity bug
// (the engine CHECK constraint only allows registered values); a nil Driver
// panics loudly on first use, rather than silently no-op'ing.
func (r *registry) MustGet(e string) Driver { return r.m[e] }

// List returns every registered driver in stable (registration) order.
func (r *registry) List() []Driver {
	out := make([]Driver, 0, len(r.order))
	for _, e := range r.order {
		out = append(out, r.m[e])
	}
	return out
}

// Registry is the process-wide driver set: postgres + redis today.
var Registry = New(&postgresDriver{}, &redisDriver{})
