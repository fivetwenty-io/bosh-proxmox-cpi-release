package ha

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// barrierMover is a barrier rig whose MoveInto runs MoveIntoSet on the same
// scripted reads, so a test follows a whole barrier with a move inside it.
type barrierMover struct {
	*barrierRig
	migrateErr error
}

func (r *barrierMover) CreateHaResourcesMigrate(_ context.Context, _ string,
	p *sdkcluster.CreateHaResourcesMigrateParams,
) (*sdkcluster.CreateHaResourcesMigrateResponse, error) {
	r.events = append(r.events, "migrate:"+p.Node)
	return &sdkcluster.CreateHaResourcesMigrateResponse{}, r.migrateErr
}

func (r *barrierMover) wire() (NarrowDeps, NarrowInput) {
	deps := r.deps()
	deps.Migrate = r
	in := r.input(&pinAB, &pinA)
	in.MoveInto = func(ctx context.Context, node string, set []string) (pve.VMLocation, error) {
		return MoveIntoSet(ctx, deps, MoveInput{VMID: 101, Node: node, Set: set, Targets: set, Budget: time.Minute})
	}
	return deps, in
}

// drainedOnB is the read sequence of a barrier whose drain ends with the VM
// on b, which an HA migration the CRM had queued before the ignored request
// leaves there.
func drainedOnB() []*pve.HAServiceState {
	return []*pve.HAServiceState{
		crm("started", 100, 90, 95),
		crm("migrate", 110, 90, 95),
		dropped("a", 110, 120, 125),
		dropped("b", 110, 130, 135),
	}
}

func TestNarrowMovesAVMThatDrainedOutsideTheSetAndKeepsItUnderHA(t *testing.T) {
	r := &barrierMover{barrierRig: &barrierRig{state: "started", settleOn: []string{"b", "b", "b", "a", "a"},
		states: append(drainedOnB(),
			// The move: the VM is still on b and PVE HA holds it as started.
			managedOn("started", "b", 140, 130),
			managedOn("started", "b", 140, 130),
			managedOn("started", "b", 150, 140),
			managedOn("migrate", "b", 160, 155),
			managedOn("started", "a", 170, 165),
			// The barrier again, with the VM on a.
			crm("started", 180, 175, 176),
			crm("started", 190, 175, 176),
			dropped("a", 190, 200, 205),
		)}}
	deps, in := r.wire()
	loc, err := NarrowActive(context.Background(), deps, in)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"request:ignored", "request:started", "migrate:a", "request:ignored", "pin:a", "mutate:a",
		"reach:a", "request:started"}
	if loc.Node != "a" || !slices.Equal(r.events, want) {
		t.Fatalf("loc %+v events %v, want %v", loc, r.events, want)
	}
	if r.state != "started" || r.currentComment() != "owner" {
		t.Fatalf("state %q comment %q, want started with no token", r.state, r.currentComment())
	}
}

func TestNarrowKeepsHAWhenPVERefusesTheMove(t *testing.T) {
	r := &barrierMover{barrierRig: &barrierRig{state: "started", settleOn: []string{"b"},
		states: append(drainedOnB(), managedOn("started", "b", 140, 130))},
		migrateErr: &sdkerrors.APIError{HTTPCode: 500, Message: "resource not allowed on target node"}}
	deps, in := r.wire()
	_, err := NarrowActive(context.Background(), deps, in)
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err = %v", err)
	}
	for _, part := range []string{"outside the narrower set a", "PVE HA did not move it into the set",
		"stays under PVE HA with its old pin", "resource not allowed on target node",
		"ha-manager migrate vm:101 a", "and retry"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("err %q lacks %q", err, part)
		}
	}
	if !slices.Equal(r.events, []string{"request:ignored", "request:started", "migrate:a"}) || r.state != "started" {
		t.Fatalf("events %v state %q", r.events, r.state)
	}
}

func TestNarrowDefersAVMOnANodeThatIsDown(t *testing.T) {
	down := managedOn("started", "b", 140, 130)
	down.NodeHealth = pve.NodeHealthEntry{Name: "b", DeadOrFenced: true}
	r := &barrierMover{barrierRig: &barrierRig{state: "started", settleOn: []string{"b"},
		states: append(drainedOnB(), down)}}
	deps, in := r.wire()
	_, err := NarrowActive(context.Background(), deps, in)
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || !strings.Contains(err.Error(), "node b is offline") ||
		!strings.Contains(err.Error(), "narrower set a") {
		t.Fatalf("err = %v", err)
	}
	if !slices.Equal(r.events, []string{"request:ignored", "request:started"}) || r.state != "started" {
		t.Fatalf("events %v state %q", r.events, r.state)
	}
}

func TestNarrowMovesTheVMOnlyOnce(t *testing.T) {
	// PVE HA moves the VM into the set, and it drains outside the set again.
	r := &barrierMover{barrierRig: &barrierRig{state: "started",
		settleOn: []string{"b", "b", "b", "a", "b", "b"},
		states: append(drainedOnB(),
			managedOn("started", "b", 140, 130),
			managedOn("started", "b", 140, 130),
			managedOn("started", "a", 150, 145),
			crm("started", 180, 175, 176),
			crm("migrate", 190, 175, 176),
			dropped("b", 190, 200, 205),
		)}}
	deps, in := r.wire()
	_, err := NarrowActive(context.Background(), deps, in)
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || !strings.Contains(err.Error(), "already moved it") {
		t.Fatalf("err = %v", err)
	}
	if want := []string{"request:ignored", "request:started", "migrate:a", "request:ignored",
		"request:started"}; !slices.Equal(r.events, want) {
		t.Fatalf("events %v, want %v", r.events, want)
	}
}
