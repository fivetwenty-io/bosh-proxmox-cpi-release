package ha

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// endedRequestResources refuses every call on a context that has ended, the
// way the API client does, and passes the rest to the rig.
type endedRequestResources struct{ rig *barrierRig }

func (e endedRequestResources) GetHaResources(ctx context.Context, sid string,
) (*sdkcluster.GetHaResourcesResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return e.rig.GetHaResources(ctx, sid)
}

func (e endedRequestResources) CreateHaResources(ctx context.Context, p *sdkcluster.CreateHaResourcesParams) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return e.rig.CreateHaResources(ctx, p)
}

func (e endedRequestResources) UpdateHaResources(ctx context.Context, sid string,
	p *sdkcluster.UpdateHaResourcesParams,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) > narrowUnwindTimeout+time.Minute {
		return errors.New("the request ran on a context whose deadline is further out than the request had")
	}
	return e.rig.UpdateHaResources(ctx, sid, p)
}

func (e endedRequestResources) DeleteHaResources(ctx context.Context, sid string,
	p *sdkcluster.DeleteHaResourcesParams,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return e.rig.DeleteHaResources(ctx, sid, p)
}

// TestNarrowUnwindsAfterTheMutationSpentTheRequest is an attach whose wait for
// a parker's lock ran until the request's deadline. The mutation returns only
// once the request's context has ended, and every call made on that context
// fails. The pin revert and the request that puts back the resource's state
// must still go through on a context of their own, so the VM ends back in
// started under its old pin with its comment as it was.
func TestNarrowUnwindsAfterTheMutationSpentTheRequest(t *testing.T) {
	t.Parallel()
	r := &barrierRig{state: "started", settleOn: []string{"a"},
		states: []*pve.HAServiceState{crm("started", 100, 90, 95), dropped("a", 110, 120, 125)}}
	deps := r.deps()
	deps.Resources = endedRequestResources{rig: r}
	deps.Mutate = func(ctx context.Context, m pve.VMConfigMutation) (pve.VMLocation, error) {
		r.events = append(r.events, "mutate:"+m.Node)
		<-ctx.Done()
		return pve.VMLocation{}, errors.Join(errors.New("timed out waiting for the parker's lock"), ctx.Err())
	}
	in := r.input(&pinAB, &pinA)
	writePin := in.WritePin
	in.WritePin = func(ctx context.Context, pin *Rule) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return writePin(ctx, pin)
	}
	in.Moving = func(ctx context.Context, _ string) (bool, error) { return false, ctx.Err() }
	in.Unchanged = func(ctx context.Context, _ string) (bool, error) { return true, ctx.Err() }

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := NarrowActive(ctx, deps, in)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the mutation's own failure", err)
	}
	want := []string{"request:ignored", "pin:a", "mutate:a", "pin:" + pinAB.NodesParam(), "request:started"}
	if !slices.Equal(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
	if r.state != "started" || r.currentComment() != "owner" {
		t.Fatalf("the resource is %s with comment %q, want it back to started with its comment",
			r.state, r.currentComment())
	}
}

// TestNarrowRestoresWhenTheRequestEndsDuringTheDrain is a request that ends
// while the barrier waits for the CRM to let go. Nothing has touched the VM or
// its pin yet, so the barrier puts back the state it found rather than leave
// the resource ignored for a later call to finish.
func TestNarrowRestoresWhenTheRequestEndsDuringTheDrain(t *testing.T) {
	t.Parallel()
	r := &barrierRig{state: "started", settleOn: []string{"a"},
		states: []*pve.HAServiceState{crm("started", 100, 90, 95), crm("started", 110, 112, 113)}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps := r.deps()
	deps.Resources = endedRequestResources{rig: r}
	deps.Sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}
	_, err := NarrowActive(ctx, deps, r.input(nil, &pinA))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the request's cancellation", err)
	}
	if want := []string{"request:ignored", "request:started"}; !slices.Equal(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
	if r.state != "started" || r.currentComment() != "owner" {
		t.Fatalf("the resource is %s with comment %q, want it back to started with its comment",
			r.state, r.currentComment())
	}
}

type unwindValueKey struct{}

// TestUnwindContext pins the context each unwind step gets: free of the
// request's cancellation and carrying its values, with the longer of the
// unwind's own bound and the time the request still has, and no deadline when
// the request has none and is still live.
func TestUnwindContext(t *testing.T) {
	t.Parallel()
	base := context.WithValue(context.Background(), unwindValueKey{}, "request")
	check := func(t *testing.T, ctx context.Context, wantLeft time.Duration) {
		t.Helper()
		got, cancel := unwindContext(ctx)
		defer cancel()
		if err := got.Err(); err != nil {
			t.Fatalf("the unwind context has already ended: %v", err)
		}
		if got.Value(unwindValueKey{}) != "request" {
			t.Fatal("the unwind context lost the request's values")
		}
		deadline, ok := got.Deadline()
		if wantLeft == 0 {
			if ok {
				t.Fatalf("the unwind context has a deadline %v, want none", deadline)
			}
			return
		}
		if !ok {
			t.Fatal("the unwind context has no deadline")
		}
		if left := time.Until(deadline); left > wantLeft || left < wantLeft-5*time.Second {
			t.Fatalf("the unwind context has %v left, want about %v", left, wantLeft)
		}
	}
	t.Run("a live request without a deadline gives no deadline", func(t *testing.T) {
		t.Parallel()
		check(t, base, 0)
	})
	t.Run("a stopped request without a deadline gets the unwind's bound", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(base)
		cancel()
		check(t, ctx, narrowUnwindTimeout)
	})
	t.Run("a stopped request with time left gets the unwind's bound", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(base, time.Hour)
		cancel()
		check(t, ctx, narrowUnwindTimeout)
	})
	t.Run("a request past its deadline gets the unwind's bound", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithDeadline(base, time.Now().Add(-time.Second))
		defer cancel()
		check(t, ctx, narrowUnwindTimeout)
	})
	t.Run("a request with more time keeps it", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(base, time.Hour)
		defer cancel()
		check(t, ctx, time.Hour)
	})
	t.Run("a stop signal during a step leaves it the window", func(t *testing.T) {
		t.Parallel()
		const window = 300 * time.Millisecond
		withDeadline := func() (context.Context, context.CancelFunc) { return context.WithTimeout(base, time.Hour) }
		withoutDeadline := func() (context.Context, context.CancelFunc) { return context.WithCancel(base) }
		for name, request := range map[string]func() (context.Context, context.CancelFunc){
			"with a deadline": withDeadline, "without a deadline": withoutDeadline,
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				ctx, stopRequest := request()
				defer stopRequest()
				step, cancel := unwindContextWithin(ctx, window)
				defer cancel()
				signalled := time.Now()
				stopRequest()
				select {
				case <-step.Done():
				case <-time.After(30 * time.Second):
					t.Fatal("the step outlived its window after the request ended")
				}
				if spent := time.Since(signalled); spent < window {
					t.Fatalf("the step ended %v after the request did, want at least the %v window", spent, window)
				}
				if !errors.Is(step.Err(), context.Canceled) {
					t.Fatalf("the step ended with %v, want it cancelled when its window ran out", step.Err())
				}
			})
		}
	})
	t.Run("the caller's cancel ends a step without waiting for the window", func(t *testing.T) {
		t.Parallel()
		ctx, stopRequest := context.WithCancel(base)
		step, cancel := unwindContextWithin(ctx, time.Hour)
		stopRequest()
		cancel()
		select {
		case <-step.Done():
		case <-time.After(30 * time.Second):
			t.Fatal("the step's cancel did not end it")
		}
	})
}

// stopDuringRestore stands in for a stop signal that arrives while the
// barrier's unwind sends the request that puts back the resource's state.
type stopDuringRestore struct {
	endedRequestResources
	stop func()
}

func (s stopDuringRestore) UpdateHaResources(ctx context.Context, sid string,
	p *sdkcluster.UpdateHaResourcesParams,
) error {
	if p.State != nil && *p.State != haStateIgnored {
		s.stop()
	}
	return s.endedRequestResources.UpdateHaResources(ctx, sid, p)
}

// TestNarrowUnwindOutlivesAStopSignal is a request with no deadline, the
// default when operation_timeout is off, whose mutation fails for its own
// reason. A stop signal then ends the request while the unwind runs, either
// during the pin revert or during the request that puts back the state. Both
// steps must still go through, so the VM ends back in started under its old
// pin with its comment as it was.
func TestNarrowUnwindOutlivesAStopSignal(t *testing.T) {
	t.Parallel()
	for _, during := range []string{"the pin revert", "the state restore"} {
		t.Run(during, func(t *testing.T) {
			t.Parallel()
			r := &barrierRig{state: "started", settleOn: []string{"a"},
				states: []*pve.HAServiceState{crm("started", 100, 90, 95), dropped("a", 110, 120, 125)}}
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			deps := r.deps()
			deps.Resources = endedRequestResources{rig: r}
			if during == "the state restore" {
				deps.Resources = stopDuringRestore{endedRequestResources: endedRequestResources{rig: r}, stop: stop}
			}
			mutateErr := errors.New("the config write failed")
			deps.Mutate = func(_ context.Context, m pve.VMConfigMutation) (pve.VMLocation, error) {
				r.events = append(r.events, "mutate:"+m.Node)
				return pve.VMLocation{}, mutateErr
			}
			in := r.input(&pinAB, &pinA)
			writePin := in.WritePin
			in.WritePin = func(ctx context.Context, pin *Rule) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				return writePin(ctx, pin)
			}
			in.Moving = func(ctx context.Context, _ string) (bool, error) {
				if during == "the pin revert" {
					stop()
				}
				return false, ctx.Err()
			}
			in.Unchanged = func(ctx context.Context, _ string) (bool, error) { return true, ctx.Err() }

			_, err := NarrowActive(ctx, deps, in)
			if !errors.Is(err, mutateErr) {
				t.Fatalf("err = %v, want the mutation's own failure", err)
			}
			want := []string{"request:ignored", "pin:a", "mutate:a", "pin:" + pinAB.NodesParam(), "request:started"}
			if !slices.Equal(r.events, want) {
				t.Fatalf("events %v, want %v", r.events, want)
			}
			if r.state != "started" || r.currentComment() != "owner" {
				t.Fatalf("the resource is %s with comment %q, want it back to started with its comment",
					r.state, r.currentComment())
			}
		})
	}
}
