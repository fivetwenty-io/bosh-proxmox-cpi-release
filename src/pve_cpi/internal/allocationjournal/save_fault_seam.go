package allocationjournal

import "context"

// saveFaultKey carries a test's save fault on the request context.
type saveFaultKey struct{}

// WithSaveFaultForTest returns a context whose Acquire handles call fault
// with each record they are about to write, after the write has passed
// validation. A non-nil error fails that write before it reaches the disk and
// poisons the handle, the same way a failed write does. A nil fault leaves
// ctx as it is. It rides the context rather than a package variable, so it
// fails only the writes made under that context, and tests that set it can
// run in parallel. Production code never calls it.
func WithSaveFaultForTest(ctx context.Context, fault func(Record) error) context.Context {
	if fault == nil {
		return ctx
	}
	return context.WithValue(ctx, saveFaultKey{}, fault)
}

// saveFaultFor returns the save fault ctx carries when a test set one, and
// nil otherwise.
func saveFaultFor(ctx context.Context) func(Record) error {
	fault, _ := ctx.Value(saveFaultKey{}).(func(Record) error)
	return fault
}
