// Package flash carries a one-shot user notification (success/error) through
// the request context so the layout can render it as a toast.
package flash

import "context"

// Flash is a one-shot notification. Kind is "ok" or "err".
type Flash struct {
	Kind string
	Msg  string
}

type ctxKey struct{}

// With returns a copy of ctx carrying f.
func With(ctx context.Context, f *Flash) context.Context {
	return context.WithValue(ctx, ctxKey{}, f)
}

// From returns the flash in ctx, or nil.
func From(ctx context.Context) *Flash {
	f, _ := ctx.Value(ctxKey{}).(*Flash)
	return f
}
