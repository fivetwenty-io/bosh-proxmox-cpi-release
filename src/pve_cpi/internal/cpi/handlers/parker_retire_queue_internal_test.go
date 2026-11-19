package handlers

import (
	"context"
	"testing"
	"time"
)

// drainedRetirementErrs runs call under a retirement queue on a request
// context that the test then cancels, drains the queue, and returns the error
// each drained retirement's context reported when it ran.
func drainedRetirementErrs(t *testing.T, call func(qctx context.Context)) []error {
	t.Helper()
	var errs []error
	defer setRetireEmptiedParkerForTest(func(ctx context.Context, _ Deps, _ string, _ int, _ string) {
		errs = append(errs, ctx.Err())
	})()
	reqCtx, cancelReq := context.WithTimeout(context.Background(), time.Hour)
	qctx, drain := deferParkerRetirements(reqCtx)
	call(qctx)
	cancelReq()
	drain()
	return errs
}

// TestDeferParkerRetirements_ADetachedCallerOutlivesTheRequest proves a
// retirement called on a context that detachedContext or finishingContext
// detached from the request still runs free of the request's cancellation
// when the queue drains it, although such a context has a Done channel of its
// own.
func TestDeferParkerRetirements_ADetachedCallerOutlivesTheRequest(t *testing.T) {
	detachers := map[string]func(context.Context) (context.Context, context.CancelFunc){
		"detachedContext": func(ctx context.Context) (context.Context, context.CancelFunc) {
			return detachedContext(ctx, time.Minute)
		},
		"finishingContext": func(ctx context.Context) (context.Context, context.CancelFunc) {
			return finishingContext(ctx, time.Minute)
		},
	}
	for name, detach := range detachers {
		t.Run(name, func(t *testing.T) {
			errs := drainedRetirementErrs(t, func(qctx context.Context) {
				dctx, cancel := detach(qctx)
				defer cancel()
				retireEmptiedParker(dctx, Deps{}, phNode, 90000, "test")
			})
			if len(errs) != 1 || errs[0] != nil {
				t.Fatalf("the drain must run the detached caller's retirement free of the request's cancellation; "+
					"context errors %v", errs)
			}
		})
	}
}

// TestDeferParkerRetirements_ARequestCallerStaysCancelable proves a
// retirement called on the request's own context runs on it when the queue
// drains it, so a request that ended stops it.
func TestDeferParkerRetirements_ARequestCallerStaysCancelable(t *testing.T) {
	errs := drainedRetirementErrs(t, func(qctx context.Context) {
		retireEmptiedParker(qctx, Deps{}, phNode, 90000, "test")
	})
	if len(errs) != 1 || errs[0] == nil {
		t.Fatalf("the drain must run the request's retirement on the request's context; context errors %v", errs)
	}
}

// TestDeferParkerRetirements_ADuplicateKeepsTheDetachedCallersFlag proves the
// queue keeps one retirement per parker, and that the one it keeps runs free
// of the request's cancellation when any caller of it was, whichever came
// first.
func TestDeferParkerRetirements_ADuplicateKeepsTheDetachedCallersFlag(t *testing.T) {
	orders := map[string]bool{"request caller first": true, "detached caller first": false}
	for name, requestFirst := range orders {
		t.Run(name, func(t *testing.T) {
			errs := drainedRetirementErrs(t, func(qctx context.Context) {
				calls := []context.Context{qctx, context.WithoutCancel(qctx)}
				if !requestFirst {
					calls[0], calls[1] = calls[1], calls[0]
				}
				for _, ctx := range calls {
					retireEmptiedParker(ctx, Deps{}, phNode, 90000, "test")
				}
			})
			if len(errs) != 1 || errs[0] != nil {
				t.Fatalf("the drain must run the parker once, free of the request's cancellation; context errors %v",
					errs)
			}
		})
	}
}
