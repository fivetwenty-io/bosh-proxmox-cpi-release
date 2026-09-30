package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	ns "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	pveerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// These tests pin the three refusals that used to fall through to the
// generic "complete consistent allocation audit required" error. Each now
// names the audit's findings, in the same place in its order of checks.
// The tests after gateSiteMalformedVM pin the message of each remaining
// audit-gate site, down to the operation, the count, and the first finding.

// TestUnsubmittedVMGenerationInspectionNamesAuditFindings is the zero-step
// resume path: a VM generation with no recorded step whose absence proof
// needs a complete audit.
func TestUnsubmittedVMGenerationInspectionNamesAuditFindings(t *testing.T) {
	deps, j, c := auditFixture(t)
	c.nodesRead.nodeNames = []string{"pve1"}
	def, err := pve.ParseStorageEntry(c.storageRead.definitions[0])
	if err != nil {
		t.Fatal(err)
	}
	plan := StorageAllocationPlan{Version: 1, Namespace: "director", AllocationKey: "agent", PolicyFingerprint: strings.Repeat("a", 64), Node: "pve1", Definitions: map[string]pve.StorageInfo{"a": def}, Targets: []StoragePlanTarget{{Role: "root", Node: "pve1", StorageID: "a", BackingKey: def.BackingKey(), VirtualBytes: 1 << 30}}}
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	args := []json.RawMessage{json.RawMessage(`"agent"`)}
	fingerprint, err := storageCallerIntentFingerprint("create_vm", args)
	if err != nil {
		t.Fatal(err)
	}
	h, err := j.AcquireVM(t.Context(), "agent", aj.Intent{IntentFingerprint: fingerprint, PolicyFingerprint: plan.PolicyFingerprint, PlanVersion: 1, Plan: payload})
	if err != nil {
		t.Fatal(err)
	}
	record := h.Record()
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	c.nodesRead.failure = errors.New("listing failed: token=secret-value")
	_, err = resumeManagedVM(t.Context(), deps, &createVMParsedArgs{agentID: "agent"}, nil, j, record, args, nil)
	message := directorMessage(err)
	if !strings.HasPrefix(message, "unsubmitted VM generation inspection refused: 1 audit issue; ") || !strings.Contains(message, `storage "a" on node "pve1"`) {
		t.Fatalf("zero-step resume refusal names no finding: %q", message)
	}
	if strings.Contains(message, "secret-value") || strings.Contains(message, "complete consistent allocation audit required") {
		t.Fatalf("refusal leaked or fell back to the generic error: %q", message)
	}
	after, err := j.Inspect(record.ID)
	if err != nil || len(after.Verifications) != 0 {
		t.Fatalf("refused inspection retained evidence: %+v %v", after.Verifications, err)
	}
}

// TestVMCleanupCompletionNamesAuditFindings drives managedVMDispositionProof,
// which both delete_vm and explicit cleanup of a deleted VM finish with.
func TestVMCleanupCompletionNamesAuditFindings(t *testing.T) {
	report := StorageAllocationAudit{Complete: false, VMScanComplete: true, Issues: []string{`storage "a" on node "pve1" could not be inspected: connection to 10.0.0.1:8006 failed`}}
	_, err := managedVMDispositionProof(context.Background(), Deps{}, report, aj.Record{ID: "allocation"}, 123, nil)
	message := directorMessage(err)
	if !strings.HasPrefix(message, "VM cleanup completion refused: 1 audit issue; storage \"a\" on node \"pve1\" could not be inspected") {
		t.Fatalf("completion refusal names no finding: %q", message)
	}
	report.addConflict("remote allocation other (VM 9) is outside recorded mutation targets", "VM 9 outside its targets")
	report.Complete, report.Issues = false, nil
	_, err = cleanupVMDeletedOwnership(context.Background(), Deps{}, aj.Record{ID: "allocation"}, report)
	if message := directorMessage(err); !strings.HasPrefix(message, "VM cleanup completion refused: 1 audit conflict; VM 9 outside its targets") {
		t.Fatalf("deleted-VM cleanup refusal names no finding: %q", message)
	}
}

// TestRetainedVMCleanupCompletionNamesAuditFindings loses audit visibility
// after the retained volume is deleted, so only the completion audit fails.
func TestRetainedVMCleanupCompletionNamesAuditFindings(t *testing.T) {
	deps, client, journal, vmID, _ := retainDeleteFixture(t)
	if _, err := HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{}); err != nil {
		t.Fatal(err)
	}
	volume := strings.Split(retainedVolume(t, client), ",")[0]
	client.visibilityErrAfterDelete = errors.New("visibility lost")
	_, err := CleanupStorageAllocation(t.Context(), deps, journal, []string{"n1", "n2"}, cleanupAttestedDecision(vmID))
	message := directorMessage(err)
	if !strings.HasPrefix(message, "retained VM cleanup completion refused: ") || !strings.Contains(message, "cluster-wide VM and storage audit visibility is unproven") {
		t.Fatalf("retained completion refusal names no finding: %q", message)
	}
	if client.state.volumes[volume] != nil {
		t.Fatal("the completion gate ran before the retained volume was deleted")
	}
}

// gateSiteMalformedVM is a guest whose disk provenance sentinel does not
// parse. The audit reports it as one issue and nothing else, so it refuses a
// gate that requires a complete audit while the VM scan and conflicts pass.
const gateSiteMalformedVM = 456

func breakGateSiteProvenance(configs map[int]map[string]any) {
	configs[gateSiteMalformedVM] = map[string]any{"description": "<!--BOSH:{broken-provenance-->"}
}

// requireGateRefusal checks that err reaches the Director as the refusal of
// operation, led by the count and the first finding its gate refused on.
func requireGateRefusal(t *testing.T, err error, lead string) {
	t.Helper()
	if message := directorMessage(err); !strings.HasPrefix(message, lead) {
		t.Fatalf("refusal = %q, want prefix %q", message, lead)
	}
}

// TestRetainedVMCleanupNamesAuditFindings reaches the admission audit that
// disposeManagedVM runs for a retained record. Its gate requires a complete
// audit, so one issue alone refuses it before the retained volume goes.
func TestRetainedVMCleanupNamesAuditFindings(t *testing.T) {
	deps, client, journal, vmID, _ := retainDeleteFixture(t)
	if _, err := HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{}); err != nil {
		t.Fatal(err)
	}
	volume := strings.Split(retainedVolume(t, client), ",")[0]
	breakGateSiteProvenance(client.state.configs)
	handle, err := journal.Acquire(t.Context(), vmID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = disposeManagedVM(t.Context(), deps, journal, handle, false)
	if closeErr := handle.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	requireGateRefusal(t, err, "retained VM cleanup refused: 1 audit issue; VM 456 has malformed disk provenance on n1")
	if client.state.volumes[volume] == nil {
		t.Fatal("the refused cleanup deleted the retained volume")
	}
}

// destroyHookPVE runs afterDestroy once PVE has destroyed VM 777, so a test
// can change what the audits that follow the destroy observe.
type destroyHookPVE struct {
	*lifecycleFlowPVE
	afterDestroy func()
}

func (c *destroyHookPVE) Nodes() ns.Service {
	return destroyHookNodes{Service: c.lifecycleFlowPVE.Nodes(), c: c}
}

type destroyHookNodes struct {
	ns.Service
	c *destroyHookPVE
}

func (n destroyHookNodes) DeleteQemu(ctx context.Context, node, vmid string, params *ns.DeleteQemuParams) (*ns.DeleteQemuResponse, error) {
	response, err := n.Service.DeleteQemu(ctx, node, vmid, params)
	if err == nil && vmid == "777" {
		n.c.afterDestroy()
	}
	return response, err
}

// TestPostDestroyVMResourceCleanupNamesAuditFindings passes the delete_vm
// admission audit and loses completeness only after the destroy, so the
// orphan-volume audit is the first gate to refuse. That gate requires a
// complete audit, so one issue alone refuses it.
func TestPostDestroyVMResourceCleanupNamesAuditFindings(t *testing.T) {
	deps, client, _, _, _ := retainDeleteFixture(t)
	deps.PVE = &destroyHookPVE{lifecycleFlowPVE: client, afterDestroy: func() { breakGateSiteProvenance(client.state.configs) }}
	_, err := HandleDeleteVM(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, "777")}, jsonrpc.Context{})
	requireGateRefusal(t, err, "post-destroy VM resource cleanup refused: 1 audit issue; VM 456 has malformed disk provenance on n1")
	if client.state.configs[777] != nil {
		t.Fatal("the refusal came before the destroy it follows")
	}
}

// TestManagedDiskDeletionProofNamesAuditFindings deletes a detached disk and
// refuses the completion audit that proves the deletion.
func TestManagedDiskDeletionProofNamesAuditFindings(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	delete(client.state.configs[777], "scsi1")
	breakGateSiteProvenance(client.state.configs)
	_, err := HandleDeleteDisk(deps).Handle(context.Background(), []json.RawMessage{planJSON(t, cid)}, jsonrpc.Context{})
	requireGateRefusal(t, err, "delete_disk refused: 1 audit issue; VM 456 has malformed disk provenance on n1")
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if client.deletes != 1 || record.State != aj.ReconciliationRequired {
		t.Fatalf("refused deletion proof: deletes=%d state=%s", client.deletes, record.State)
	}
}

// TestCreateVMRetryNamesAuditFindings drives the absence proof that admits a
// retry of a closed VM attempt.
func TestCreateVMRetryNamesAuditFindings(t *testing.T) {
	deps, j, c := auditFixture(t)
	c.nodesRead.nodeNames = []string{"pve1"}
	breakGateSiteProvenance(c.configs)
	_, err := proveManagedVMAttemptAbsent(t.Context(), deps, j, aj.Record{ID: "allocation"})
	requireGateRefusal(t, err, "create_vm retry refused: 1 audit issue; VM 456 has malformed disk provenance on pve1")
}

// TestCreateDiskRetryNamesAuditFindings has PVE reject the first allocation
// before execution. The guest with malformed provenance appears once the
// create is submitted, so the admission audit passes and the audit that
// would admit the retry is the one refused. That gate requires a complete
// audit, so one issue alone refuses it.
func TestCreateDiskRetryNamesAuditFindings(t *testing.T) {
	m, h, state := managedDiskFixture(t, "spread", true)
	state.createErr = &pveerrors.APIError{HTTPCode: 400, Message: "Parameter verification failed.", Errors: map[string]string{"filename": "rejected"}}
	state.failFirst = true
	state.configs = map[int]map[string]any{}
	before := state.before
	state.before = func(volume string) {
		before(volume)
		breakGateSiteProvenance(state.configs)
	}
	_, err := m.execute(t.Context(), h)
	requireGateRefusal(t, err, "create_disk retry refused: 1 audit issue; VM 456 has malformed disk provenance on n1")
	if len(state.created) != 1 {
		t.Fatalf("the refused retry submitted another allocation: %v", state.created)
	}
}

// TestUnknownVMAllocationCleanupNamesAuditFindings drives the audit that
// admits cleanup of a VM allocation whose create task never reported back.
func TestUnknownVMAllocationCleanupNamesAuditFindings(t *testing.T) {
	deps, j, c, record, _ := unknownVMAllocationFixture(t, storageRoleRoot)
	breakGateSiteProvenance(c.configs)
	_, _, err := observeCleanupVMAllocation(t.Context(), deps, j, record, record.Steps[len(record.Steps)-1])
	requireGateRefusal(t, err, "unknown VM allocation cleanup refused: 1 audit issue; VM 456 has malformed disk provenance on pve1")
	if c.destroyCount != 0 {
		t.Fatal("the refused cleanup destroyed the VM")
	}
}

// TestPoolOnlyCleanupStorageScopeNamesAuditFindings refuses the storage-only
// audit that pool-only cleanup runs. That report never scans VMs, so its gate
// requires only completeness, and a malformed storage definition refuses it.
func TestPoolOnlyCleanupStorageScopeNamesAuditFindings(t *testing.T) {
	deps, _, client, record := cleanupPoolFixture(t, "complete")
	client.storageRead.definitions = append(client.storageRead.definitions, json.RawMessage(`{"type":"nfs"}`))
	err := observePlannedVMStorageAbsence(t.Context(), deps, record, []string{"pve1"}, 123)
	requireGateRefusal(t, err, "pool-only cleanup storage scope refused: 1 audit issue; a storage definition was malformed: entry 1 has no storage name")
}
