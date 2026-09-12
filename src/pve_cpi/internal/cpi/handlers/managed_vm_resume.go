package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

type managedVMObservation struct {
	Node         string
	VMID         int
	Config       map[string]any
	Volumes      []string
	Verification aj.Verification
}

// observeManagedVMRecord proves ownership from the journal, full marker and
// actual storage. It never resolves current sets, writes PVE state, or resumes a
// transfer. Recorded completion avoids depending on expired PVE task logs.
func observeManagedVMRecord(ctx context.Context, deps Deps, journal *aj.Journal, record aj.Record) (*managedVMObservation, error) {
	fail := func(reason string) (*managedVMObservation, error) {
		return nil, cpierrors.Cloud("allocation %s requires reconciliation: %s", record.ID, reason)
	}
	if record.Kind != "vm" || record.State == aj.Deleted || record.State == aj.Cleaned || record.State == aj.VMDeletedRetained {
		return fail("record does not describe an active VM")
	}
	started := time.Now()
	plan, err := activeStorageAllocationPlan(record)
	if err != nil {
		return nil, err
	}
	readback := managedVMRecordReadback{deps: deps, record: record, plan: plan}
	if err := readback.read(ctx); err != nil {
		return nil, err
	}
	node, vmid, cfg, volumes := readback.node, readback.vmid, readback.cfg, readback.volumes
	sort.Strings(volumes)
	audit, err := AuditStorageAllocations(ctx, deps, journal, []string{node})
	if err != nil {
		return nil, err
	}
	if !audit.VMScanComplete || len(audit.Conflicts) > 0 {
		return fail("VM provenance audit is incomplete or inconsistent")
	}
	sightings := 0
	for _, evidence := range audit.Evidence {
		if evidence.Kind == "vm" && evidence.VolumeID == "" && evidence.AllocationID == record.ID {
			sightings++
		}
	}
	if sightings != 1 {
		return fail("allocation marker does not identify exactly one VM")
	}
	evidenceID, evidenceJSON, err := aj.VerificationEvidence(struct {
		Version                int    `json:"version"`
		Scope                  string `json:"scope"`
		AllocationID           string `json:"allocation_id"`
		StartedAt, CompletedAt time.Time
		Node                   string   `json:"node"`
		VMID                   int      `json:"vmid"`
		Volumes                []string `json:"volumes"`
	}{1, "actual VM and recorded active-attempt volumes", record.ID, started, time.Now(), node, vmid, volumes})
	if err != nil {
		return fail("VM ownership evidence could not be retained")
	}
	proof := aj.Verification{EvidenceID: evidenceID, EvidenceJSON: evidenceJSON, Complete: true, OwnershipVerified: true}
	return &managedVMObservation{Node: node, VMID: vmid, Config: cfg, Volumes: volumes, Verification: proof}, nil
}

func managedVMVolumeDevice(key string) bool {
	for _, prefix := range []string{"scsi", "virtio", "sata", "ide", "efidisk", "tpmstate", "unused"} {
		if suffix, ok := strings.CutPrefix(key, prefix); ok && suffix != "" {
			for _, c := range suffix {
				if c < '0' || c > '9' {
					return false
				}
			}
			return true
		}
	}
	return false
}

func saveManagedVMOwnership(handle *aj.Handle, observation *managedVMObservation) error {
	if handle == nil || observation == nil || !observation.Verification.OwnershipVerified {
		return fmt.Errorf("verified VM ownership required")
	}
	record := handle.Record()
	record.Verifications = append(record.Verifications, observation.Verification)
	if record.State == aj.ReconciliationRequired {
		settled := true
		for i := range record.Steps {
			step := &record.Steps[i]
			if step.Attempt == record.ActiveAttempt() && step.State != aj.Observed {
				settled = false
				break
			}
		}
		if settled {
			record.State = aj.Observed
			record.Reason = ""
		}
	}
	return handle.Save(record)
}

// The continuation receives the held generation lock and its original plan. It
// must revalidate current boundaries before executing any remaining mutation.
type managedVMContinuation func(context.Context, Deps, *createVMParsedArgs, *StoragePlacementSelection, *aj.Journal, *aj.Handle, *StorageAllocationPlan, *managedVMObservation) (any, error)

func resumeManagedVM(ctx context.Context, deps Deps, parsed *createVMParsedArgs, selection *StoragePlacementSelection, journal *aj.Journal, lookedUp aj.Record, args []json.RawMessage, continuation managedVMContinuation) (result any, retErr error) {
	fingerprint, err := storageCallerIntentFingerprint("create_vm", args)
	if err != nil {
		return nil, err
	}
	handle, err := journal.AcquireVM(ctx, parsed.agentID, aj.Intent{IntentFingerprint: fingerprint})
	if err != nil {
		return nil, cpierrors.Cloud("existing VM generation cannot be resumed: caller intent or journal authority differs")
	}
	defer func() { retErr = errors.Join(retErr, handle.Close()) }()
	record := handle.Record()
	if !handle.Resumed || record.ID != lookedUp.ID {
		return nil, cpierrors.Cloud("VM generation changed during recovery; audit required")
	}
	plan, err := activeStorageAllocationPlan(record)
	if err != nil {
		return nil, err
	}
	if managedVMAttemptClosed(record) {
		if continuation == nil {
			return nil, cpierrors.Cloud("closed VM attempt requires frozen-plan retry executor")
		}
		return continuation(ctx, deps, parsed, selection, journal, handle, plan, nil)
	}
	var observed *managedVMObservation
	if len(record.Steps) == 0 {
		nodes, err := managedVMClusterNodes(ctx, deps)
		if err != nil {
			return nil, err
		}
		audit, err := AuditStorageAllocations(ctx, deps, journal, nodes)
		if err != nil {
			return nil, err
		}
		hash := sha256.Sum256([]byte(record.AgentID))
		for _, evidence := range audit.Evidence {
			if evidence.AllocationID == record.ID || evidence.Kind == "vm" && evidence.AgentSHA256 == hex.EncodeToString(hash[:]) {
				return nil, cpierrors.Cloud("unsubmitted VM generation has remote provenance; audit required")
			}
		}
		proof, err := storageAllocationVerification(audit, map[string]any{"operation": "unsubmitted VM generation inspection", "allocation_id": record.ID, "outcome": "no mutation intent and no remote allocation provenance"})
		if err != nil {
			return nil, err
		}
		proof.AbsenceVerified = true
		record.Verifications = append(record.Verifications, proof)
		if err := handle.Save(record); err != nil {
			return nil, err
		}
	} else {
		observed, err = observeManagedVMRecord(ctx, deps, journal, record)
		if err != nil {
			return nil, err
		}
		if err := saveManagedVMOwnership(handle, observed); err != nil {
			return nil, err
		}
		if record.State == aj.ReadyToReturn || record.State == aj.Adopted {
			return []any{record.CID, buildResponseNetworks(parsed.networks, planNICs(parsed.networks), observed.Config)}, nil
		}
	}
	if continuation == nil {
		return nil, cpierrors.Cloud("allocation %s requires a frozen-plan executor to continue", record.ID)
	}
	return continuation(ctx, deps, parsed, selection, journal, handle, plan, observed)
}

// resumeExistingManagedVM runs before current policy resolution. Removing sets
// cannot hide an existing generation or send its retry down legacy creation.
func resumeExistingManagedVM(ctx context.Context, deps Deps, args []json.RawMessage, parsed *createVMParsedArgs) (result any, found bool, retErr error) {
	if deps.Config == nil || deps.Config.StoragePlacementNamespace == "" && deps.Config.StorageAllocationJournalDir == "" {
		return nil, false, nil
	}
	if parsed == nil {
		return nil, false, fmt.Errorf("existing allocation lookup requires parsed VM arguments")
	}
	nodes, err := managedVMClusterNodes(ctx, deps)
	if err != nil {
		return nil, false, err
	}
	journal, err := openStorageAllocationJournal(ctx, deps, nodes)
	if err != nil {
		return nil, false, err
	}
	defer func() { retErr = errors.Join(retErr, journal.Close()) }()
	record, found, err := journal.InspectVMContext(ctx, parsed.agentID)
	if err != nil || !found {
		return nil, found, err
	}
	continuation := func(ctx context.Context, deps Deps, parsed *createVMParsedArgs, _ *StoragePlacementSelection, journal *aj.Journal, handle *aj.Handle, plan *StorageAllocationPlan, observed *managedVMObservation) (any, error) {
		current, err := ResolveStoragePlacementSelectors(deps.Config, "create_vm", parsed.cloudPropsMap, parsed.cloudProps.EphemeralDiskSizeMB > 0)
		if err != nil {
			return nil, fmt.Errorf("recorded VM continuation is blocked by current storage policy")
		}
		return continueManagedVM(ctx, deps, parsed, current, journal, handle, plan, observed)
	}
	result, err = resumeManagedVM(ctx, deps, parsed, nil, journal, record, args, continuation)
	return result, true, err
}
