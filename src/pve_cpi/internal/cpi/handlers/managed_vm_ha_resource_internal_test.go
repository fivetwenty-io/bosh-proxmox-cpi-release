package handlers

import (
	"context"
	"errors"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// haResourceReadCluster answers GetHaResources for one HA resource, which is
// all the guard's admission and readback of a removal read.
type haResourceReadCluster struct {
	cluster.Service
	comment *string
	gone    bool
	readErr error
	reads   int
}

func (c *haResourceReadCluster) GetHaResources(_ context.Context, sid string) (*cluster.GetHaResourcesResponse, error) {
	c.reads++
	switch {
	case c.readErr != nil:
		return nil, c.readErr
	case c.gone:
		return nil, errors.New("no such resource '" + sid + "'")
	}
	state := "ignored"
	return &cluster.GetHaResourcesResponse{Sid: sid, State: &state, Comment: c.comment}, nil
}

func haResourceDeleteCall(sid string) ManagedAllocationMutation {
	return ManagedAllocationMutation{Service: "Cluster", Method: "DeleteHaResources", Args: map[string]any{"sid": sid}}
}

func stagedHAResourceCall(sid, state string) ManagedAllocationMutation {
	return ManagedAllocationMutation{Service: "Cluster", Method: "CreateHaResources", Args: map[string]any{
		managedArgumentParams: &cluster.CreateHaResourcesParams{Sid: sid, State: &state}}}
}

// TestManagedGuardAdmitsOnlyTheUnstageOfItsOwnHAResource lets the removal of
// the allocated VM's HA resource through only when this call staged it and the
// resource carries this Director's marker. Every refusal is one that leaves
// the guard usable.
func TestManagedGuardAdmitsOnlyTheUnstageOfItsOwnHAResource(t *testing.T) {
	ours, other := haIdentity{Owner: aaDirector}.marker(), haIdentity{Owner: aaOtherDirector}.marker()
	operator := "operator note"
	newAlloc := func(c *haResourceReadCluster) *managedVMAllocation {
		m := &managedVMAllocation{vmid: 100, deps: Deps{PVE: &haRulesReadClient{svc: c},
			RequestDirectorUUID: aaDirector}}
		m.noteStagedHAResource(stagedHAResourceCall("vm:100", "ignored"))
		return m
	}
	ctx := context.Background()

	if err := newAlloc(&haResourceReadCluster{comment: &ours}).admitHAResourceDelete(ctx,
		haResourceDeleteCall("vm:100")); err != nil {
		t.Fatalf("the unstage of our own staged resource was refused: %v", err)
	}
	if err := newAlloc(&haResourceReadCluster{gone: true}).admitHAResourceDelete(ctx,
		haResourceDeleteCall("vm:100")); err != nil {
		t.Fatalf("the removal of a resource that is gone was refused: %v", err)
	}

	notStaged := &managedVMAllocation{vmid: 100, deps: Deps{PVE: &haRulesReadClient{
		svc: &haResourceReadCluster{comment: &ours}}, RequestDirectorUUID: aaDirector}}
	noDirector := newAlloc(&haResourceReadCluster{comment: &ours})
	noDirector.deps.RequestDirectorUUID = ""
	for name, err := range map[string]error{
		"a resource this call did not stage": notStaged.admitHAResourceDelete(ctx, haResourceDeleteCall("vm:100")),
		"another VM's resource": newAlloc(&haResourceReadCluster{comment: &ours}).admitHAResourceDelete(ctx,
			haResourceDeleteCall("vm:101")),
		"a resource with no comment": newAlloc(&haResourceReadCluster{}).admitHAResourceDelete(ctx,
			haResourceDeleteCall("vm:100")),
		"an operator's resource": newAlloc(&haResourceReadCluster{comment: &operator}).admitHAResourceDelete(ctx,
			haResourceDeleteCall("vm:100")),
		"another Director's resource": newAlloc(&haResourceReadCluster{comment: &other}).admitHAResourceDelete(ctx,
			haResourceDeleteCall("vm:100")),
		"a call with no Director": noDirector.admitHAResourceDelete(ctx, haResourceDeleteCall("vm:100")),
	} {
		if !errors.Is(err, errManagedHAResourceDeleteRefused) {
			t.Errorf("%s: err = %v, want a refusal", name, err)
		}
	}

	unread := newAlloc(&haResourceReadCluster{readErr: errors.New("connection reset")})
	err := unread.admitHAResourceDelete(ctx, haResourceDeleteCall("vm:100"))
	if !errors.Is(err, errManagedAdmissionReadFailed) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("an unreadable owner: err = %v, want a retriable admission read failure", err)
	}
}

// TestManagedGuardRecordsOnlyAnIgnoredStagingOfItsOwnVM marks the resource
// staged only for a creation of the allocated VM's resource as ignored.
func TestManagedGuardRecordsOnlyAnIgnoredStagingOfItsOwnVM(t *testing.T) {
	for name, tc := range map[string]struct {
		call ManagedAllocationMutation
		want bool
	}{
		"ignored":       {stagedHAResourceCall("vm:100", "ignored"), true},
		"started":       {stagedHAResourceCall("vm:100", "started"), false},
		"another VM":    {stagedHAResourceCall("vm:101", "ignored"), false},
		"no parameters": {ManagedAllocationMutation{Service: "Cluster", Method: "CreateHaResources"}, false},
	} {
		m := &managedVMAllocation{vmid: 100}
		m.noteStagedHAResource(tc.call)
		if m.haResourceStaged != tc.want {
			t.Errorf("%s: staged %v, want %v", name, m.haResourceStaged, tc.want)
		}
	}
}

// TestManagedGuardStaysUsableAfterARefusedHAResourceRemoval runs a refusal
// through the guard's begin. The refusal reaches the caller as not attempted,
// and the guard admits the next write.
func TestManagedGuardStaysUsableAfterARefusedHAResourceRemoval(t *testing.T) {
	other := haIdentity{Owner: aaOtherDirector}.marker()
	m := &managedVMAllocation{vmid: 100, deps: Deps{PVE: &haRulesReadClient{
		svc: &haResourceReadCluster{comment: &other}}, RequestDirectorUUID: aaDirector}}
	m.noteStagedHAResource(stagedHAResourceCall("vm:100", "ignored"))
	calls := 0
	g := &ManagedAllocationGuard{hooks: ManagedAllocationHooks{
		Before: func(ctx context.Context, call ManagedAllocationMutation) (string, error) {
			calls++
			if call.Method == "DeleteHaResources" {
				return "", m.admitHAResourceDelete(ctx, call)
			}
			return "step-2", nil
		},
	}}
	_, err := g.begin(context.Background(), haResourceDeleteCall("vm:100"))
	if !errors.Is(err, errManagedHAResourceDeleteRefused) || !errors.Is(err, pve.ErrMutationNotAttempted) {
		t.Fatalf("err = %v, want a refusal reported as not attempted", err)
	}
	if g.Err() != nil {
		t.Fatalf("a refused removal poisoned the guard: %v", g.Err())
	}
	if token, err := g.begin(context.Background(), stagedHAResourceCall("vm:100", "ignored")); err != nil ||
		token != "step-2" {
		t.Fatalf("the next write after a refusal: token %q err %v", token, err)
	}
	if calls != 2 {
		t.Fatalf("admission ran %d times, want 2", calls)
	}
}

// TestManagedGuardSettlesARemovalPVEFoundNothingFor settles a removal that PVE
// answered with not-found once a fresh read proves the resource gone, and
// leaves every other failure uncertain.
func TestManagedGuardSettlesARemovalPVEFoundNothingFor(t *testing.T) {
	journal, intent := infrastructureJournal(t)
	handle := infrastructureHandle(t, journal, "ha-resource", intent)
	step, err := storageMutationIntent(handle, "vm.Cluster.DeleteHaResources", aj.Target{Node: "pve1", VMID: 123},
		nil)
	if err != nil {
		t.Fatal(err)
	}
	notFound := errors.New("no such resource 'vm:123'")
	ctx := context.Background()

	present := &haResourceReadCluster{}
	m := &managedVMAllocation{vmid: 123, handle: handle, deps: Deps{PVE: &haRulesReadClient{svc: present}}}
	if m.settleFailedWrite(ctx, haResourceDeleteCall("vm:123"), step, errors.New("HTTP 500 timeout")) {
		t.Fatal("a removal that failed for another reason was settled")
	}
	if m.settleFailedWrite(ctx, haResourceDeleteCall("vm:123"), step, notFound) {
		t.Fatal("a removal was settled while the resource still reads present")
	}
	present.gone = true
	if !m.settleFailedWrite(ctx, haResourceDeleteCall("vm:123"), step, notFound) {
		t.Fatal("a removal of a resource that reads gone was not settled")
	}
	for _, s := range handle.Record().Steps {
		if s.ID == step && s.State != aj.Observed {
			t.Fatalf("step state %v, want observed", s.State)
		}
	}
}
