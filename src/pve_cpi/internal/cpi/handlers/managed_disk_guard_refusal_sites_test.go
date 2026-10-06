package handlers

// These tests drive the lifecycle guard's admission straight through begin, one
// refusal site at a time, and check what each refusal does to the call, the
// guard, and the allocation. A read that fails before the mutation is sent
// refuses that one call as retriable and leaves the guard usable. A request
// that ends during a check that combines several reads gets the request's own
// refusal and leaves the guard usable too. An answer that only an operator can
// change, such as a VM configuration that doesn't exist, locks the guard as it
// did before failed reads were told apart from answers, and so does an
// identity conflict that the pre-delete check finds as the request ends.

import (
	"context"
	"encoding/json"
	"errors"
	"runtime/debug"
	"strings"
	"sync"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// guardSiteRead names one kind of read the lifecycle guard makes to admit a
// mutation.
type guardSiteRead string

const (
	guardSiteReadConfig      guardSiteRead = "config"
	guardSiteReadPending     guardSiteRead = "pending"
	guardSiteReadIdentity    guardSiteRead = "identity"
	guardSiteReadDefinitions guardSiteRead = "definitions"
	guardSiteReadStatuses    guardSiteRead = "statuses"
	guardSiteReadVolume      guardSiteRead = "volume"
	guardSiteReadListing     guardSiteRead = "listing"
	guardSiteReadMembers     guardSiteRead = "members"
)

// guardSiteFault fires once, on the first read of its kind whose call stack
// passes through every function named in within and through none named in
// outside. That pins the fault to one refusal site in the guard.
//
// Some reads run on goroutines of their own, such as the reads of the
// cluster's identity and the capacity check's storage statuses, so their
// stacks don't show the guard. For those, armRead and armWithin name a read
// the guard makes on its own goroutine just before the site, and the fault
// fires on the next read of its kind after that one.
type guardSiteFault struct {
	read    guardSiteRead
	within  []string
	outside []string
	// armRead, when set, keeps the fault from firing until a read of this
	// kind whose stack passes through every function in armWithin.
	armRead   guardSiteRead
	armWithin []string
	// act runs when the fault fires. It returns the error the read fails
	// with, or nil to let the read through.
	act func(ctx context.Context) error
	// unlisted makes a fired storage listing answer with no storages at all.
	unlisted bool
	// localMigration answers the storage listing the migration check reads
	// with local, unshared storages, so the check goes on to the source
	// volume and the capacity at the target.
	localMigration bool

	mu    sync.Mutex
	armed bool
	fired int
}

// stackPasses reports whether the current call stack passes through every
// function in within and through none in outside.
func stackPasses(within, outside []string) bool {
	if len(within) == 0 && len(outside) == 0 {
		return true
	}
	stack := string(debug.Stack())
	for _, name := range within {
		if !strings.Contains(stack, name) {
			return false
		}
	}
	for _, name := range outside {
		if strings.Contains(stack, name) {
			return false
		}
	}
	return true
}

func (f *guardSiteFault) hit(ctx context.Context, read guardSiteRead) (bool, error) {
	if f.armRead != "" && read == f.armRead && stackPasses(f.armWithin, nil) {
		f.mu.Lock()
		f.armed = true
		f.mu.Unlock()
	}
	if f.read != read || !stackPasses(f.within, f.outside) {
		return false, nil
	}
	f.mu.Lock()
	if f.fired > 0 || f.armRead != "" && !f.armed {
		f.mu.Unlock()
		return false, nil
	}
	f.fired++
	f.mu.Unlock()
	if f.act == nil {
		return true, nil
	}
	return true, f.act(ctx)
}

func (f *guardSiteFault) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fired
}

// guardSitePVE is the flow fixture's cluster with the reads guardSiteFault
// can answer badly.
type guardSitePVE struct {
	*lifecycleFlowPVE
	fault *guardSiteFault
}

func (c guardSitePVE) QEMU() qemu.Service {
	return guardSiteQEMU{Service: c.lifecycleFlowPVE.QEMU(), fault: c.fault}
}

func (c guardSitePVE) Nodes() nodes.Service {
	return guardSiteNodes{Service: c.lifecycleFlowPVE.Nodes(), fault: c.fault}
}

func (c guardSitePVE) Cluster() cluster.Service {
	return guardSiteCluster{Service: c.lifecycleFlowPVE.Cluster(), fault: c.fault}
}

type guardSiteCluster struct {
	cluster.Service
	fault *guardSiteFault
}

func (c guardSiteCluster) ListConfigNodes(ctx context.Context) (*cluster.ListConfigNodesResponse, error) {
	if _, err := c.fault.hit(ctx, guardSiteReadMembers); err != nil {
		return nil, err
	}
	return c.Service.ListConfigNodes(ctx)
}

func (c guardSitePVE) ClusterStorage() clusterstorage.Service {
	return guardSiteStorage{Service: c.lifecycleFlowPVE.ClusterStorage(), fault: c.fault}
}

type guardSiteQEMU struct {
	qemu.Service
	fault *guardSiteFault
}

func (q guardSiteQEMU) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if _, err := q.fault.hit(ctx, guardSiteReadConfig); err != nil {
		return nil, err
	}
	return q.Service.Config(ctx, node, vmid)
}

type guardSiteNodes struct {
	nodes.Service
	fault *guardSiteFault
}

func (n guardSiteNodes) ListQemuPending(ctx context.Context, node, vmid string) (*nodes.ListQemuPendingResponse, error) {
	if _, err := n.fault.hit(ctx, guardSiteReadPending); err != nil {
		return nil, err
	}
	return n.Service.ListQemuPending(ctx, node, vmid)
}

func (n guardSiteNodes) ListCertificatesInfo(ctx context.Context, node string) (*nodes.ListCertificatesInfoResponse, error) {
	if _, err := n.fault.hit(ctx, guardSiteReadIdentity); err != nil {
		return nil, err
	}
	return n.Service.ListCertificatesInfo(ctx, node)
}

func (n guardSiteNodes) ListStorage(ctx context.Context, node string, p *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
	if _, err := n.fault.hit(ctx, guardSiteReadStatuses); err != nil {
		return nil, err
	}
	return n.Service.ListStorage(ctx, node, p)
}

func (n guardSiteNodes) GetStorageContent(ctx context.Context, node, storage, volume string) (*nodes.GetStorageContentResponse, error) {
	if _, err := n.fault.hit(ctx, guardSiteReadVolume); err != nil {
		return nil, err
	}
	return n.Service.GetStorageContent(ctx, node, storage, volume)
}

func (n guardSiteNodes) ListStorageContent(ctx context.Context, node, storage string, p *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
	if _, err := n.fault.hit(ctx, guardSiteReadListing); err != nil {
		return nil, err
	}
	return n.Service.ListStorageContent(ctx, node, storage, p)
}

type guardSiteStorage struct {
	clusterstorage.Service
	fault *guardSiteFault
}

func (s guardSiteStorage) ListStorage(ctx context.Context, p *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
	fired, err := s.fault.hit(ctx, guardSiteReadDefinitions)
	if err != nil {
		return nil, err
	}
	if fired && s.fault.unlisted {
		empty := clusterstorage.ListStorageResponse{}
		return &empty, nil
	}
	if s.fault.localMigration {
		stack := string(debug.Stack())
		if strings.Contains(stack, "prepareMigration") && !strings.Contains(stack, "managedDiskLifecycleCapacity") {
			return lifecycleFlowDefinitions{c: &lifecycleFlowPVE{localStorage: true}}.ListStorage(ctx, p)
		}
	}
	return s.Service.ListStorage(ctx, p)
}

// guardSiteOutcome is what a refusal does to the call, the guard, and the
// allocation.
type guardSiteOutcome int

const (
	// guardSiteReadFailed is a failed admission read. The call is refused as
	// retriable and not sent, the guard stays usable, and the allocation is
	// returned as it was.
	guardSiteReadFailed guardSiteOutcome = iota
	// guardSiteEnded is a request that ended during the check. The call gets
	// the request's own retriable refusal, the guard stays usable, and the
	// allocation is returned as it was.
	guardSiteEnded
	// guardSiteLocked is an answer that locks the guard. The call is refused
	// for good, and the allocation is left for reconciliation.
	guardSiteLocked
)

// guardSiteFixture is one journal-managed disk on 777's scsi1, with its
// lifecycle open and its guard's reads routed through a guardSiteFault.
type guardSiteFixture struct {
	client    *lifecycleFlowPVE
	journal   *aj.Journal
	id        string
	volid     string
	storage   string
	key       string
	ctx       context.Context
	cancel    context.CancelFunc
	lifecycle *managedDiskLifecycle
	steps     int
}

func newGuardSiteFixture(t *testing.T, operation string) *guardSiteFixture {
	t.Helper()
	deps, client, journal, id, cid := lifecycleFlowFixtureState(t, true)
	client.state.configs[888] = map[string]any{"name": "receiver", "digest": "888"}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	bare, meta, err := decodeDiskCID(ctx, deps, operation, cid)
	if err != nil {
		t.Fatal(err)
	}
	rd, err := resolveDiskForOp(ctx, deps, operation, cid, bare, meta)
	if err != nil {
		t.Fatalf("the disk does not resolve: %v", err)
	}
	_, lifecycle, err := managedDiskOperation(ctx, deps, rd, operation)
	if err != nil || lifecycle == nil {
		t.Fatalf("the disk's lifecycle did not open (lifecycle %v): %v", lifecycle != nil, err)
	}
	storage, _, err := pve.ParseDiskCID(lifecycle.disk.volid)
	if err != nil {
		t.Fatal(err)
	}
	return &guardSiteFixture{
		client: client, journal: journal, id: id, volid: lifecycle.disk.volid, storage: storage, key: rd.sentinelKey(),
		ctx: ctx, cancel: cancel, lifecycle: lifecycle, steps: len(lifecycle.handle.Record().Steps),
	}
}

func guardSiteResize() ManagedAllocationMutation {
	return ManagedAllocationMutation{Service: managedServiceQEMU, Method: "ResizeDisk", Args: map[string]any{resourceTypeNode: "n1", metadataKeyVMID: 777, "diskID": "scsi1", "sizeGiB": 1}}
}

func guardSiteSlotDelete() ManagedAllocationMutation {
	slot := "scsi1"
	return ManagedAllocationMutation{Service: managedServiceNodes, Method: "UpdateQemuConfig", Args: map[string]any{resourceTypeNode: "n1", metadataKeyVMID: "777", managedArgumentParams: &nodes.UpdateQemuConfigParams{Delete: &slot}}}
}

func guardSiteMove() ManagedAllocationMutation {
	target, slot := int64(888), "scsi2"
	return ManagedAllocationMutation{Service: managedServiceNodes, Method: "CreateQemuMoveDisk", Args: map[string]any{resourceTypeNode: "n1", metadataKeyVMID: "777", managedArgumentParams: &nodes.CreateQemuMoveDiskParams{Disk: "scsi1", TargetVmid: &target, TargetDisk: &slot}}}
}

func guardSiteMigration() ManagedAllocationMutation {
	same := "1"
	return ManagedAllocationMutation{Service: managedServiceNodes, Method: "CreateQemuMigrate", Args: map[string]any{resourceTypeNode: "n1", metadataKeyVMID: "777", managedArgumentParams: &nodes.CreateQemuMigrateParams{Target: "n2", Targetstorage: &same}}}
}

func (s *guardSiteFixture) deletion() ManagedAllocationMutation {
	return ManagedAllocationMutation{Service: managedDiskServiceStorage, Method: "DeleteVolumeAsync", Args: map[string]any{resourceTypeNode: "n1", managedArgumentStorageName: s.storage, "volume": s.volid}}
}

// detach takes the disk off 777, so the pre-delete check finds no holder and
// goes on to read the volume's presence.
func (s *guardSiteFixture) detach() {
	delete(s.client.state.configs[777], "scsi1")
}

// conflict writes provenance for the disk onto 777 under another allocation,
// which the pre-delete check reads as a conflict with the record.
func (s *guardSiteFixture) conflict(t *testing.T) {
	t.Helper()
	other, err := aj.NewAllocationID()
	if err != nil {
		t.Fatal(err)
	}
	claim := pve.DiskAllocationProvenance{Version: 1, AllocationID: other, AllocationNamespace: lifecycleFlowNamespace, Node: "n1", Volid: s.volid}
	encoded, err := json.Marshal(map[string]pve.DiskAllocationProvenance{s.key: claim})
	if err != nil {
		t.Fatal(err)
	}
	notes, err := pve.RenderSentinel("", map[string]json.RawMessage{"bosh_disk_allocations": encoded})
	if err != nil {
		t.Fatal(err)
	}
	s.client.state.configs[777]["description"] = notes
}

// require checks what the guard did with call, then finishes the lifecycle
// and checks what became of the allocation.
func (s *guardSiteFixture) require(t *testing.T, call ManagedAllocationMutation, fault *guardSiteFault, want guardSiteOutcome) {
	t.Helper()
	label := call.Service + "." + call.Method
	token, err := s.lifecycle.guard.begin(s.ctx, call)
	if err == nil {
		t.Fatalf("%s was admitted (step %s) although its check was refused; the site's read was reached %d times", label, token, fault.count())
	}
	if n := fault.count(); n != 1 {
		t.Fatalf("the refusal site's read was reached %d times, want once; err = %v", n, err)
	}
	if !errors.Is(err, pve.ErrMutationNotAttempted) {
		t.Fatalf("err = %v, want the refusal to say %s was not attempted", err, label)
	}
	if n := len(s.lifecycle.handle.Record().Steps); n != s.steps {
		t.Fatalf("the refusal journaled %d steps, want none", n-s.steps)
	}
	retriable := cpierrors.IsType(err, cpierrors.TypeRetriableCloud)
	switch want {
	case guardSiteReadFailed:
		if !errors.Is(err, errManagedAdmissionReadFailed) || !retriable {
			t.Fatalf("err = %v, want a retriable failed admission read", err)
		}
		if !strings.Contains(err.Error(), "nothing was sent; retry the operation") {
			t.Fatalf("err = %v, want it to say nothing was sent and to retry", err)
		}
	case guardSiteEnded:
		if !errors.Is(err, errManagedRequestEnded) || !retriable {
			t.Fatalf("err = %v, want the ended request's retriable refusal", err)
		}
	case guardSiteLocked:
		if retriable || errors.Is(err, errManagedAdmissionReadFailed) || errors.Is(err, errManagedRequestEnded) {
			t.Fatalf("err = %v, want the permanent refusal of a locked guard", err)
		}
		if !strings.Contains(err.Error(), "managed allocation blocked before "+label) || strings.Contains(err.Error(), "retry") {
			t.Fatalf("err = %v, want the guard locked before %s with no retry advice", err, label)
		}
	}
	if locked := s.lifecycle.guard.Err(); (locked != nil) != (want == guardSiteLocked) {
		t.Fatalf("guard lock = %v after the refusal, want locked %v", locked, want == guardSiteLocked)
	}

	finished := s.lifecycle.finish(s.ctx, err, false)
	record, inspectErr := s.journal.Inspect(s.id)
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if want == guardSiteLocked {
		if finished == nil || isDiskReturnedUnchanged(finished) {
			t.Fatalf("finish = %v, want the locked guard's error and no return", finished)
		}
		if record.State != aj.ReconciliationRequired {
			t.Fatalf("allocation state = %s, want %s", record.State, aj.ReconciliationRequired)
		}
		return
	}
	if !isDiskReturnedUnchanged(finished) {
		t.Fatalf("finish = %v, want the allocation returned unchanged", finished)
	}
	assertReturnedRecord(t, label, record)
}

// errGuardSiteNotFound is PVE's answer that what the read named doesn't exist.
var errGuardSiteNotFound = &sdkerrors.APIError{HTTPCode: 404, Message: "does not exist"}

// errGuardSiteConfigGone is the 500 PVE answers when a VM's configuration file
// is gone from the node it reads.
var errGuardSiteConfigGone = &sdkerrors.APIError{HTTPCode: 500, Message: "Configuration file 'nodes/n1/qemu-server/777.conf' does not exist"}

// errGuardSiteListingDenied is PVE refusing a storage content listing. The
// presence read does not read a listing again after a refusal, so the fault's
// one firing is the whole failure. A server error would be read again and
// answered by the next read.
var errGuardSiteListingDenied = &sdkerrors.PermissionError{What: "Datastore.Audit"}

func guardSiteFails(err error) func(context.Context) error {
	return func(context.Context) error { return err }
}

// TestManagedGuardRefusalSites runs each refusal site in the guard's
// admission and checks the outcome its refusal leaves.
func TestManagedGuardRefusalSites(t *testing.T) {
	t.Parallel()
	type row struct {
		name      string
		operation string
		call      func(*guardSiteFixture) ManagedAllocationMutation
		fault     func(s *guardSiteFixture) *guardSiteFault
		setup     func(*testing.T, *guardSiteFixture)
		want      guardSiteOutcome
	}
	fixed := func(call ManagedAllocationMutation) func(*guardSiteFixture) ManagedAllocationMutation {
		return func(*guardSiteFixture) ManagedAllocationMutation { return call }
	}
	cancelling := func(s *guardSiteFixture) func(context.Context) error {
		return func(ctx context.Context) error {
			s.cancel()
			return ctx.Err()
		}
	}
	rows := []row{
		{
			name: "storage unlisted while checking the backing", operation: "resize_disk", call: fixed(guardSiteResize()),
			fault: func(*guardSiteFixture) *guardSiteFault {
				return &guardSiteFault{read: guardSiteReadDefinitions, within: []string{"observeContinuity"}, unlisted: true}
			},
			want: guardSiteLocked,
		},
		{
			name: "holder configuration answers 404", operation: "resize_disk", call: fixed(guardSiteResize()),
			fault: func(*guardSiteFixture) *guardSiteFault {
				return &guardSiteFault{read: guardSiteReadConfig, within: []string{"readHolderConfig"}, act: guardSiteFails(errGuardSiteNotFound)}
			},
			want: guardSiteLocked,
		},
		{
			name: "holder configuration file is gone", operation: "resize_disk", call: fixed(guardSiteResize()),
			fault: func(*guardSiteFixture) *guardSiteFault {
				return &guardSiteFault{read: guardSiteReadConfig, within: []string{"readHolderConfig"}, act: guardSiteFails(errGuardSiteConfigGone)}
			},
			want: guardSiteLocked,
		},
		{
			name: "pending view read fails", operation: "detach_disk", call: fixed(guardSiteSlotDelete()),
			fault: func(*guardSiteFixture) *guardSiteFault {
				return &guardSiteFault{read: guardSiteReadPending, within: []string{"addPendingDeletes"}, act: guardSiteFails(errAdmissionReadRefused)}
			},
			want: guardSiteReadFailed,
		},
		{
			name: "pending view answers 404", operation: "detach_disk", call: fixed(guardSiteSlotDelete()),
			fault: func(*guardSiteFixture) *guardSiteFault {
				return &guardSiteFault{read: guardSiteReadPending, within: []string{"addPendingDeletes"}, act: guardSiteFails(errGuardSiteNotFound)}
			},
			want: guardSiteLocked,
		},
		{
			name: "resize capacity check ends with the request", operation: "resize_disk", call: fixed(guardSiteResize()),
			fault: func(s *guardSiteFixture) *guardSiteFault {
				return &guardSiteFault{read: guardSiteReadStatuses, armRead: guardSiteReadConfig, armWithin: []string{"readHolderConfig"}, act: cancelling(s)}
			},
			want: guardSiteEnded,
		},
		{
			name: "resize capacity read fails", operation: "resize_disk", call: fixed(guardSiteResize()),
			fault: func(*guardSiteFixture) *guardSiteFault {
				return &guardSiteFault{read: guardSiteReadStatuses, armRead: guardSiteReadConfig, armWithin: []string{"readHolderConfig"}, act: guardSiteFails(errAdmissionReadRefused)}
			},
			want: guardSiteLocked,
		},
		{
			name: "move receiver configuration read fails", operation: "detach_disk", call: fixed(guardSiteMove()),
			fault: func(*guardSiteFixture) *guardSiteFault {
				return &guardSiteFault{read: guardSiteReadConfig, within: []string{"prepareMove"}, act: guardSiteFails(errAdmissionReadRefused)}
			},
			want: guardSiteReadFailed,
		},
		{
			name: "move receiver configuration answers 404", operation: "detach_disk", call: fixed(guardSiteMove()),
			fault: func(*guardSiteFixture) *guardSiteFault {
				return &guardSiteFault{read: guardSiteReadConfig, within: []string{"prepareMove"}, act: guardSiteFails(errGuardSiteNotFound)}
			},
			want: guardSiteLocked,
		},
		{
			name: "migration target identity read fails", operation: "attach_disk", call: fixed(guardSiteMigration()),
			fault: func(*guardSiteFixture) *guardSiteFault {
				return &guardSiteFault{read: guardSiteReadIdentity, armRead: guardSiteReadConfig, armWithin: []string{"readHolderConfig"}, act: guardSiteFails(errAdmissionReadRefused)}
			},
			want: guardSiteReadFailed,
		},
		{
			name: "migration storage definition read fails", operation: "attach_disk", call: fixed(guardSiteMigration()),
			fault: func(*guardSiteFixture) *guardSiteFault {
				return &guardSiteFault{read: guardSiteReadDefinitions, within: []string{"prepareMigration"}, outside: []string{"managedDiskLifecycleCapacity"}, act: guardSiteFails(errAdmissionReadRefused)}
			},
			want: guardSiteReadFailed,
		},
		{
			name: "migration storage unlisted", operation: "attach_disk", call: fixed(guardSiteMigration()),
			fault: func(*guardSiteFixture) *guardSiteFault {
				return &guardSiteFault{read: guardSiteReadDefinitions, within: []string{"prepareMigration"}, outside: []string{"managedDiskLifecycleCapacity"}, unlisted: true}
			},
			want: guardSiteLocked,
		},
		{
			name: "migration source volume read fails", operation: "attach_disk", call: fixed(guardSiteMigration()),
			fault: func(*guardSiteFixture) *guardSiteFault {
				return &guardSiteFault{read: guardSiteReadVolume, within: []string{"prepareMigration"}, localMigration: true, act: guardSiteFails(errAdmissionReadRefused)}
			},
			want: guardSiteReadFailed,
		},
		{
			name: "migration source volume answers 404", operation: "attach_disk", call: fixed(guardSiteMigration()),
			fault: func(*guardSiteFixture) *guardSiteFault {
				return &guardSiteFault{read: guardSiteReadVolume, within: []string{"prepareMigration"}, localMigration: true, act: guardSiteFails(errGuardSiteNotFound)}
			},
			want: guardSiteLocked,
		},
		{
			name: "migration capacity check ends with the request", operation: "attach_disk", call: fixed(guardSiteMigration()),
			fault: func(s *guardSiteFixture) *guardSiteFault {
				return &guardSiteFault{read: guardSiteReadStatuses, armRead: guardSiteReadVolume, armWithin: []string{"prepareMigration"}, localMigration: true, act: cancelling(s)}
			},
			want: guardSiteEnded,
		},
		{
			name: "migration capacity read fails", operation: "attach_disk", call: fixed(guardSiteMigration()),
			fault: func(*guardSiteFixture) *guardSiteFault {
				return &guardSiteFault{read: guardSiteReadStatuses, armRead: guardSiteReadVolume, armWithin: []string{"prepareMigration"}, localMigration: true, act: guardSiteFails(errAdmissionReadRefused)}
			},
			want: guardSiteLocked,
		},
		{
			name: "delete presence read fails", operation: "delete_disk", call: (*guardSiteFixture).deletion,
			fault: func(*guardSiteFixture) *guardSiteFault {
				return &guardSiteFault{read: guardSiteReadListing, within: []string{"prepareDeletion", "managedVolumePresent"}, outside: []string{"resolveDiskForOp"}, act: guardSiteFails(errGuardSiteListingDenied)}
			},
			setup: func(_ *testing.T, s *guardSiteFixture) { s.detach() },
			want:  guardSiteReadFailed,
		},
		{
			name: "delete identity check ends with the request", operation: "delete_disk", call: (*guardSiteFixture).deletion,
			fault: func(s *guardSiteFixture) *guardSiteFault {
				return &guardSiteFault{read: guardSiteReadMembers, within: []string{"prepareDeletion"}, act: cancelling(s)}
			},
			setup: func(_ *testing.T, s *guardSiteFixture) { s.detach() },
			want:  guardSiteEnded,
		},
		{
			name: "delete identity conflict found as the request ends", operation: "delete_disk", call: (*guardSiteFixture).deletion,
			fault: func(s *guardSiteFixture) *guardSiteFault {
				return &guardSiteFault{read: guardSiteReadConfig, within: []string{"prepareDeletion", "resolveManagedDiskRecord"}, act: func(context.Context) error {
					s.cancel()
					return nil
				}}
			},
			setup: func(t *testing.T, s *guardSiteFixture) { s.conflict(t) },
			want:  guardSiteLocked,
		},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newGuardSiteFixture(t, tc.operation)
			fault := tc.fault(s)
			s.lifecycle.deps.PVE = guardSitePVE{lifecycleFlowPVE: s.client, fault: fault}
			if tc.setup != nil {
				tc.setup(t, s)
			}
			s.require(t, tc.call(s), fault, tc.want)
		})
	}
}
