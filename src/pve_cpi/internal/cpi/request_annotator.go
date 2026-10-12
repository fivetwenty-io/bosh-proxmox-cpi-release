package cpi

import "context"

// annotatorCtxKey is the unexported key type for the per-request annotator
// slot stored in a context.
type annotatorCtxKey struct{}

// annotatorSlot carries the annotator a handler built from its effective Deps
// (the job-level Deps after WithRequestOverrides). The middleware installs the
// slot before the handler runs, the handler fills it, and the After hooks read
// it, so a hook writes through the same PVE client the handler used instead of
// the job-level one. It is a pointer in the context because the handler's
// context is derived from, and never visible to, the hooks' context.
type annotatorSlot struct {
	v any
}

// withAnnotatorSlot installs a fresh slot into ctx unless one is already
// present, mirroring withRollbackHolder for nested WrapHandler calls.
func withAnnotatorSlot(ctx context.Context) context.Context {
	if ctx.Value(annotatorCtxKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, annotatorCtxKey{}, &annotatorSlot{})
}

// PublishRequestAnnotator records v, an annotator built from the handler's
// effective Deps, for the After hooks of this request. When ctx carries no slot
// (bare context or unhooked dispatch) the call is a no-op.
func PublishRequestAnnotator(ctx context.Context, v any) {
	if s, _ := ctx.Value(annotatorCtxKey{}).(*annotatorSlot); s != nil {
		s.v = v
	}
}

// RequestAnnotator returns the value a handler published with
// PublishRequestAnnotator, or nil when none was published.
func RequestAnnotator(ctx context.Context) any {
	if s, _ := ctx.Value(annotatorCtxKey{}).(*annotatorSlot); s != nil {
		return s.v
	}
	return nil
}
