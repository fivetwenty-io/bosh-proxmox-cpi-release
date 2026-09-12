package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	inv "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
	ns "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

type cleanupPoolNodes struct {
	ns.Service
	mode string
}

func (n *cleanupPoolNodes) ListLxc(context.Context, string) (*ns.ListLxcResponse, error) {
	rows := ns.ListLxcResponse{}
	switch n.mode {
	case "LXC present":
		rows = append(rows, json.RawMessage(`{"vmid":123}`))
	case "nil LXC":
		return nil, nil
	case "duplicate guest":
		rows = append(rows, json.RawMessage(`{"vmid":999}`))
	}
	return &rows, nil
}
func (n *cleanupPoolNodes) ListQemu(ctx context.Context, node string, params *ns.ListQemuParams) (*ns.ListQemuResponse, error) {
	switch n.mode {
	case "nil QEMU":
		return nil, nil
	case "malformed QEMU":
		r := ns.ListQemuResponse{json.RawMessage(`{"vmid":null}`)}
		return &r, nil
	case "duplicate guest":
		r := ns.ListQemuResponse{json.RawMessage(`{"vmid":999}`)}
		return &r, nil
	}
	return n.Service.ListQemu(ctx, node, params)
}

func cleanupPoolFixture(t *testing.T, mode string) (Deps, *aj.Journal, *allocationAuditClient, aj.Record) {
	t.Helper()
	deps, j, client := auditFixture(t)
	client.nodesRead.Service = &cleanupPoolNodes{Service: &resumeVMNodes{Service: client.nodesRead.Service}, mode: mode}
	def, err := pve.ParseStorageEntry(client.storageRead.definitions[0])
	if err != nil {
		t.Fatal(err)
	}
	plan := StorageAllocationPlan{Version: 1, Namespace: "director", AllocationKey: "agent", PolicyFingerprint: strings.Repeat("a", 64), Node: "pve1", Definitions: map[string]pve.StorageInfo{"a": def}, VMExecution: &StorageVMExecution{Version: 1, Node: "pve1", Pool: ensureTestPool}, Charges: []inv.ChargeRecord{{Charge: inv.Charge{ID: "root", Role: storageRoleRoot, Node: "pve1", StorageID: "a", Bytes: 1 << 30}, CapacityKey: def.BackingKey()}}}
	if mode == "exclusive lock" {
		plan.VMExecution.Pool = "bosh-lock-vmid-123"
	}
	if mode == "acquired capacity" {
		plan.Charges[0].Acquired = true
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	h, err := j.AcquireVM(t.Context(), "agent", aj.Intent{IntentFingerprint: strings.Repeat("b", 64), PolicyFingerprint: plan.PolicyFingerprint, PlanVersion: 1, Plan: raw})
	if err != nil {
		t.Fatal(err)
	}
	record := h.Record()
	step := aj.Step{ID: "pool-intent", Attempt: 0, Kind: "vm.Pool.CreatePool", State: aj.Planned, Target: aj.Target{Node: "pve1", VMID: 123}}
	switch mode {
	case "wrong kind":
		step.Kind = managedVMStepClone
	case "wrong node":
		step.Target.Node = "other"
	case "storage target":
		step.Target.Storage = "a"
		step.Target.Backing = def.BackingKey()
	case "charged mutation":
		step.Charges = []aj.Charge{{Backing: def.BackingKey(), PlannedBytes: 1, OutstandingBytes: 1}}
	}
	record.Steps = []aj.Step{step}
	if mode == "additional step" {
		extra := step
		extra.ID = "second"
		record.Steps = append(record.Steps, extra)
	}
	record.State = aj.ReconciliationRequired
	record.Reason = "pool response uncertain"
	if err = h.Save(record); err != nil {
		t.Fatal(err)
	}
	record = h.Record()
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	return deps, j, client, record
}

func TestCleanupPoolOnlyPreservesSharedPoolAndPendingHistory(t *testing.T) {
	deps, j, client, record := cleanupPoolFixture(t, "complete")
	// The inherited pool service is intentionally absent: any pool read/write
	// would panic. Cleanup must preserve shared infrastructure without touching it.
	tasks := &cleanupTaskClient{Client: client}
	deps.PVE = tasks
	if _, ok := cleanupOnlySharedPool(record); !ok {
		t.Fatal("fixture pool scope rejected")
	}

	got, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, cleanupAttestedDecision(record.ID))
	if err != nil {
		t.Fatalf("%v: %v", err, errors.Unwrap(err))
	}
	if got.State != aj.Cleaned || !reflect.DeepEqual(got.Steps, record.Steps) || !reflect.DeepEqual(got.Attempts, record.Attempts) {
		t.Fatal("cleanup changed original steps or frozen reservations")
	}
	if tasks.taskCalls != 1 || len(client.destroyed) != 0 || len(client.descWrites) != 0 {
		t.Fatal("missing task proof or unexpected VM mutation")
	}
	proof := got.Verifications[len(got.Verifications)-1]
	if !proof.Complete || !proof.AbsenceVerified || !proof.ArtifactDispositionVerified || !strings.Contains(proof.EvidenceJSON, `"shared_pool_only":"`+ensureTestPool+`"`) {
		t.Fatal("missing durable pool-preserving disposition")
	}
	if storageLifecycleSettled(got) == nil {
		t.Fatal("pending pool history became ordinary replay authority")
	}
}

func TestCleanupPoolOnlyRefusesAmbiguousHistoryAndPresence(t *testing.T) {
	modes := []string{"wrong kind", "wrong node", "storage target", "charged mutation", "additional step", "exclusive lock", "acquired capacity", "unfenced", "unsettled", "incomplete task proof", "active task", "visibility", "VM present", "plain image present", "ISO present", "malformed listing", "LXC present", "nil LXC", "nil QEMU", "malformed QEMU", "duplicate guest"}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) { testCleanupPoolOnlyRefusal(t, mode) })
	}
}
func testCleanupPoolOnlyRefusal(t *testing.T, mode string) {
	t.Helper()
	deps, j, client, record := cleanupPoolFixture(t, mode)
	tasks := &cleanupTaskClient{Client: client}
	deps.PVE = tasks
	decision := cleanupAttestedDecision(record.ID)
	switch mode {
	case "unfenced":
		decision.PreviousWriterFenced = false
	case "unsettled":
		decision.RemoteTasksSettled = false
	case "incomplete task proof":
		tasks.incomplete = true
	case "active task":
		tasks.taskErr = errors.New("active task")
	case "visibility":
		tasks.visibilityErr = errors.New("missing audit")
	case "VM present":
		client.configs[123] = map[string]any{"name": "foreign"}
	case "plain image present":
		client.nodesRead.content = append(client.nodesRead.content, json.RawMessage(`{"volid":"a:123/vm-123-disk-0.qcow2"}`))
	case "ISO present":
		client.nodesRead.content = append(client.nodesRead.content, json.RawMessage(`{"volid":"a:iso/vm-123-config.iso"}`))
	case "malformed listing":
		client.nodesRead.content = append(client.nodesRead.content, json.RawMessage(`{"volid":null}`))
	}
	before := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
	if _, err := CleanupStorageAllocation(t.Context(), deps, j, []string{"pve1"}, decision); err == nil {
		t.Fatal("unsafe pool-only disposition accepted")
	}
	if !reflect.DeepEqual(before, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
		t.Fatal("refused cleanup changed journal")
	}
	if len(client.destroyed) != 0 || len(client.descWrites) != 0 {
		t.Fatal("refused cleanup mutated VM")
	}
}

func TestCleanupPoolOnlyRejectsChangedRecordScope(t *testing.T) {
	_, _, _, original := cleanupPoolFixture(t, "complete")
	for _, mode := range []string{"disk", "returned", "attempt", "upid", "volumes", "external", "bytes", "parameters"} {
		t.Run(mode, func(t *testing.T) {
			record := original
			record.Steps = append([]aj.Step(nil), original.Steps...)
			switch mode {
			case "disk":
				record.Kind = "disk"
			case "returned":
				record.CID = "123"
			case "attempt":
				record.Attempts = []aj.Attempt{{}, {}}
			case "upid":
				record.Steps[0].UPID = "UPID:unknown"
			case "volumes":
				record.Steps[0].VolIDs = []string{"a:123/vm-123-disk-0.qcow2"}
			case "external":
				record.Steps[0].Target.External = true
			case "bytes":
				record.Steps[0].Target.VirtualBytes = 1
			case "parameters":
				record.Steps[0].Parameters = json.RawMessage(`{"version":1,"kind":"unknown"}`)
			}
			if _, ok := cleanupOnlySharedPool(record); ok {
				t.Fatal("changed scope admitted")
			}
		})
	}
}

func TestPoolOnlyGuestRowsRequireUnambiguousIdentity(t *testing.T) {
	for _, input := range []string{`[null]`, `[{"vmid":null}]`, `[{"vmid":true}]`, `[{"vmid":1.5}]`, `[{"vmid":0}]`, `[{"vmid":-1}]`, `[{"vmid":999},{"vmid":999}]`, `[{"vmid":123}]`} {
		var rows []json.RawMessage
		if err := json.Unmarshal([]byte(input), &rows); err != nil {
			t.Fatal(err)
		}
		if err := validatePoolOnlyGuestRows(rows, 123, map[int]bool{}); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
	seen := map[int]bool{}
	rows := []json.RawMessage{json.RawMessage(`{"vmid":"999"}`)}
	if err := validatePoolOnlyGuestRows(rows, 123, seen); err != nil {
		t.Fatal(err)
	}
	if err := validatePoolOnlyGuestRows(rows, 123, seen); err == nil {
		t.Fatal("duplicate across nodes or guest types accepted")
	}
}
