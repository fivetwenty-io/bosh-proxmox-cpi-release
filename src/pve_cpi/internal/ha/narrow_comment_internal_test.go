package ha

import (
	"context"
	"slices"
	"testing"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// inOrderStates is the read sequence of a barrier that drains on a and runs
// the change there.
func inOrderStates() []*pve.HAServiceState {
	return []*pve.HAServiceState{
		crm("started", 100, 90, 95),
		crm("started", 110, 90, 95),
		dropped("a", 110, 125, 130),
	}
}

// addToken appends token to the rig's comment the way another writer of the
// resource does, as one write of resources.cfg.
func (r *barrierRig) addToken(token string) {
	c := r.currentComment() + " " + token
	r.comment = &c
	r.version++
}

const otherToken = "bosh-failback-hold=1700000000"

// TestNarrowKeepsATokenWrittenDuringTheBarrier is another writer that adds its
// token to the resource's comment while the barrier holds it ignored. The
// request that puts back the state must keep that token, because the barrier
// owns only its own.
func TestNarrowKeepsATokenWrittenDuringTheBarrier(t *testing.T) {
	r := &barrierRig{state: "started", settleOn: []string{"a"}, states: inOrderStates(), digests: true}
	in := r.input(&pinAB, &pinA)
	writePin := in.WritePin
	in.WritePin = func(ctx context.Context, pin *Rule) error {
		r.addToken(otherToken)
		return writePin(ctx, pin)
	}
	if _, err := NarrowActive(context.Background(), r.deps(), in); err != nil {
		t.Fatal(err)
	}
	want := []string{"request:ignored", "pin:a", "mutate:a", "reach:a", "request:started"}
	if !slices.Equal(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
	if r.state != "started" || r.currentComment() != "owner "+otherToken {
		t.Fatalf("state %q comment %q, want started with the other writer's token", r.state, r.currentComment())
	}
}

// TestNarrowKeepsTheHoldItsMoveWrote is a VM that drained outside the
// narrower set, so the barrier moves it into the set, and the move writes the
// failback hold right before the migrate request. The barrier runs again with
// the VM in the set, and the hold must still be on the resource when it ends,
// so failback stays off until the caller releases the hold.
func TestNarrowKeepsTheHoldItsMoveWrote(t *testing.T) {
	r := &barrierMover{barrierRig: &barrierRig{state: "started", settleOn: []string{"b", "b", "b", "a", "a"},
		digests: true,
		states: append(drainedOnB(),
			managedOn("started", "b", 140, 130),
			managedOn("started", "b", 140, 130),
			managedOn("started", "b", 150, 140),
			managedOn("migrate", "b", 160, 155),
			managedOn("started", "a", 170, 165),
			crm("started", 180, 175, 176),
			crm("started", 190, 175, 176),
			dropped("a", 190, 200, 205),
		)}}
	deps, in := r.wire()
	in.MoveInto = func(ctx context.Context, node string, set []string) (pve.VMLocation, error) {
		return MoveIntoSet(ctx, deps, MoveInput{VMID: 101, Node: node, Set: set, Targets: set, Budget: time.Minute,
			BeforeMigrate: func(context.Context) error {
				r.addToken(otherToken)
				return nil
			}})
	}
	if _, err := NarrowActive(context.Background(), deps, in); err != nil {
		t.Fatal(err)
	}
	if r.state != "started" || r.currentComment() != "owner "+otherToken {
		t.Fatalf("state %q comment %q, want started with the hold", r.state, r.currentComment())
	}
}

// TestNarrowRequestComparesTheDigest is another writer that adds its token
// between the barrier's read of the resource and its ignored request. The
// request names the digest of its read, so PVE refuses it, and the barrier
// reads again and keeps the token.
func TestNarrowRequestComparesTheDigest(t *testing.T) {
	r := &barrierRig{state: "started", settleOn: []string{"a"}, states: inOrderStates(), digests: true}
	r.beforeUpdate = func(n int) {
		if n == 1 {
			r.addToken(otherToken)
		}
	}
	if _, err := NarrowActive(context.Background(), r.deps(), r.input(&pinAB, &pinA)); err != nil {
		t.Fatal(err)
	}
	if r.updates != 3 {
		t.Fatalf("updates %d, want 3: the refused request, its retry, and the restore", r.updates)
	}
	if r.state != "started" || r.currentComment() != "owner "+otherToken {
		t.Fatalf("state %q comment %q, want started with the other writer's token", r.state, r.currentComment())
	}
}

// TestNarrowRequestThatKeepsLosingTheRaceFails is a resources.cfg that changes
// under every write. The ignored request gives up after CommentWriteAttempts
// writes and leaves the resource as it was, and a restore that gives up leaves
// the resource ignored with the barrier's token for the next call to finish.
func TestNarrowRequestThatKeepsLosingTheRaceFails(t *testing.T) {
	t.Run("the ignored request", func(t *testing.T) {
		r := &barrierRig{state: "started", settleOn: []string{"a"}, states: inOrderStates(), digests: true}
		r.beforeUpdate = func(int) { r.version++ }
		_, err := NarrowActive(context.Background(), r.deps(), r.input(&pinAB, &pinA))
		if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
			t.Fatalf("err = %v, want a retriable error", err)
		}
		if r.updates != CommentWriteAttempts || len(r.events) != 0 {
			t.Fatalf("updates %d events %v, want %d refused writes", r.updates, r.events, CommentWriteAttempts)
		}
		if r.state != "started" || r.currentComment() != "owner" {
			t.Fatalf("state %q comment %q, want the resource as it was", r.state, r.currentComment())
		}
	})
	t.Run("the restore", func(t *testing.T) {
		r := &barrierRig{state: "started", settleOn: []string{"a"}, states: inOrderStates(), digests: true}
		r.beforeUpdate = func(n int) {
			if n > 1 {
				r.version++
			}
		}
		_, err := NarrowActive(context.Background(), r.deps(), r.input(&pinAB, &pinA))
		if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
			t.Fatalf("err = %v, want a retriable error", err)
		}
		if r.updates != 1+CommentWriteAttempts {
			t.Fatalf("updates %d, want %d", r.updates, 1+CommentWriteAttempts)
		}
		if from, marked := narrowMarkOf(r.comment); r.state != "ignored" || !marked || from != "started" {
			t.Fatalf("state %q comment %q, want ignored with the token that names started", r.state,
				r.currentComment())
		}
	})
}
