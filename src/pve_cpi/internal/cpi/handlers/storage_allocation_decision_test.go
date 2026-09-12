package handlers

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"encoding/json"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	ns "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

func TestAllocationDecisionAdoptsExactReadyVM(t *testing.T) {
	deps, journal, client, record, _ := resumeVMFixture(t)
	next, err := ApplyStorageAllocationDecision(context.Background(), deps, journal, []string{"pve1"}, StorageAllocationDecision{Action: "adopt", AllocationID: record.ID, ExpectedCID: record.CID, DecisionID: "operator-ticket-42"})
	if err != nil {
		t.Fatal(err)
	}
	if next.State != aj.Adopted || next.CID != record.CID {
		t.Fatalf("unexpected disposition: %s %q", next.State, next.CID)
	}
	proof := next.Verifications[len(next.Verifications)-1]
	if !proof.OwnershipVerified || !strings.Contains(proof.EvidenceJSON, "operator-ticket-42") || proof.AbsenceVerified {
		t.Fatalf("invalid adoption evidence: %+v", proof)
	}
	if len(client.destroyed) != 0 || len(client.descWrites) != 0 {
		t.Fatal("adoption mutated PVE")
	}
}

func TestAllocationDecisionRejectsWrongCIDAndLiveCleanup(t *testing.T) {
	for _, action := range []string{"adopt", "finalize-cleanup"} {
		t.Run(action, func(t *testing.T) {
			deps, journal, _, record, _ := resumeVMFixture(t)
			_, err := ApplyStorageAllocationDecision(context.Background(), deps, journal, []string{"pve1"}, StorageAllocationDecision{Action: action, AllocationID: record.ID, ExpectedCID: "999", DecisionID: "operator-ticket-42"})
			if err == nil {
				t.Fatal("unsafe disposition accepted")
			}
			current, err := journal.Inspect(record.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.State != record.State || len(current.Verifications) != len(record.Verifications) {
				t.Fatal("failed disposition changed record")
			}
		})
	}
}

func TestAllocationDecisionFinalizesVerifiedAbsentVM(t *testing.T) {
	deps, journal, client, record, _ := resumeVMFixture(t)
	delete(client.configs, 123)
	next, err := ApplyStorageAllocationDecision(context.Background(), deps, journal, []string{"pve1"}, StorageAllocationDecision{Action: "finalize-cleanup", AllocationID: record.ID, DecisionID: "operator-ticket-43"})
	if err != nil {
		t.Fatal(err)
	}
	if next.State != aj.Cleaned {
		t.Fatalf("state %s", next.State)
	}
	proof := next.Verifications[len(next.Verifications)-1]
	if !proof.AbsenceVerified || !proof.ArtifactDispositionVerified || proof.OwnershipVerified {
		t.Fatal("invalid cleanup evidence")
	}
}

func TestAllocationDecisionRejectsOrphanVMVolumeCleanup(t *testing.T) {
	deps, journal, client, record, _ := resumeVMFixture(t)
	delete(client.configs, 123)
	client.nodesRead.content = ns.ListStorageContentResponse{json.RawMessage(`{"volid":"a:123/vm-123-disk-0.qcow2"}`)}
	_, err := ApplyStorageAllocationDecision(context.Background(), deps, journal, []string{"pve1"}, StorageAllocationDecision{Action: "finalize-cleanup", AllocationID: record.ID, DecisionID: "operator-ticket-44"})
	if err == nil {
		t.Fatal("orphan root volume incorrectly treated as absent")
	}
	current, err := journal.Inspect(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != record.State {
		t.Fatal("unsafe cleanup changed record")
	}
}

func TestAllocationDecisionAbsenceDoesNotSettleUnknownMutation(t *testing.T) {
	deps, journal, _, original, _ := resumeVMFixture(t)
	id, err := aj.NewAllocationID()
	if err != nil {
		t.Fatal(err)
	}
	handle, err := journal.CreateDisk(context.Background(), id, original.Intent)
	if err != nil {
		t.Fatal(err)
	}
	step, err := storageMutationIntent(handle, "allocate", aj.Target{Node: "pve1", Storage: "a", IntendedVolume: "a:pending"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = storageMutationSubmitted(handle, step, "UPID:pve1:pending"); err != nil {
		t.Fatal(err)
	}
	if err = handle.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = ApplyStorageAllocationDecision(context.Background(), deps, journal, []string{"pve1"}, StorageAllocationDecision{Action: "finalize-cleanup", AllocationID: id, DecisionID: "operator-ticket-45"})
	if err == nil || !strings.Contains(err.Error(), "unsettled") {
		t.Fatalf("unknown mutation not blocked: %v", err)
	}
	record, err := journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != aj.Submitted {
		t.Fatalf("unknown task state changed: %s", record.State)
	}
}

func TestAllocationDecisionDiskAdoptionAfterSetRemoval(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	before := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
	next, err := ApplyStorageAllocationDecision(t.Context(), deps, journal, []string{"n1"}, StorageAllocationDecision{Action: "adopt", AllocationID: id, ExpectedCID: cid, DecisionID: "disk-adoption-audit"})
	if err != nil {
		t.Fatal(err)
	}
	if next.State != aj.Adopted || next.CID != cid {
		t.Fatalf("invalid disk adoption: %+v", next)
	}
	proof := next.Verifications[len(next.Verifications)-1]
	if !proof.OwnershipVerified || !strings.Contains(proof.EvidenceJSON, "targeted_live_disk_ownership") {
		t.Fatal("disk adoption lacks actual physical proof")
	}
	if client.state.parkMutations != 0 || len(client.state.created) != 0 || client.moves != 0 || client.resizeCalls != 0 {
		t.Fatal("adoption mutated PVE")
	}
	after := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
	changed := 0
	for path, data := range before {
		if after[path] != data {
			changed++
			if !strings.HasSuffix(path, "allocation-"+id+".json") {
				t.Fatalf("adoption changed unrelated file %s", path)
			}
		}
	}
	if changed != 1 {
		t.Fatalf("expected one record transition, got %d", changed)
	}
	snapshot := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
	if _, err = ApplyStorageAllocationDecision(t.Context(), deps, journal, []string{"n1"}, StorageAllocationDecision{Action: "adopt", AllocationID: id, ExpectedCID: cid, DecisionID: "repeat"}); err == nil {
		t.Fatal("already adopted record adopted again")
	}
	if !reflect.DeepEqual(snapshot, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
		t.Fatal("failed repeat changed journal")
	}
}
func TestAllocationDecisionDiskAdoptionRejectsDuplicateHolderAndForeignSerial(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(strconv.FormatBool(foreign), func(t *testing.T) {
			deps, client, journal, id, cid := lifecycleFlowFixture(t)
			drive := client.state.configs[777]["scsi1"].(string)
			if foreign {
				drive = "b:123/foreign.raw," + strings.SplitN(drive, ",", 2)[1]
			}
			client.state.configs[778] = map[string]any{"name": "arbitrary-label", "tags": "bosh-parker", "scsi1": drive}
			before := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
			if _, err := ApplyStorageAllocationDecision(t.Context(), deps, journal, []string{"n1"}, StorageAllocationDecision{Action: "adopt", AllocationID: id, ExpectedCID: cid, DecisionID: "disk-ambiguous"}); err == nil {
				t.Fatal("ambiguous holder adopted")
			}
			if !reflect.DeepEqual(before, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
				t.Fatal("failed decision wrote evidence")
			}
			if client.state.parkMutations != 0 || len(client.state.created) != 0 {
				t.Fatal("failed decision mutated PVE")
			}
		})
	}
}
func TestAllocationDecisionDiskAdoptionScrubsReadFailure(t *testing.T) {
	deps, client, journal, id, cid := lifecycleFlowFixture(t)
	client.state.readErr = errors.New("API credential SUPER-SECRET")
	before := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
	_, err := ApplyStorageAllocationDecision(t.Context(), deps, journal, []string{"n1"}, StorageAllocationDecision{Action: "adopt", AllocationID: id, ExpectedCID: cid, DecisionID: "read-failure"})
	if err == nil || strings.Contains(err.Error(), "SUPER-SECRET") {
		t.Fatalf("unsafe error: %v", err)
	}
	if !reflect.DeepEqual(before, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
		t.Fatal("read failure changed record")
	}
}
func TestAllocationDecisionWaitsForRecordOwnership(t *testing.T) {
	deps, _, journal, id, cid := lifecycleFlowFixture(t)
	handle, err := journal.Acquire(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	before := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	_, err = ApplyStorageAllocationDecision(ctx, deps, journal, []string{"n1"}, StorageAllocationDecision{Action: "adopt", AllocationID: id, ExpectedCID: cid, DecisionID: "contended"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("record lock bypassed or cancellation lost: %v", err)
	}
	if !reflect.DeepEqual(before, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
		t.Fatal("lock failure changed journal")
	}
	if err = handle.Close(); err != nil {
		t.Fatal(err)
	}
}
func TestAllocationDecisionMalformedDiskSentinelBlocksAbsence(t *testing.T) {
	deps, journal, client, record, _ := resumeVMFixture(t)
	delete(client.configs, 123)
	client.configs[456] = map[string]any{"description": "<!--BOSH:{broken-provenance-->"}
	_, err := ApplyStorageAllocationDecision(t.Context(), deps, journal, []string{"pve1"}, StorageAllocationDecision{Action: "finalize-cleanup", AllocationID: record.ID, DecisionID: "bad-provenance"})
	if err == nil {
		t.Fatal("malformed provenance certified artifact absence")
	}
	current, err := journal.Inspect(record.ID)
	if err != nil || current.State != record.State {
		t.Fatalf("failed audit changed state %+v %v", current, err)
	}
}

func TestAllocationDecisionRequiresAdapterLevelNamespaceAndClusterContinuity(t *testing.T) {
	for _, wrongNamespace := range []bool{true, false} {
		t.Run(strconv.FormatBool(wrongNamespace), func(t *testing.T) {
			deps, _, journal, id, cid := lifecycleFlowFixture(t)
			if wrongNamespace {
				deps.Config.StoragePlacementNamespace = "other"
			} else {
				deps.PVE = decisionWrongCluster{Client: deps.PVE}
			}
			before := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
			_, err := ApplyStorageAllocationDecision(t.Context(), deps, journal, []string{"n1"}, StorageAllocationDecision{Action: "adopt", AllocationID: id, ExpectedCID: cid, DecisionID: "wrong-context"})
			if err == nil {
				t.Fatal("adapter trusted mismatched caller context")
			}
			if !reflect.DeepEqual(before, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
				t.Fatal("context rejection changed journal")
			}
		})
	}
}

type decisionWrongCluster struct{ pve.Client }

func (c decisionWrongCluster) Nodes() ns.Service {
	return decisionWrongClusterNodes{Service: c.Client.Nodes()}
}

type decisionWrongClusterNodes struct{ ns.Service }

func (n decisionWrongClusterNodes) ListCertificatesInfo(context.Context, string) (*ns.ListCertificatesInfoResponse, error) {
	response := ns.ListCertificatesInfoResponse{json.RawMessage(`{"filename":"pve-root-ca.pem","fingerprint":"cd:cd:cd:cd:cd:cd:cd:cd:cd:cd:cd:cd:cd:cd:cd:cd:cd:cd:cd:cd:cd:cd:cd:cd:cd:cd:cd:cd:cd:cd:cd:cd"}`)}
	return &response, nil
}

func TestAllocationDecisionMissingRecordCreatesNoOwnershipFiles(t *testing.T) {
	deps, _, journal, _, _ := lifecycleFlowFixture(t)
	id, err := aj.NewAllocationID()
	if err != nil {
		t.Fatal(err)
	}
	before := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
	_, err = ApplyStorageAllocationDecision(t.Context(), deps, journal, []string{"n1"}, StorageAllocationDecision{Action: "finalize-cleanup", AllocationID: id, DecisionID: "unknown-record"})
	if err == nil {
		t.Fatal("nonexistent record accepted")
	}
	if !reflect.DeepEqual(before, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
		t.Fatal("failed lookup created ownership files")
	}
}

//nolint:gocognit // Keep the complete failure/recovery scenario and its evidence assertions together.
func TestAllocationDecisionAdoptionHonorsSettledRetryWithoutHidingOldArtifacts(t *testing.T) {
	for _, resurfaced := range []bool{false, true} {
		t.Run(strconv.FormatBool(resurfaced), func(t *testing.T) {
			deps, client, journal, originalID, _ := lifecycleFlowFixture(t)
			original, err := journal.Inspect(originalID)
			if err != nil {
				t.Fatal(err)
			}
			id, err := aj.NewAllocationID()
			if err != nil {
				t.Fatal(err)
			}
			handle, err := journal.CreateDisk(t.Context(), id, original.Intent)
			if err != nil {
				t.Fatal(err)
			}
			oldName, err := pve.AllocationVolumeName(124, original.Namespace, id, "raw")
			if err != nil {
				t.Fatal(err)
			}
			oldVolume := "a:124/" + oldName
			if _, err = storageMutationIntent(handle, "create", aj.Target{Node: "n1", Storage: "a", Backing: "nfs://nas/a", VMID: 124, IntendedVolume: oldVolume}, nil); err != nil {
				t.Fatal(err)
			}
			evidenceID, evidenceJSON, err := aj.VerificationEvidence(map[string]any{"operation": "validated pre-execution rejection", "allocation_id": id, "absence": true})
			if err != nil {
				t.Fatal(err)
			}
			proof := aj.AttemptVerification{Verification: aj.Verification{EvidenceID: evidenceID, EvidenceJSON: evidenceJSON, Complete: true, AbsenceVerified: true, ArtifactDispositionVerified: true}, OutcomesKnown: true, NoSubmissionVerified: true}
			if err = handle.BeginAttempt(handle.Record().ActivePlan(), proof); err != nil {
				t.Fatal(err)
			}
			name, err := pve.AllocationVolumeName(125, original.Namespace, id, "raw")
			if err != nil {
				t.Fatal(err)
			}
			volume := "a:125/" + name
			step, err := storageMutationIntent(handle, "create", aj.Target{Node: "n1", Storage: "a", Backing: "nfs://nas/a", VMID: 125, IntendedVolume: volume}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = storageMutationObserved(handle, step, []string{volume}, false); err != nil {
				t.Fatal(err)
			}
			record := handle.Record()
			cid, err := pve.EncodeDiskCID(volume, &pve.DiskCIDMeta{ID: record.DiskToken, Format: "raw"})
			if err != nil {
				t.Fatal(err)
			}
			record.CID = cid
			record.State = aj.ReadyToReturn
			if err = handle.Save(record); err != nil {
				t.Fatal(err)
			}
			if err = handle.Close(); err != nil {
				t.Fatal(err)
			}
			client.state.configs[778] = map[string]any{"scsi1": volume + ",serial=" + record.DiskToken + ",size=5G"}
			client.state.volumes[volume] = &ns.GetStorageContentResponse{Size: 5 << 30, Format: "raw"}
			if resurfaced {
				client.state.volumes[oldVolume] = &ns.GetStorageContentResponse{Size: 5 << 30, Format: "raw"}
			}
			before := diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)
			next, err := ApplyStorageAllocationDecision(t.Context(), deps, journal, []string{"n1"}, StorageAllocationDecision{Action: "adopt", AllocationID: id, ExpectedCID: cid, DecisionID: "retry-adoption"})
			if resurfaced {
				if err == nil {
					t.Fatal("old artifact hidden by successful current target")
				}
				if !reflect.DeepEqual(before, diagnosticFiles(t, deps.Config.StorageAllocationJournalDir)) {
					t.Fatal("old-artifact rejection changed evidence")
				}
			} else if err != nil || next.State != aj.Adopted {
				t.Fatalf("verified no-submission retry permanently blocked: %+v %v", next, err)
			}
		})
	}
}

func TestAllocationDecisionAdoptsFreeVolumeCIDWithoutStableToken(t *testing.T) {
	deps, client, journal, originalID, _ := lifecycleFlowFixture(t)
	original, err := journal.Inspect(originalID)
	if err != nil {
		t.Fatal(err)
	}
	id, err := aj.NewAllocationID()
	if err != nil {
		t.Fatal(err)
	}
	h, err := journal.CreateDisk(t.Context(), id, original.Intent)
	if err != nil {
		t.Fatal(err)
	}
	name, err := pve.AllocationVolumeName(126, original.Namespace, id, "raw")
	if err != nil {
		t.Fatal(err)
	}
	volume := "a:126/" + name
	step, err := storageMutationIntent(h, "create", aj.Target{Node: "n1", Storage: "a", Backing: "nfs://nas/a", VMID: 126, IntendedVolume: volume}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = storageMutationObserved(h, step, []string{volume}, false); err != nil {
		t.Fatal(err)
	}
	cid, err := pve.EncodeDiskCID(volume, &pve.DiskCIDMeta{Format: "raw"})
	if err != nil {
		t.Fatal(err)
	}
	record := h.Record()
	record.State = aj.ReadyToReturn
	record.CID = cid
	if err = h.Save(record); err != nil {
		t.Fatal(err)
	}
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	client.state.volumes[volume] = &ns.GetStorageContentResponse{Size: 5 << 30, Format: "raw"}
	next, err := ApplyStorageAllocationDecision(t.Context(), deps, journal, []string{"n1"}, StorageAllocationDecision{Action: "adopt", AllocationID: id, ExpectedCID: cid, DecisionID: "free-volume-adoption"})
	if err != nil || next.State != aj.Adopted || next.CID != cid {
		t.Fatalf("free-volume adoption changed CID or failed %+v %v", next, err)
	}
	if len(client.state.configs) != 1 || client.state.parkMutations != 0 || len(client.state.created) != 0 {
		t.Fatal("adoption parked or attached a free disk")
	}
}

func TestAllocationDecisionFinalizesExplicitlyClosedNoSubmissionAttempt(t *testing.T) {
	deps, journal, _, original, _ := resumeVMFixture(t)
	id, err := aj.NewAllocationID()
	if err != nil {
		t.Fatal(err)
	}
	intent := original.Intent
	intent.FrozenInputsFingerprint = strings.Repeat("b", 64)
	h, err := journal.CreateDisk(t.Context(), id, intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = storageMutationIntent(h, "create", aj.Target{Node: "pve1", Storage: "a", Backing: "nfs://nas/a", IntendedVolume: "a:9001/unsubmitted.raw"}, nil); err != nil {
		t.Fatal(err)
	}
	proofID, proofJSON, err := aj.VerificationEvidence(map[string]any{"operation": "verified no-submission", "allocation_id": id})
	if err != nil {
		t.Fatal(err)
	}
	proof := aj.AttemptVerification{Verification: aj.Verification{EvidenceID: proofID, EvidenceJSON: proofJSON, Complete: true, AbsenceVerified: true, ArtifactDispositionVerified: true}, NoSubmissionVerified: true, OutcomesKnown: true}
	if err = h.CompleteAttempt(proof); err != nil {
		t.Fatal(err)
	}
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := ApplyStorageAllocationDecision(t.Context(), deps, journal, []string{"pve1"}, StorageAllocationDecision{Action: "finalize-cleanup", AllocationID: id, DecisionID: "closed-attempt-cleanup"})
	if err != nil || next.State != aj.Cleaned {
		t.Fatalf("verified closed attempt could not be finalized %+v %v", next, err)
	}
}
