package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// These tests cover a delete_vm whose ephemeral retention runs out its parker
// lock wait, and every way the record it leaves is closed. Every probe runs on
// the fixture's storage a, which the fake defines as a shared NFS store, so the
// ephemeral volume has the file-backed name a:777/vm-777-ephemeral-0.raw. The
// fake can't model a block-backed store: its definitions are only nfs and dir
// (managedDiskTestDefinitions.ListStorage, lifecycleFlowDefinitions.ListStorage),
// and its reassignment always names the landed volume in the file form. The
// managed path finds the volume through the record's vm.ephemeral step, not by
// its name, so the name form doesn't change what these tests cover.

// retentionEphemeral is the ephemeral volume retainDeleteFixture gives VM 777.
const retentionEphemeral = "a:777/vm-777-ephemeral-0.raw"

// retentionExistingParker is the parker the existing-parker shape plants
// before the first call.
const retentionExistingParker = 90000

// destroySubmission is one VM destroy that reached the fake, with the volume
// drives the VM held at that moment.
type destroySubmission struct {
	vmid   string
	drives []string
}

// destroyWatch records every destroy submission. The flow fake refuses to
// destroy a VM that still holds a volume, but real PVE destroys every volume
// the VM owns that its config references, so the tests assert on the
// submission rather than on the fake's refusal.
type destroyWatch struct {
	mu          sync.Mutex
	submissions []destroySubmission
}

func (w *destroyWatch) record(vmid string, drives []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.submissions = append(w.submissions, destroySubmission{vmid: vmid, drives: drives})
}

// unsafe returns the submissions of VM 777 made while it still held a volume.
func (w *destroyWatch) unsafe() []destroySubmission {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []destroySubmission
	for _, s := range w.submissions {
		if s.vmid == "777" && len(s.drives) > 0 {
			out = append(out, s)
		}
	}
	return out
}

// guestDestroyed reports whether any destroy of VM 777 was submitted.
func (w *destroyWatch) guestDestroyed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.ContainsFunc(w.submissions, func(s destroySubmission) bool { return s.vmid == "777" })
}

// watchedDestroyPVE is the contended flow fake with its destroys recorded.
// configAnswer, when set, can fail a VM's config read before the fake
// answers it.
type watchedDestroyPVE struct {
	contendedFlowPVE
	watch        *destroyWatch
	configAnswer func(vmid int) error
}

func (c watchedDestroyPVE) QEMU() qemu.Service {
	return answeredConfigQEMU{Service: c.contendedFlowPVE.QEMU(), answer: c.configAnswer}
}

type answeredConfigQEMU struct {
	qemu.Service
	answer func(vmid int) error
}

func (q answeredConfigQEMU) Config(ctx context.Context, node string, vmid int) (map[string]any, error) {
	if q.answer != nil {
		if err := q.answer(vmid); err != nil {
			return nil, err
		}
	}
	return q.Service.Config(ctx, node, vmid)
}

func (c watchedDestroyPVE) Nodes() nodes.Service {
	return watchedDestroyNodes{Service: c.contendedFlowPVE.Nodes(), c: c}
}

type watchedDestroyNodes struct {
	nodes.Service
	c watchedDestroyPVE
}

func (n watchedDestroyNodes) ListQemuPending(ctx context.Context, node, vmid string) (*nodes.ListQemuPendingResponse, error) {
	return PendingFromConfigRead(ctx, n.c.QEMU().Config, node, vmid)
}

func (n watchedDestroyNodes) DeleteQemu(ctx context.Context, node, vmid string, params *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
	var drives []string
	var id int
	if _, err := fmt.Sscanf(vmid, "%d", &id); err == nil {
		for key, value := range n.c.state.configs[id] {
			if managedVMVolumeDevice(key) {
				drives = append(drives, fmt.Sprintf("%s=%v", key, value))
			}
		}
	}
	slices.Sort(drives)
	n.c.watch.record(vmid, drives)
	return n.Service.DeleteQemu(ctx, node, vmid, params)
}

// retentionCase is one delete_vm with a retained ephemeral disk, driven
// through the Director's dispatcher.
type retentionCase struct {
	deps       Deps
	client     *lifecycleFlowPVE
	journal    *aj.Journal
	id         string
	locks      *lockContention
	watch      *destroyWatch
	pve        *watchedDestroyPVE
	dispatcher *cpi.Dispatcher
	request    *jsonrpc.Request
	ctx        context.Context
}

// newRetentionCase builds the case. existingParker plants a parker of our
// prefix on n1 first, which is the shape a node that has parked before has.
func newRetentionCase(t *testing.T, existingParker bool) *retentionCase {
	t.Helper()
	deps, client, journal, id, _ := retainDeleteFixture(t)
	c := &retentionCase{client: client, journal: journal, id: id, locks: newLockContention(t), watch: &destroyWatch{}}
	gate := make(chan struct{})
	close(gate)
	c.pve = &watchedDestroyPVE{contendedFlowPVE: contendedFlowPVE{lifecycleFlowPVE: client, locks: c.locks, gate: gate}, watch: c.watch}
	deps.PVE = c.pve
	c.deps = deps
	if existingParker {
		plantRetentionParker(client, retentionExistingParker)
	}
	c.ctx = pve.WithTestBackoff(shortenManagedLockWait(t.Context(), testManagedLockWait), func(int) time.Duration { return 0 })
	c.dispatcher = cpi.NewDispatcher(log.NewNopLogger())
	if err := c.dispatcher.Register("delete_vm", HandleDeleteVM(deps)); err != nil {
		t.Fatal(err)
	}
	c.request = &jsonrpc.Request{Method: "delete_vm", Arguments: []json.RawMessage{planJSON(t, "777")}}
	return c
}

// plantRetentionParker gives n1 an empty parker of our prefix.
func plantRetentionParker(client *lifecycleFlowPVE, vmid int) {
	client.state.configs[vmid] = map[string]any{"name": fmt.Sprintf("bosh-parker-%d", vmid), "tags": "bosh-cpi;bosh-parker;vm-prefix--bosh", "protection": 1, "digest": "1"}
}

// holdEveryParkerLock gives another request a live claim on every parker in
// the band, so the retention runs out its wait wherever it parks.
func (c *retentionCase) holdEveryParkerLock() {
	start := c.deps.Config.ParkedDiskVMIDRangeStartValue()
	for vmid := start; vmid < start+1000; vmid++ {
		plantHeldParkerLock(c.locks, vmid)
	}
}

// replayV080 makes the sentinel create fail without an answer, which leaves
// the lifecycle's Pool.CreatePool step planned and the guard poisoned at the
// lock create, the record 0.8.0 left behind on any contended parker.
func (c *retentionCase) replayV080() {
	c.locks.createErr = errors.New("connection reset by peer")
}

func (c *retentionCase) deleteVM() *jsonrpc.ErrorBody {
	return c.dispatcher.Handle(c.ctx, c.request).Error
}

func (c *retentionCase) record(t *testing.T) aj.Record {
	t.Helper()
	record, err := c.journal.Inspect(c.id)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func (c *retentionCase) token() string {
	sum := sha256.Sum256([]byte("vm-ephemeral-retention\x00" + c.id))
	return "bpd-" + hex.EncodeToString(sum[:8])
}

func (c *retentionCase) cleanup(attested bool) (aj.Record, error) {
	deps, decision := c.deps, StorageAllocationDecision{Action: "cleanup", AllocationID: c.id, DecisionID: "retained-ephemeral-cleanup"}
	if attested {
		deps, decision = attestedCleanupDeps(c.deps), cleanupAttestedDecision(c.id)
	}
	return CleanupStorageAllocation(c.ctx, deps, c.journal, []string{"n1"}, decision)
}

// uncertain marks a record that timed out the way the CPI left it before
// this fix.
func (c *retentionCase) uncertain(t *testing.T) {
	t.Helper()
	handle, err := c.journal.Acquire(t.Context(), c.id)
	if err != nil {
		t.Fatal(err)
	}
	_ = storageAllocationUncertain(handle, "VM cleanup")
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	record := c.record(t)
	if record.State != aj.ReconciliationRequired || record.Reason != "outcome requires reconciliation at VM cleanup" {
		t.Fatalf("the rebuilt record is %s (%q), want the reconciliation record the CPI left before this fix", record.State, record.Reason)
	}
	for index := range record.Steps {
		step := &record.Steps[index]
		if step.State != aj.Observed {
			t.Fatalf("the rebuilt record has step %s (%s) %s", step.ID, step.Kind, step.State)
		}
	}
}

// createdParker returns the VMID of the parker the first attempt created, or
// zero when it created none.
func (c *retentionCase) createdParker(t *testing.T) int {
	t.Helper()
	record := c.record(t)
	for index := range record.Steps {
		if strings.HasSuffix(record.Steps[index].Kind, "_QEMU_Create") {
			return record.Steps[index].Target.VMID
		}
	}
	return 0
}

// assertReturnedTimeout checks that a call that ran out the parker lock wait
// gave the Director the retriable timeout and left the record observed.
func (c *retentionCase) assertReturnedTimeout(t *testing.T, label string, body *jsonrpc.ErrorBody) {
	t.Helper()
	if body == nil {
		t.Fatalf("%s: delete_vm succeeded behind held parker locks", label)
	}
	if body.Type != string(cpierrors.TypeCloud) || !body.OkToRetry || !strings.HasPrefix(body.Message, "detach_disk: transfer disk ") ||
		!strings.Contains(body.Message, `AcquireClusterLock: timed out after 150ms waiting for lock "bosh-lock-vm-`) || !strings.Contains(body.Message, "cluster lock acquire timed out") {
		t.Errorf("%s: the Director read %+v, want a retriable CloudError naming the parker lock timeout", label, body)
	}
	record := c.record(t)
	if record.State != aj.Observed || record.Reason != "" {
		t.Errorf("%s: the record is %s (%q), want observed", label, record.State, record.Reason)
	}
	for index := range record.Steps {
		step := &record.Steps[index]
		if step.State != aj.Observed {
			t.Errorf("%s: step %s (%s) is %s", label, step.ID, step.Kind, step.State)
		}
	}
}

// assertClosedRetained checks that the record closed by keeping the volume:
// vm_deleted_retained with one retained target that reads back on its parker
// under our token, the guest gone, and no destroy of the guest while it held
// a volume.
func (c *retentionCase) assertClosedRetained(t *testing.T, wantParker int) {
	t.Helper()
	c.assertNoUnsafeDestroy(t)
	record := c.record(t)
	if record.State != aj.VMDeletedRetained {
		t.Fatalf("the record is %s (%q), want vm_deleted_retained", record.State, record.Reason)
	}
	var evidence aj.VMRetentionEvidence
	if err := json.Unmarshal([]byte(record.Verifications[len(record.Verifications)-1].EvidenceJSON), &evidence); err != nil || len(evidence.RetainedArtifacts) != 1 {
		t.Fatalf("the closing proof retains %+v (%v), want exactly one target", evidence.RetainedArtifacts, err)
	}
	target := evidence.RetainedArtifacts[0]
	handle, err := c.journal.Acquire(t.Context(), c.id)
	if err != nil {
		t.Fatal(err)
	}
	_, resolveErr := resolveManagedRetainedEphemeral(t.Context(), c.deps, handle, target)
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if resolveErr != nil {
		t.Fatalf("the retained target doesn't read back: %v", resolveErr)
	}
	if wantParker > 0 && target.VMID != wantParker {
		t.Errorf("the volume landed in parker %d, want %d", target.VMID, wantParker)
	}
	if c.client.state.configs[777] != nil {
		t.Error("VM 777 is still present")
	}
	var holders []int
	for vmid, config := range c.client.state.configs {
		if managedVMConfigCarriesSerialForTest(config, c.token()) {
			holders = append(holders, vmid)
		}
	}
	if len(holders) != 1 || holders[0] != target.VMID {
		t.Errorf("guests carrying our token: %v, want only parker %d", holders, target.VMID)
	}
	if c.client.state.volumes[target.IntendedVolume] == nil {
		t.Errorf("the retained volume %s is gone", target.IntendedVolume)
	}
}

func (c *retentionCase) assertNoUnsafeDestroy(t *testing.T) {
	t.Helper()
	for _, s := range c.watch.unsafe() {
		t.Errorf("DeleteQemu vmid=%s submitted while the guest held %v", s.vmid, s.drives)
	}
}

// assertRefusedAndKept checks a refusal that must leave the guest and its
// ephemeral volume alone.
func (c *retentionCase) assertRefusedAndKept(t *testing.T) {
	t.Helper()
	if record := c.record(t); record.State != aj.ReconciliationRequired {
		t.Errorf("the record is %s, want reconciliation_required", record.State)
	}
	if c.client.state.configs[777] == nil || c.client.state.volumes[retentionEphemeral] == nil {
		t.Error("the refusal lost VM 777 or its ephemeral volume")
	}
	c.assertNoUnsafeDestroy(t)
	if c.watch.guestDestroyed() {
		t.Error("a destroy of VM 777 was submitted")
	}
}

func managedVMConfigCarriesSerialForTest(config map[string]any, serial string) bool {
	for key, value := range config {
		if !managedVMVolumeDevice(key) {
			continue
		}
		if drive, ok := value.(string); ok && slices.Contains(strings.Split(drive, ",")[1:], "serial="+serial) {
			return true
		}
	}
	return false
}

// assertDirectorRefused checks that the Director read a non-retriable
// CloudError with exactly message.
func assertDirectorRefused(t *testing.T, label string, body *jsonrpc.ErrorBody, message string) {
	t.Helper()
	if body == nil || body.Type != string(cpierrors.TypeCloud) || body.OkToRetry || body.Message != message {
		t.Errorf("%s: the Director read %+v, want a non-retriable CloudError %q", label, body, message)
	}
}

func reconciliationMessage(id string) string {
	return "allocation " + id + " requires reconciliation at VM cleanup; no alternate allocation was attempted"
}

func TestEphemeralRetentionTimeoutThenRerunClosesRetained(t *testing.T) {
	t.Run("existing parker", func(t *testing.T) {
		c := newRetentionCase(t, true)
		c.holdEveryParkerLock()
		c.assertReturnedTimeout(t, "first call", c.deleteVM())
		c.locks.reset()
		if body := c.deleteVM(); body != nil {
			c.assertNoUnsafeDestroy(t)
			t.Fatalf("the rerun failed: %+v", body)
		}
		c.assertClosedRetained(t, retentionExistingParker)
	})
	t.Run("fresh parker", func(t *testing.T) {
		c := newRetentionCase(t, false)
		c.holdEveryParkerLock()
		c.assertReturnedTimeout(t, "first call", c.deleteVM())
		created := c.createdParker(t)
		c.locks.reset()
		if body := c.deleteVM(); body != nil {
			c.assertNoUnsafeDestroy(t)
			t.Fatalf("the rerun failed: %+v", body)
		}
		c.assertClosedRetained(t, created)
	})
	// The rerun times out again, on a lifecycle whose serial step belongs to
	// the first call, and still goes back retriable with the record observed.
	t.Run("existing parker, repeated timeout", func(t *testing.T) {
		c := newRetentionCase(t, true)
		c.holdEveryParkerLock()
		c.assertReturnedTimeout(t, "first call", c.deleteVM())
		c.assertReturnedTimeout(t, "second call", c.deleteVM())
		c.locks.reset()
		if body := c.deleteVM(); body != nil {
			c.assertNoUnsafeDestroy(t)
			t.Fatalf("the third call failed: %+v", body)
		}
		c.assertClosedRetained(t, retentionExistingParker)
	})
}

// TestEphemeralRetentionTimeoutThenRerunLandsInAnotherParker fills the parker
// the first attempt created, so the resumed transfer lands in another one, and
// requires the created parker to hold nothing of ours.
func TestEphemeralRetentionTimeoutThenRerunLandsInAnotherParker(t *testing.T) {
	c := newRetentionCase(t, false)
	c.holdEveryParkerLock()
	c.assertReturnedTimeout(t, "first call", c.deleteVM())
	created := c.createdParker(t)
	other := retentionExistingParker
	if created == other {
		other = 90999
	}
	plantRetentionParker(c.client, other)
	for slot := 0; slot <= 30; slot++ {
		c.client.state.configs[created][fmt.Sprintf("scsi%d", slot)] = fmt.Sprintf("a:%d/vm-%d-filler-%d.raw", created, created, slot)
	}
	c.locks.reset()
	if body := c.deleteVM(); body != nil {
		c.assertNoUnsafeDestroy(t)
		t.Fatalf("the rerun failed: %+v", body)
	}
	c.assertClosedRetained(t, other)
	config := c.client.state.configs[created]
	if managedVMConfigCarriesSerialForTest(config, c.token()) {
		t.Error("the created parker carries our token")
	}
	_, sentinel := pve.ParseSentinel(pve.DescriptionFromConfig(config))
	var entries map[string]map[string]any
	if raw, ok := sentinel["bosh_parked_disks"]; ok {
		if err := json.Unmarshal(raw, &entries); err != nil {
			t.Fatal(err)
		}
	}
	if slot, _ := entries[c.token()]["slot"].(string); slot != "" {
		t.Errorf("the created parker's entry for our token names slot %q", slot)
	}
}

func TestEphemeralRetentionReconciliationRecordClosesThroughCleanup(t *testing.T) {
	for _, existing := range []bool{true, false} {
		for _, attested := range []bool{false, true} {
			t.Run(fmt.Sprintf("existing parker=%t attested=%t", existing, attested), func(t *testing.T) {
				c := newRetentionCase(t, existing)
				c.holdEveryParkerLock()
				_ = c.deleteVM()
				want := retentionExistingParker
				if !existing {
					want = c.createdParker(t)
				}
				c.uncertain(t)
				c.locks.reset()
				result, err := c.cleanup(attested)
				if err != nil {
					c.assertNoUnsafeDestroy(t)
					t.Fatalf("cleanup refused: %s", StorageAllocationDecisionFailure(err))
				}
				if result.State != aj.VMDeletedRetained {
					t.Errorf("cleanup returned %s, want vm_deleted_retained", result.State)
				}
				c.assertClosedRetained(t, want)
			})
		}
	}
}

// assertV080Replay checks that the replay left the record 0.8.0 left: in
// reconciliation_required with one planned lock create and every other step
// observed.
func (c *retentionCase) assertV080Replay(t *testing.T) {
	t.Helper()
	record := c.record(t)
	if record.State != aj.ReconciliationRequired {
		t.Fatalf("the replayed record is %s", record.State)
	}
	planned := 0
	for index := range record.Steps {
		step := &record.Steps[index]
		if step.State == aj.Observed {
			continue
		}
		if step.Kind != "lifecycle_delete_vm_retain_ephemeral_Pool_CreatePool" || step.State != aj.Planned || len(step.VolIDs) != 0 {
			t.Fatalf("the replayed record has step %s (%s) %s", step.ID, step.Kind, step.State)
		}
		planned++
	}
	if planned != 1 {
		t.Fatalf("the replayed record has %d planned lock steps, want 1", planned)
	}
}

// closeBy closes the record through one of the three paths.
func (c *retentionCase) closeBy(t *testing.T, path string) {
	t.Helper()
	if path == "rerun" {
		if body := c.deleteVM(); body != nil {
			c.assertNoUnsafeDestroy(t)
			t.Fatalf("the rerun failed: %+v", body)
		}
		return
	}
	if _, err := c.cleanup(path == "attested cleanup"); err != nil {
		c.assertNoUnsafeDestroy(t)
		t.Fatalf("cleanup refused: %s", StorageAllocationDecisionFailure(err))
	}
}

func TestEphemeralRetentionV080RecordCloses(t *testing.T) {
	for _, existing := range []bool{true, false} {
		for _, path := range []string{"rerun", "plain cleanup", "attested cleanup"} {
			t.Run(fmt.Sprintf("existing parker=%t %s", existing, path), func(t *testing.T) {
				c := newRetentionCase(t, existing)
				c.replayV080()
				assertDirectorRefused(t, "first call", c.deleteVM(), reconciliationMessage(c.id))
				c.assertV080Replay(t)
				want := retentionExistingParker
				if !existing {
					want = c.createdParker(t)
				}
				c.locks.reset()
				c.closeBy(t, path)
				c.assertClosedRetained(t, want)
				record := c.record(t)
				for index := range record.Steps {
					if record.Steps[index].State != aj.Observed {
						t.Errorf("step %s (%s) is still %s", record.Steps[index].ID, record.Steps[index].Kind, record.Steps[index].State)
					}
				}
			})
		}
	}
}

// TestCleanupStopsAtRetainedAfterAFinishedTransfer stops a clean delete_vm
// after its transfer and before its destroy writes an intent. The fake takes
// the guest's allocation marker away while the move runs, so the destroy's
// provenance check refuses, and the test puts it back before cleanup. That is
// the record a process that stopped between the transfer and the destroy
// leaves. Cleanup must close it as vm_deleted_retained and keep the volume.
func TestCleanupStopsAtRetainedAfterAFinishedTransfer(t *testing.T) {
	for _, attested := range []bool{false, true} {
		t.Run(fmt.Sprintf("attested=%t", attested), func(t *testing.T) {
			c := newRetentionCase(t, true)
			marker, _ := c.client.state.configs[777][pveConfigKeyDescription].(string)
			c.pve.enter = func() {
				description, _ := c.client.state.configs[777][pveConfigKeyDescription].(string)
				c.client.state.configs[777][pveConfigKeyDescription] = strings.Replace(description, strings.TrimSpace(marker), "", 1)
			}
			c.deps.PVE = c.pve
			if body := c.deleteVM(); body == nil {
				t.Fatal("delete_vm destroyed the VM without its marker")
			}
			c.pve.enter = nil
			c.deps.PVE = c.pve
			record := c.record(t)
			for _, step := range record.Steps {
				if step.State != aj.Observed || step.Kind == "vm.delete.destroy" {
					t.Fatalf("the stopped record has step %s (%s) %s", step.ID, step.Kind, step.State)
				}
			}
			description, _ := c.client.state.configs[777][pveConfigKeyDescription].(string)
			c.client.state.configs[777][pveConfigKeyDescription] = marker + description
			result, err := c.cleanup(attested)
			if err != nil {
				c.assertNoUnsafeDestroy(t)
				t.Fatalf("cleanup refused: %s", StorageAllocationDecisionFailure(err))
			}
			if result.State != aj.VMDeletedRetained {
				t.Errorf("cleanup returned %s, want vm_deleted_retained", result.State)
			}
			c.assertClosedRetained(t, retentionExistingParker)
		})
	}
}

// TestCleanupRetainsATaggedVMWhoseRetentionNeverStarted cleans a returned VM
// tagged to retain its ephemeral disk, with no delete_vm run first. Cleanup
// retains whenever delete_vm would have.
func TestCleanupRetainsATaggedVMWhoseRetentionNeverStarted(t *testing.T) {
	for _, attested := range []bool{false, true} {
		t.Run(fmt.Sprintf("attested=%t", attested), func(t *testing.T) {
			c := newRetentionCase(t, true)
			result, err := c.cleanup(attested)
			if err != nil {
				c.assertNoUnsafeDestroy(t)
				t.Fatalf("cleanup refused: %s", StorageAllocationDecisionFailure(err))
			}
			if result.State != aj.VMDeletedRetained {
				t.Errorf("cleanup returned %s, want vm_deleted_retained", result.State)
			}
			c.assertClosedRetained(t, retentionExistingParker)
		})
	}
}

// TestManagedVMDestroyRefusesAStableIDSerial gives the VM's owned ephemeral
// drive a stable-ID serial that isn't the retention's, with no retain tag, so
// nothing retains and the disposal reaches the destroy.
func TestManagedVMDestroyRefusesAStableIDSerial(t *testing.T) {
	const refusal = "VM destruction would take the disk on scsi1, which carries a stable-ID serial"
	for _, path := range []string{"delete_vm", "plain cleanup", "attested cleanup"} {
		t.Run(path, func(t *testing.T) {
			c := newRetentionCase(t, true)
			delete(c.client.state.configs[777], "tags")
			c.client.state.configs[777]["scsi1"] = retentionEphemeral + ",size=5G,serial=bpd-0123456789abcdef"
			if path == "delete_vm" {
				assertDirectorRefused(t, path, c.deleteVM(), "VM disposal failed: "+refusal)
			} else if _, err := c.cleanup(path == "attested cleanup"); StorageAllocationDecisionFailure(err) != "cleanup_resource_cleanup: "+refusal {
				t.Errorf("cleanup printed %q, want the destroy guard's refusal", StorageAllocationDecisionFailure(err))
			}
			c.assertRefusedAndKept(t)
		})
	}
}

// TestEphemeralRetentionResumeStillRefuses keeps every shape a resume must not
// accept refusing, from the observed record the retriable timeout leaves and
// from the reconciliation record a field deployment holds.
func TestEphemeralRetentionResumeStillRefuses(t *testing.T) {
	type refusalCase struct {
		name     string
		existing bool
		edit     func(t *testing.T, c *retentionCase)
		director string
		cli      string
	}
	foreignSerial := func(t *testing.T, c *retentionCase) {
		c.client.state.configs[777]["scsi1"] = retentionEphemeral + ",size=5G,serial=bpd-0000000000000000"
	}
	otherHolder := func(t *testing.T, c *retentionCase) {
		c.client.state.configs[777]["scsi1"] = retentionEphemeral + ",size=5G"
		other := "a:778/vm-778-disk-0.raw"
		c.client.state.volumes[other] = c.client.state.volumes[retentionEphemeral]
		c.client.state.configs[778] = map[string]any{"name": "other", "digest": "1", "scsi0": other + ",serial=" + c.token()}
	}
	differentVolume := func(t *testing.T, c *retentionCase) {
		created := c.createdParker(t)
		if created == 0 {
			t.Fatal("the fresh-parker shape recorded no parker")
		}
		other := "a:999/vm-999-disk-0.raw"
		c.client.state.volumes[other] = c.client.state.volumes[retentionEphemeral]
		entry, err := json.Marshal(map[string]any{c.token(): map[string]any{"disk_cid": "pvd-other", "volid": other, "node": "n1", "slot": "scsi0"}})
		if err != nil {
			t.Fatal(err)
		}
		description, err := pve.RenderSentinel("", map[string]json.RawMessage{"bosh_parked_disks": entry})
		if err != nil {
			t.Fatal(err)
		}
		c.client.state.configs[created]["scsi0"] = other
		c.client.state.configs[created][pveConfigKeyDescription] = description
	}
	// In the existing-parker shape a foreign serial reaches the retention's
	// own identity check, which refuses after the lifecycle opened, so the
	// Director reads the lifecycle's reconciliation. In the fresh-parker shape
	// the token no longer names a drive, so it resolves to the created
	// parker's stamp and the live-token check refuses first.
	cases := []refusalCase{
		{"foreign serial, existing parker", true, foreignSerial, "", ""},
		{"foreign serial, fresh parker", false, foreignSerial, "VM disposal failed: ephemeral retention token already identifies a live resource; audit recorded retention", ""},
		{"token on another holder, existing parker", true, otherHolder, "VM disposal failed: ephemeral retention token already identifies a live resource; audit recorded retention", ""},
		{"token on another holder, fresh parker", false, otherHolder, "VM disposal failed: ephemeral retention token already identifies a live resource; audit recorded retention", ""},
		{"parker names a different volume, fresh parker", false, differentVolume, "VM disposal failed: retained parker identity differs", "cleanup_resource_cleanup: retained parker identity differs"},
	}
	for _, start := range []string{"observed", "reconciliation"} {
		for _, rc := range cases {
			t.Run(start+"/"+rc.name, func(t *testing.T) {
				c := newRetentionCase(t, rc.existing)
				c.holdEveryParkerLock()
				_ = c.deleteVM()
				if start == "observed" {
					if record := c.record(t); record.State != aj.Observed {
						t.Fatalf("the timeout left the record %s, want observed", record.State)
					}
				} else {
					c.uncertain(t)
				}
				c.locks.reset()
				rc.edit(t, c)
				director := rc.director
				if director == "" {
					director = "allocation " + c.id + " requires reconciliation at lifecycle delete_vm_retain_ephemeral ephemeral retention incomplete; no alternate allocation was attempted"
				}
				assertDirectorRefused(t, "rerun", c.deleteVM(), director)
				if rc.cli != "" {
					if _, err := c.cleanup(false); StorageAllocationDecisionFailure(err) != rc.cli {
						t.Errorf("cleanup printed %q, want %q", StorageAllocationDecisionFailure(err), rc.cli)
					}
				}
				c.assertRefusedAndKept(t)
			})
		}
	}
}

// TestRetainedParkerIdentityRefusalNamesItselfInTheCLI closes a clean
// retention, then rewrites its parker's entry for our token so it names
// another volume. Retained cleanup must name the refusal in the CLI.
func TestRetainedParkerIdentityRefusalNamesItselfInTheCLI(t *testing.T) {
	const want = "cleanup_resource_cleanup: retained parker identity differs"
	for _, attested := range []bool{false, true} {
		t.Run(fmt.Sprintf("attested=%t", attested), func(t *testing.T) {
			c := newRetentionCase(t, true)
			if body := c.deleteVM(); body != nil {
				t.Fatalf("the clean delete failed: %+v", body)
			}
			if record := c.record(t); record.State != aj.VMDeletedRetained {
				t.Fatalf("the clean delete left %s", record.State)
			}
			config := c.client.state.configs[retentionExistingParker]
			nonBOSH, sentinel := pve.ParseSentinel(pve.DescriptionFromConfig(config))
			var entries map[string]map[string]any
			if err := json.Unmarshal(sentinel["bosh_parked_disks"], &entries); err != nil {
				t.Fatal(err)
			}
			entries[c.token()]["volid"] = "a:999/vm-999-disk-0.raw"
			encoded, err := json.Marshal(entries)
			if err != nil {
				t.Fatal(err)
			}
			sentinel["bosh_parked_disks"] = encoded
			description, err := pve.RenderSentinel(nonBOSH, sentinel)
			if err != nil {
				t.Fatal(err)
			}
			config[pveConfigKeyDescription] = description
			_, err = c.cleanup(attested)
			if err == nil {
				t.Fatal("cleanup accepted a parker whose entry names another volume")
			}
			if got := StorageAllocationDecisionFailure(err); got != want {
				t.Errorf("CLI text = %q, want %q", got, want)
			}
		})
	}
}

// TestEphemeralRetentionRequestEndedAfterSlotDeleteNeedsReconciliation ends
// the request inside the parker window, after the slot delete is journaled.
// That changed the disk, so the record must require reconciliation and the
// Director must not retry. It is a control: it holds before and after.
func TestEphemeralRetentionRequestEndedAfterSlotDeleteNeedsReconciliation(t *testing.T) {
	c := newRetentionCase(t, true)
	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	var once sync.Once
	c.pve.onConfig = func(_ context.Context, vmid int) {
		if config := c.client.state.configs[777]; vmid == 777 && config != nil && config["scsi1"] == nil {
			once.Do(cancel)
		}
	}
	c.deps.PVE = c.pve
	body := c.dispatcher.Handle(ctx, c.request).Error
	if body == nil || body.OkToRetry {
		t.Fatalf("the Director read %+v, want a non-retriable error", body)
	}
	if record := c.record(t); record.State != aj.ReconciliationRequired {
		t.Errorf("the record is %s, want reconciliation_required", record.State)
	}
	if c.watch.guestDestroyed() {
		t.Error("a destroy of VM 777 was submitted")
	}
}

// TestCleanupThatWaitsOutTheParkerLockCanRunAgain runs cleanup on the
// reconciliation record with every parker lock still held. Cleanup now runs
// the transfer, so it waits out the lock too. It must leave the record
// observed, submit no destroy, and name the wait in the CLI, and then a second
// cleanup or the Director's delete_vm with the locks free must close it.
func TestCleanupThatWaitsOutTheParkerLockCanRunAgain(t *testing.T) {
	const want = "cleanup_resource_cleanup: a parker lock wait ran out before the disk moved, so nothing was destroyed; run cleanup again once the lock is free"
	for _, then := range []string{"plain cleanup", "attested cleanup", "delete_vm"} {
		t.Run(then, func(t *testing.T) {
			c := newRetentionCase(t, true)
			c.holdEveryParkerLock()
			_ = c.deleteVM()
			c.uncertain(t)
			_, err := c.cleanup(then == "attested cleanup")
			c.assertNoUnsafeDestroy(t)
			if got := StorageAllocationDecisionFailure(err); err == nil || got != want {
				t.Fatalf("the timed-out cleanup printed %q (err=%v), want %q", got, err, want)
			}
			if record := c.record(t); record.State != aj.Observed || record.Reason != "" {
				t.Errorf("the timed-out cleanup left the record %s (%q), want observed", record.State, record.Reason)
			}
			if c.watch.guestDestroyed() {
				t.Error("the timed-out cleanup submitted a destroy of VM 777")
			}
			c.locks.reset()
			if then == "delete_vm" {
				if body := c.deleteVM(); body != nil {
					t.Fatalf("delete_vm after the timed-out cleanup failed: %+v", body)
				}
			} else if result, err := c.cleanup(then == "attested cleanup"); err != nil || result.State != aj.VMDeletedRetained {
				t.Fatalf("the second cleanup returned %s: %s", result.State, StorageAllocationDecisionFailure(err))
			}
			c.assertClosedRetained(t, retentionExistingParker)
		})
	}
}

// pmxcfsConfigMissing is the 500 PVE answers for a config read of a VM whose
// config file is gone.
func pmxcfsConfigMissing(vmid int) error {
	body := fmt.Sprintf(`{"data":null,"message":"Configuration file 'nodes/n1/qemu-server/%d.conf' does not exist\n"}`, vmid)
	return fmt.Errorf("API request failed: %w", sdkerrors.ParseAPIError(500, []byte(body)))
}

// TestEphemeralRetentionRecordedParkerReadback reads the parker the first
// attempt created back in every way it can come back. Gone counts as holding
// nothing of ours, and the retention resumes. Any other read error refuses.
func TestEphemeralRetentionRecordedParkerReadback(t *testing.T) {
	const refusal = "VM disposal failed: a retention parker the record names could not be read"
	for _, row := range []struct {
		name   string
		answer func(parker int) error
		delete bool
		closes bool
	}{
		{name: "deleted, not found", delete: true, closes: true},
		{name: "config file missing", answer: pmxcfsConfigMissing, closes: true},
		{name: "permission refused", answer: func(parker int) error {
			body := fmt.Sprintf(`{"data":null,"message":"Permission check failed (/vms/%d, VM.Audit)\n"}`, parker)
			return fmt.Errorf("API request failed: %w", sdkerrors.ParseAPIError(403, []byte(body)))
		}},
		{name: "transport error", answer: func(int) error { return errors.New("connection reset by peer") }},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := newRetentionCase(t, false)
			c.holdEveryParkerLock()
			c.assertReturnedTimeout(t, "first call", c.deleteVM())
			parker := c.createdParker(t)
			if parker == 0 {
				t.Fatal("the fresh-parker shape recorded no parker")
			}
			c.locks.reset()
			if row.delete {
				delete(c.client.state.configs, parker)
			}
			if row.answer != nil {
				// The answer replaces only the first read of the parker after
				// the rerun's admission, which is the readback, so the
				// admission audit before it and the reads after it see the
				// parker as it is.
				baseline := len(c.record(t).Verifications)
				var mu sync.Mutex
				answered := false
				c.pve.configAnswer = func(vmid int) error {
					// delete_vm reads every parker for transfers off the VM
					// before it preserves its disks, and that read has its
					// own refusal, so the answer is kept for the readback.
					if vmid != parker || strings.Contains(string(debug.Stack()), "managedSourceTransferRecords") {
						return nil
					}
					record, err := c.journal.Inspect(c.id)
					mu.Lock()
					defer mu.Unlock()
					if err != nil || answered || len(record.Verifications) <= baseline {
						return nil
					}
					answered = true
					return row.answer(parker)
				}
			}
			body := c.deleteVM()
			if row.closes {
				if body != nil {
					c.assertNoUnsafeDestroy(t)
					t.Fatalf("the rerun failed: %+v", body)
				}
				c.assertClosedRetained(t, 0)
				return
			}
			assertDirectorRefused(t, "rerun", body, refusal)
			c.assertRefusedAndKept(t)
		})
	}
}

// movedParkerCase finishes a retention's transfer into the parker it created,
// stops before the destroy, and then moves that parker to n2. It builds the
// state with a clean delete_vm whose allocation marker the fake takes away
// while the move runs, so the same setup works before and after the fix. It
// returns the volume the parker holds.
func movedParkerCase(t *testing.T) (*retentionCase, string) {
	t.Helper()
	c := newRetentionCase(t, false)
	marker, _ := c.client.state.configs[777][pveConfigKeyDescription].(string)
	c.pve.enter = func() {
		description, _ := c.client.state.configs[777][pveConfigKeyDescription].(string)
		c.client.state.configs[777][pveConfigKeyDescription] = strings.Replace(description, strings.TrimSpace(marker), "", 1)
	}
	if body := c.deleteVM(); body == nil {
		t.Fatal("delete_vm destroyed the VM without its marker")
	}
	c.pve.enter = nil
	description, _ := c.client.state.configs[777][pveConfigKeyDescription].(string)
	c.client.state.configs[777][pveConfigKeyDescription] = marker + description
	parker := c.createdParker(t)
	if parker == 0 {
		t.Fatal("the delete created no parker")
	}
	landed := ""
	for key, value := range c.client.state.configs[parker] {
		if drive, ok := value.(string); ok && managedVMVolumeDevice(key) && strings.Contains(drive, "serial="+c.token()) {
			landed = strings.Split(drive, ",")[0]
		}
	}
	if landed == "" {
		t.Fatal("the transfer didn't land in the created parker")
	}
	if c.client.vmNodes == nil {
		c.client.vmNodes = map[int]string{}
	}
	c.client.vmNodes[parker] = "n2"
	return c, landed
}

// TestEphemeralRetentionRefusesWhenItsParkerMoved cleans the record after its
// parker moved to n2 with the retained volume. The read on n1 comes back gone,
// but the volume is on the moved parker. The control subtest holds before and
// after the fix: cleanup refuses, the record stays in reconciliation_required,
// the volume stays on n2, and no destroy goes out. The refusal subtest needs
// the fix, because cleanup now names the live token where it said
// "unclassified error".
func TestEphemeralRetentionRefusesWhenItsParkerMoved(t *testing.T) {
	t.Run("control", func(t *testing.T) {
		c, landed := movedParkerCase(t)
		if _, err := c.cleanup(false); err == nil {
			t.Fatal("cleanup accepted a record whose parker moved away with its volume")
		}
		if record := c.record(t); record.State != aj.ReconciliationRequired {
			t.Errorf("the record is %s, want reconciliation_required", record.State)
		}
		if c.client.state.volumes[landed] == nil {
			t.Errorf("the retained volume %s is gone", landed)
		}
		if c.watch.guestDestroyed() {
			t.Error("a destroy of VM 777 was submitted")
		}
	})
	t.Run("refusal", func(t *testing.T) {
		c, _ := movedParkerCase(t)
		_, err := c.cleanup(false)
		if got, want := StorageAllocationDecisionFailure(err), "cleanup_resource_cleanup: ephemeral retention token already identifies a live resource; audit recorded retention"; err == nil || got != want {
			t.Errorf("cleanup printed %q, want %q", got, want)
		}
	})
}
