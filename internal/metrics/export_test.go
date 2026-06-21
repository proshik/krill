package metrics

import "context"

func (s *Sampler) TickForTest(ctx context.Context) { s.tick(ctx) }

// PruneEvery exposes the package-level pruneEvery constant for white-box tests.
const PruneEvery = pruneEvery
