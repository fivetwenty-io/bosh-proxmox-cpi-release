package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	ns "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// vmPoolLockGroup is an instance group whose name sanitizing changes, so a
// sentinel rebuilt from the unsanitized name would miss the real one.
const vmPoolLockGroup = "web_worker.v2"

// vmPoolLockSentinel is the anti-affinity sentinel create_vm takes for
// vmPoolLockGroup.
var vmPoolLockSentinel = pve.ClusterLockPoolName(antiAffinityLockPrefix + sanitizeTagValue(vmPoolLockGroup))

// sentinelReadLog records every raw sentinel read in order.
type sentinelReadLog struct {
	mu    sync.Mutex
	reads []string
}

func (l *sentinelReadLog) names() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.reads)
}

// loggedPools serves the shared sentinel store and logs each raw read.
type loggedPools struct {
	contendedPools
	log *sentinelReadLog
}

func (p loggedPools) ReadPoolComment(ctx context.Context, id string) (string, error) {
	p.log.mu.Lock()
	p.log.reads = append(p.log.reads, id)
	p.log.mu.Unlock()
	return p.contendedPools.ReadPoolComment(ctx, id)
}

// vmPoolLockClient is the VM delete fixture with a shared sentinel store.
type vmPoolLockClient struct {
	*deleteManagedClient
	locks *lockContention
	log   *sentinelReadLog
}

func (c *vmPoolLockClient) Pools() pve.PoolService {
	return loggedPools{contendedPools: contendedPools{locks: c.locks}, log: c.log}
}

// vmPoolStepRecord is a create_vm record for VM 123 that stopped at the pool
// steps that steps writes. When live is set, the VM landed with its root and
// ephemeral disks first and still exists. execution is the VM execution the
// record's plan froze. The record is left the way the VM guard leaves one after
// a pool call it could not classify, in reconciliation_required with no CID.
func vmPoolStepRecord(t *testing.T, execution *StorageVMExecution, live bool, steps func(*aj.Handle)) (Deps, *aj.Journal, *vmPoolLockClient, aj.Record) {
	t.Helper()
	deps, j, base := auditFixture(t)
	resume := &resumeVMClient{allocationAuditClient: base, volumes: &resumeVMNodes{Service: base.Nodes()}}
	client := &vmPoolLockClient{deleteManagedClient: &deleteManagedClient{resumeVMClient: resume}, locks: newLockContention(t), log: &sentinelReadLog{}}
	deps.PVE = client
	def, err := pve.ParseStorageEntry(base.storageRead.definitions[0])
	if err != nil {
		t.Fatal(err)
	}
	plan := StorageAllocationPlan{Version: 1, Namespace: "director", AllocationKey: "agent", PolicyFingerprint: strings.Repeat("a", 64), Node: "pve1", Definitions: map[string]pve.StorageInfo{"a": def}, Targets: []StoragePlanTarget{{Role: "root", Node: "pve1", StorageID: "a", BackingKey: def.BackingKey(), VirtualBytes: 1 << 30}, {Role: "ephemeral", Node: "pve1", StorageID: "a", BackingKey: def.BackingKey(), VirtualBytes: 1 << 30}}, VMExecution: execution}
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := storageCallerIntentFingerprint("create_vm", []json.RawMessage{json.RawMessage(`"agent"`)})
	if err != nil {
		t.Fatal(err)
	}
	h, err := j.AcquireVM(t.Context(), "agent", aj.Intent{IntentFingerprint: fp, PolicyFingerprint: plan.PolicyFingerprint, PlanVersion: 1, Plan: payload})
	if err != nil {
		t.Fatal(err)
	}
	if live {
		for _, binding := range []struct{ kind, volume string }{{"vm.root.virtio0", "a:123/vm-123-disk-0.qcow2"}, {"vm.ephemeral.scsi1", "a:123/vm-123-disk-1.qcow2"}} {
			id, err := storageMutationIntent(h, binding.kind, aj.Target{Node: "pve1", VMID: 123, Storage: "a", Backing: def.BackingKey()}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = storageMutationObserved(h, id, []string{binding.volume}, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	steps(h)
	if err := storageAllocationUncertain(h, "VM Pool call"); err == nil {
		t.Fatal("marking the record uncertain returned no refusal")
	}
	record := h.Record()
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if live {
		hash := sha256.Sum256([]byte("agent"))
		marker, err := pve.FormatStorageAllocationMarker(pve.StorageAllocationMarker{Version: 1, Namespace: "director", Kind: "vm", AllocationID: record.ID, AgentSHA256: hex.EncodeToString(hash[:])})
		if err != nil {
			t.Fatal(err)
		}
		resume.configs[123] = map[string]any{"description": marker, "virtio0": "a:123/vm-123-disk-0.qcow2", "scsi1": "a:123/vm-123-disk-1.qcow2"}
		resume.nodesRead.content = ns.ListStorageContentResponse{json.RawMessage(`{"volid":"a:123/vm-123-disk-0.qcow2"}`), json.RawMessage(`{"volid":"a:123/vm-123-disk-1.qcow2"}`)}
	}
	return deps, j, client, record
}

// vmPoolStep writes a VM pool step in the shape the VM guard writes one.
func vmPoolStep(t *testing.T, h *aj.Handle, method string, observed bool) string {
	t.Helper()
	id, err := storageMutationIntent(h, "vm.Pool."+method, aj.Target{Node: "pve1", VMID: 123}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if observed {
		if err := storageMutationObserved(h, id, nil, false); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func groupExecution(pool, group string) *StorageVMExecution {
	return &StorageVMExecution{Version: 1, Node: "pve1", Pool: pool, PoolInstanceGroup: group}
}

// cleanupVMPoolRecord runs the operator's cleanup and checks that it settled
// the planned pool step and closed the record, destroying the VM once when it
// was still there.
func cleanupVMPoolRecord(t *testing.T, deps Deps, j *aj.Journal, client *vmPoolLockClient, record aj.Record, planned string, live bool) {
	t.Helper()
	result, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, StorageAllocationDecision{Action: "cleanup", AllocationID: record.ID, DecisionID: "vm-pool-lock"})
	if err != nil {
		t.Fatalf("cleanup refused the record: %v", err)
	}
	if result.State != aj.Cleaned {
		t.Fatalf("cleanup left the record %s", result.State)
	}
	if step := stepByID(t, result, planned); step.State != aj.Observed || len(step.VolIDs) != 0 {
		t.Fatalf("cleanup did not settle the pool step the way the guard settles one: %+v", step)
	}
	want := map[bool]int{false: 0, true: 1}[live]
	if client.destroyCount != want {
		t.Fatalf("cleanup destroyed the VM %d times, want %d", client.destroyCount, want)
	}
}

// TestVMPoolLockReleaseFailureSettlesAndCleansUp covers an anti-affinity lock
// whose release lost its answer. The VM guard leaves the release's DeletePool
// planned and the record in reconciliation_required. Cleanup settles that step
// on an exact read of the sentinel rebuilt from the record's group, which still
// holds the dead request's claim, and then closes the record. The claim is left
// for its TTL.
func TestVMPoolLockReleaseFailureSettlesAndCleansUp(t *testing.T) {
	m, fixture, _, _ := newManagedVMGuardCase(t, managedVMGuardCase{})
	locks := newLockContention(t)
	m.deps.PVE = lockedVMClient{managedVMGuardFixture: fixture, locks: locks}
	m.guard = nil
	if err := m.newGuard(); err != nil {
		t.Fatal(err)
	}
	guarded := m.deps
	guarded.PVE = m.guard.Client()
	handle, err := acquireAntiAffinityLock(t.Context(), guarded, "web", m.vmid)
	if err != nil {
		t.Fatal(err)
	}
	locks.deleteErr = errors.New("connection reset by peer")
	if err := handle.Release(t.Context()); err == nil {
		t.Fatal("the lost release answer did not fail the release")
	}
	guardRecord := m.handle.Record()
	var release []aj.Step
	for i := range guardRecord.Steps {
		if guardRecord.Steps[i].State != aj.Observed {
			release = append(release, guardRecord.Steps[i])
		}
	}
	if guardRecord.State != aj.ReconciliationRequired || len(release) != 1 || release[0].Kind != "vm.Pool.DeletePool" {
		t.Fatalf("the failed release left %s with %+v", guardRecord.State, release)
	}
	if !isLockStep(release[0]) {
		t.Fatalf("the VM guard's planned release is not settled as a lock step: %+v", release[0])
	}

	deps, j, client, record := vmPoolStepRecord(t, groupExecution(ensureTestPool, vmPoolLockGroup), true, func(h *aj.Handle) {
		vmPoolStep(t, h, "CreatePool", true)
		vmPoolStep(t, h, "DeletePool", false)
	})
	planned := record.Steps[len(record.Steps)-1].ID
	client.locks.pools = map[string]string{vmPoolLockSentinel: "owner=dead-request exp=9999999999"}
	cleanupVMPoolRecord(t, deps, j, client, record, planned, true)
	if reads := client.log.names(); !slices.Contains(reads, vmPoolLockSentinel) {
		t.Fatalf("settlement did not read the rebuilt sentinel %s: %v", vmPoolLockSentinel, reads)
	}
	client.locks.mu.Lock()
	defer client.locks.mu.Unlock()
	if _, held := client.locks.pools[vmPoolLockSentinel]; !held {
		t.Fatalf("settlement deleted the dead request's claim: %v", client.locks.pools)
	}
}

// TestVMPoolSentinelCreateFailureSettlesAndCleansUp covers the anti-affinity
// sentinel create that lost its answer after the VM landed. The record holds
// observed steps and one planned CreatePool, which could have been the
// deployment pool's ensure or the sentinel. Cleanup reads both the rebuilt
// sentinel and the VMID probe, settles the step, and closes the record.
func TestVMPoolSentinelCreateFailureSettlesAndCleansUp(t *testing.T) {
	var planned string
	deps, j, client, record := vmPoolStepRecord(t, groupExecution(ensureTestPool, vmPoolLockGroup), true, func(h *aj.Handle) {
		planned = vmPoolStep(t, h, "CreatePool", false)
	})
	cleanupVMPoolRecord(t, deps, j, client, record, planned, true)
	reads := client.log.names()
	slices.Sort(reads)
	if want := []string{vmPoolLockSentinel, "bosh-lock-vm-123"}; !slices.Equal(reads, want) {
		t.Fatalf("settlement read %v, want %v", reads, want)
	}
}

// TestVMPoolStepWithoutAPoolSettlesOnTheVMIDProbe covers a VM that landed
// under a record that resolved no deployment pool, so the record froze no
// group either. Its planned
// DeletePool can only have meant the sentinel of a group the record never
// stored, and the only read settlement can make is the VMID probe, which shows
// that PVE is answering exactly.
func TestVMPoolStepWithoutAPoolSettlesOnTheVMIDProbe(t *testing.T) {
	var planned string
	deps, j, client, record := vmPoolStepRecord(t, groupExecution("", ""), true, func(h *aj.Handle) {
		vmPoolStep(t, h, "CreatePool", true)
		planned = vmPoolStep(t, h, "DeletePool", false)
	})
	cleanupVMPoolRecord(t, deps, j, client, record, planned, true)
	if reads := client.log.names(); !slices.Equal(reads, []string{"bosh-lock-vm-123"}) {
		t.Fatalf("settlement read %v, want only the VMID probe", reads)
	}
}

// TestVMPoolStepCarryingMoreStaysPlanned keeps VM pool steps that carry more
// than the VM guard records out of the rule. Each record's VM has landed, so
// only the step's own shape decides. The bare step settles, and the same step
// with a volume or with parameters stays planned.
func TestVMPoolStepCarryingMoreStaysPlanned(t *testing.T) {
	for _, shape := range []string{"bare", "volume", "parameters"} {
		t.Run(shape, func(t *testing.T) {
			var planned string
			_, j, client, record := vmPoolStepRecord(t, groupExecution(ensureTestPool, vmPoolLockGroup), true, func(h *aj.Handle) {
				target := aj.Target{Node: "pve1", VMID: 123}
				var err error
				switch shape {
				case "bare":
					planned, err = storageMutationIntent(h, "vm.Pool.CreatePool", target, nil)
				case "volume":
					planned, err = storageMutationIntent(h, "vm.Pool.CreatePool", target, nil)
					if err == nil {
						record := h.Record()
						record.Steps[len(record.Steps)-1].VolIDs = []string{"a:123/vm-123-disk-0.qcow2"}
						err = h.Save(record)
					}
				case "parameters":
					planned, err = storageMutationIntent(h, "vm.Pool.CreatePool", target, nil, json.RawMessage(`{"version":1,"kind":"unknown"}`))
				}
				if err != nil {
					t.Fatal(err)
				}
			})
			handle, err := j.Acquire(t.Context(), record.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := handle.Close(); err != nil {
					t.Error(err)
				}
			}()
			if _, err := settlePlannedLockSteps(t.Context(), Deps{PVE: client}, handle); err != nil {
				t.Fatal(err)
			}
			want := map[string]aj.State{"bare": aj.Observed, "volume": aj.Planned, "parameters": aj.Planned}[shape]
			if step := stepByID(t, handle.Record(), planned); step.State != want {
				t.Fatalf("the %s pool step is %s after settlement, want %s", shape, step.State, want)
			}
		})
	}
}

// TestVMPoolSentinelNameMatchesTheAntiAffinityLock derives the sentinel both
// ways from one env. create_vm names the lock after the sanitized instance
// group from the env, and it freezes the unsanitized group in the plan. The
// name settlement rebuilds from the record must be the pool the lock creates.
func TestVMPoolSentinelNameMatchesTheAntiAffinityLock(t *testing.T) {
	env := map[string]any{"bosh": map[string]any{"group": "dir-dep-" + vmPoolLockGroup, "groups": []any{"dir", "dep", "dir-dep", vmPoolLockGroup, "dir-dep-" + vmPoolLockGroup}}}
	deps, _, base := auditFixture(t)
	locks := newLockContention(t)
	deps.PVE = &vmPoolLockClient{deleteManagedClient: &deleteManagedClient{resumeVMClient: &resumeVMClient{allocationAuditClient: base}}, locks: locks, log: &sentinelReadLog{}}
	handle, err := acquireAntiAffinityLock(t.Context(), deps, sanitizeTagValue(instanceGroupName(env)), 123)
	if err != nil {
		t.Fatal(err)
	}
	locks.mu.Lock()
	taken := slices.Collect(maps.Keys(locks.pools))
	locks.mu.Unlock()
	if err := handle.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(taken) != 1 {
		t.Fatalf("the anti-affinity lock created %v", taken)
	}

	_, _, group := poolTemplateTokensFromEnv(deps.Config, env)
	if group == sanitizeTagValue(group) {
		t.Fatalf("the group %q does not exercise sanitizing", group)
	}
	_, _, _, record := vmPoolStepRecord(t, groupExecution(ensureTestPool, group), false, func(h *aj.Handle) {
		vmPoolStep(t, h, "CreatePool", false)
	})
	if got := lockStepSentinels(record); !slices.Contains(got, taken[0]) {
		t.Fatalf("settlement rebuilds %v from group %q, and the lock created %s", got, group, taken[0])
	}
}

// guestListingClient adds the LXC listing that the shared-pool proof reads.
type guestListingClient struct{ *cleanupTaskClient }

func (c guestListingClient) Nodes() ns.Service {
	return &cleanupPoolNodes{Service: c.cleanupTaskClient.Nodes(), mode: "complete"}
}

// TestVMPoolStepBeforeVMWorkKeepsItsRoute pins the records that settlement
// leaves alone. A create_vm that stopped after planning its first pool call
// and before any VM write leaves a sole planned CreatePool, even with a pool
// service that would answer every read exactly. With a deployment pool, an
// attested cleanup still closes that record through the shared-pool proof and
// leaves its history as it was. Without one, cleanup refuses it as before.
func TestVMPoolStepBeforeVMWorkKeepsItsRoute(t *testing.T) {
	for _, pool := range []string{ensureTestPool, ""} {
		name := map[string]string{ensureTestPool: "deployment pool", "": "no pool"}[pool]
		t.Run(name, func(t *testing.T) { testVMPoolStepBeforeVMWork(t, pool) })
	}
}

func testVMPoolStepBeforeVMWork(t *testing.T, pool string) {
	t.Helper()
	var planned string
	deps, j, client, record := vmPoolStepRecord(t, groupExecution(pool, ""), false, func(h *aj.Handle) {
		planned = vmPoolStep(t, h, "CreatePool", false)
	})
	tasks := &cleanupTaskClient{Client: client}
	deps.PVE = guestListingClient{tasks}
	handle, err := j.Acquire(t.Context(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := settlePlannedLockSteps(t.Context(), deps, handle); err != nil {
		t.Fatal(err)
	}
	if step := stepByID(t, handle.Record(), planned); step.State != aj.Planned {
		t.Fatalf("settlement took the pool step of a record whose VM work never began to %s", step.State)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if reads := client.log.names(); len(reads) != 0 {
		t.Fatalf("settlement read %v for a record whose VM work never began", reads)
	}
	result, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, cleanupAttestedDecision(record.ID))
	if pool == "" {
		assertNoPoolCleanupRefused(t, j, record.ID, planned, err)
		return
	}
	if err != nil {
		t.Fatalf("cleanup refused the shared-pool record: %v", err)
	}
	proof := result.Verifications[len(result.Verifications)-1]
	if result.State != aj.Cleaned || !proof.AbsenceVerified || !strings.Contains(proof.EvidenceJSON, `"shared_pool_only":"`+ensureTestPool+`"`) {
		t.Fatalf("cleanup did not close the record through the shared-pool proof: %s %s", result.State, proof.EvidenceJSON)
	}
	if step := stepByID(t, result, planned); step.State != aj.Planned || tasks.taskCalls != 1 {
		t.Fatalf("the shared-pool route changed the pool step to %s with %d task observations", step.State, tasks.taskCalls)
	}
}

// assertNoPoolCleanupRefused checks that cleanup refused a record with no
// pool at its pending-mutation settlement and left the record as it was.
func assertNoPoolCleanupRefused(t *testing.T, j *aj.Journal, id, planned string, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "pending_mutation_settlement") {
		t.Fatalf("cleanup did not refuse the pool step of a record with no pool: %v", err)
	}
	after, inspectErr := j.Inspect(id)
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if step := stepByID(t, after, planned); step.State != aj.Planned || after.State != aj.ReconciliationRequired {
		t.Fatalf("a refused cleanup changed the record: %s %s", after.State, step.State)
	}
}
