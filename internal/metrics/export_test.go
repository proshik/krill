package metrics

import "context"

func (s *Sampler) TickForTest(ctx context.Context) { s.tick(ctx) }
