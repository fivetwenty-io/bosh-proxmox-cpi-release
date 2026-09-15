package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
)

// openStorageAllocationJournal verifies cluster continuity without creating or
// enrolling an authority. Historical provenance is audited by admission/recovery
// before allocating an identity; opening alone never authorizes a PVE mutation.
func openStorageAllocationJournal(ctx context.Context, deps Deps, nodes []string) (*allocationjournal.Journal, error) {
	if deps.Config == nil || deps.PVE == nil {
		return nil, cpierrors.Cloud("storage allocation requires configuration and PVE client")
	}
	if err := deps.Config.ValidateStoragePlacementAllocation(); err != nil {
		return nil, err
	}
	identity, err := pve.ObserveStorageClusterIdentity(ctx, deps.PVE.Nodes(), nodes)
	if err != nil {
		return nil, err
	}
	return allocationjournal.Open(deps.Config.StorageAllocationJournalDir, deps.Config.StoragePlacementNamespace, identity.ID())
}

// storageJournalIntent keeps caller identity separate from changing global policy.
// Credential values are excluded before hashing; request data is never persisted.
func storageJournalIntent(method string, args []json.RawMessage, selection *StoragePlacementSelection, inventory *storageinventory.Snapshot, plan *StorageAllocationPlan) (allocationjournal.Intent, error) {
	if selection == nil || selection.Policy == nil || inventory == nil || plan == nil || plan.Version != 1 {
		return allocationjournal.Intent{}, fmt.Errorf("storage journal requires a supported complete allocation plan")
	}
	intentFP, err := storageCallerIntentFingerprint(method, args)
	if err != nil {
		return allocationjournal.Intent{}, err
	}
	memberships := map[string][]string{}
	definitions := map[string]pve.StorageInfo{}
	addID := func(id string) error {
		def, ok := inventory.Definition(id)
		if !ok {
			return fmt.Errorf("frozen storage definition %q missing", id)
		}
		sort.Strings(def.Nodes)
		definitions[id] = def
		return nil
	}
	names := storageJournalSetNames(selection)
	for name := range names {
		members, ok := inventory.Members(name)
		if !ok {
			return allocationjournal.Intent{}, fmt.Errorf("frozen set %q missing", name)
		}
		sort.Strings(members)
		memberships[name] = members
		for _, id := range members {
			if err := addID(id); err != nil {
				return allocationjournal.Intent{}, err
			}
		}
	}
	for _, members := range plan.CapacityDomains {
		for _, id := range members {
			if err := addID(id); err != nil {
				return allocationjournal.Intent{}, err
			}
		}
	}
	for id := range plan.Definitions {
		if err := addID(id); err != nil {
			return allocationjournal.Intent{}, err
		}
	}
	for rangeIndex98 := range plan.Targets {
		if err := addID(plan.Targets[rangeIndex98].StorageID); err != nil {
			return allocationjournal.Intent{}, err
		}
		if plan.Targets[rangeIndex98].Source != nil {
			if err := addID(plan.Targets[rangeIndex98].Source.StorageID); err != nil {
				return allocationjournal.Intent{}, err
			}
		}
	}
	frozenFP, err := allocationjournal.Fingerprint(struct {
		Nodes             []string
		Memberships       map[string][]string
		Definitions       map[string]pve.StorageInfo
		PolicyFingerprint string
	}{inventory.Nodes(), memberships, definitions, plan.PolicyFingerprint})
	if err != nil {
		return allocationjournal.Intent{}, err
	}
	payload, err := json.Marshal(plan)
	if err != nil {
		return allocationjournal.Intent{}, err
	}
	return allocationjournal.Intent{IntentFingerprint: intentFP, PolicyFingerprint: plan.PolicyFingerprint, FrozenInputsFingerprint: frozenFP, PlanVersion: plan.Version, Plan: payload}, nil
}

func activeStorageAllocationPlan(record allocationjournal.Record) (*StorageAllocationPlan, error) {
	active := record.ActivePlan()
	if active.PlanVersion != 1 {
		return nil, cpierrors.Cloud("allocation %s uses an unsupported plan version; audit required", record.ID)
	}
	d := json.NewDecoder(bytes.NewReader(active.Plan))
	d.DisallowUnknownFields()
	var plan StorageAllocationPlan
	if err := d.Decode(&plan); err != nil {
		return nil, cpierrors.Cloud("allocation %s has invalid plan evidence; audit required", record.ID)
	}
	if err := d.Decode(new(any)); err != io.EOF || plan.Version != active.PlanVersion || plan.Namespace != record.Namespace || plan.PolicyFingerprint != active.PolicyFingerprint {
		return nil, cpierrors.Cloud("allocation %s plan identity disagrees with journal; audit required", record.ID)
	}
	return &plan, nil
}

// siblingStorageAllocationPlan decodes another allocation's active plan for
// sibling accounting. It deliberately differs from activeStorageAllocationPlan
// in two ways, because it reads a record we do not own.
//
// It decodes without DisallowUnknownFields. Every create now decodes every
// in-flight sibling, so a strict decode here would mean that any field a later
// release adds to the plan breaks every create an older binary attempts while a
// newer sibling record is still non-terminal. It still requires a trailing
// io.EOF, so malformed trailing bytes fail rather than pass unnoticed.
//
// It also skips the namespace and policy fingerprint comparisons. Those checks
// audit the identity of a plan against the record that owns it, and a sibling
// is not ours to audit. The owner's own decode through
// activeStorageAllocationPlan stays strict and is not affected by this one.
func siblingStorageAllocationPlan(record allocationjournal.Record) (*StorageAllocationPlan, error) {
	active := record.ActivePlan()
	if active.PlanVersion != 1 {
		return nil, cpierrors.Cloud("allocation %s uses an unsupported plan version; audit required", record.ID)
	}
	d := json.NewDecoder(bytes.NewReader(active.Plan))
	var plan StorageAllocationPlan
	if err := d.Decode(&plan); err != nil {
		return nil, cpierrors.Cloud("allocation %s has invalid plan evidence; audit required", record.ID)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, cpierrors.Cloud("allocation %s has invalid plan evidence; audit required", record.ID)
	}
	return &plan, nil
}

// storageMutationIntent must succeed before the corresponding API call. A
// planned record after restart remains uncertain even when no UPID was saved.
func storageMutationIntent(handle *allocationjournal.Handle, kind string, target allocationjournal.Target, charges []storageinventory.ChargeRecord, parameters ...json.RawMessage) (string, error) {
	if handle == nil {
		return "", fmt.Errorf("mutation requires allocation ownership")
	}
	if len(parameters) > 1 {
		return "", fmt.Errorf("mutation accepts one concrete parameter object")
	}
	record := handle.Record()
	id := fmt.Sprintf("attempt-%d-step-%d", record.ActiveAttempt(), len(record.Steps))
	step := allocationjournal.Step{ID: id, Attempt: record.ActiveAttempt(), Kind: kind, Target: target, State: allocationjournal.Planned}
	if len(parameters) == 1 && len(parameters[0]) != 0 {
		canonical, err := allocationjournal.MutationParameters(parameters[0])
		if err != nil {
			return "", err
		}
		step.Parameters = canonical
	}
	for rangeIndex150 := range charges {
		if charges[rangeIndex150].Charge.Bytes > math.MaxInt64 {
			return "", fmt.Errorf("journal charge exceeds supported byte range")
		}
		step.Charges = append(step.Charges, allocationjournal.Charge{Backing: charges[rangeIndex150].CapacityKey, Domain: charges[rangeIndex150].DomainKey, PlannedBytes: int64(charges[rangeIndex150].Charge.Bytes), OutstandingBytes: int64(charges[rangeIndex150].Charge.Bytes)})
	}
	record.Steps = append(record.Steps, step)
	if record.State == allocationjournal.Observed {
		record.State = allocationjournal.Planned
	}
	if err := handle.Save(record); err != nil {
		return "", err
	}
	return id, nil
}

func storageMutationSubmitted(handle *allocationjournal.Handle, id, upid string) error {
	if upid == "" {
		return fmt.Errorf("submitted mutation requires task identity")
	}
	record := handle.Record()
	for i := range record.Steps {
		if record.Steps[i].ID == id && record.Steps[i].Attempt == record.ActiveAttempt() {
			record.Steps[i].State = allocationjournal.Submitted
			record.Steps[i].UPID = upid
			if record.State != allocationjournal.VMDeletedRetained {
				record.State = allocationjournal.Submitted
			}
			return handle.Save(record)
		}
	}
	return fmt.Errorf("active mutation step not found")
}

// storageMutationObserved records readback proven by the executor. acquired is
// true only after all resources represented by the step's charges are observed.
func storageMutationObserved(handle *allocationjournal.Handle, id string, volumes []string, acquired bool) error {
	record := handle.Record()
	for i := range record.Steps {
		if record.Steps[i].ID != id || record.Steps[i].Attempt != record.ActiveAttempt() {
			continue
		}
		step := &record.Steps[i]
		step.State = allocationjournal.Observed
		for _, volume := range volumes {
			found := false
			for _, old := range step.VolIDs {
				found = found || old == volume
			}
			if !found {
				step.VolIDs = append(step.VolIDs, volume)
			}
		}
		if acquired {
			for k := range step.Charges {
				step.Charges[k].AcquiredBytes = step.Charges[k].PlannedBytes
				step.Charges[k].OutstandingBytes = 0
			}
		}
		if record.State != allocationjournal.VMDeletedRetained {
			record.State = allocationjournal.Observed
		}
		return handle.Save(record)
	}
	return fmt.Errorf("active mutation step not found")
}

// storageAllocationUncertain deliberately does not chain an upstream retriable
// error or its response text. The allocation ID points to retained audit evidence.
func storageAllocationUncertain(handle *allocationjournal.Handle, phase string) error {
	if handle == nil {
		return cpierrors.Cloud("storage allocation requires reconciliation at %s", phase)
	}
	record := handle.Record()
	if record.State != allocationjournal.VMDeletedRetained {
		record.State = allocationjournal.ReconciliationRequired
	}
	record.Reason = "outcome requires reconciliation at " + phase
	if err := handle.Save(record); err != nil {
		return cpierrors.Cloud("allocation %s requires reconciliation at %s; journal persistence also failed", record.ID, phase)
	}
	return cpierrors.Cloud("allocation %s requires reconciliation at %s; no alternate allocation was attempted", record.ID, phase)
}

// storageCallerIntentFingerprint permits active-generation lookup before planning.
func storageCallerIntentFingerprint(method string, args []json.RawMessage) (string, error) {
	if method != "create_vm" && method != "create_disk" {
		return "", fmt.Errorf("unsupported allocation method")
	}
	canonical := make([]any, len(args))
	for i, arg := range args {
		d := json.NewDecoder(bytes.NewReader(arg))
		d.UseNumber()
		if err := d.Decode(&canonical[i]); err != nil {
			return "", fmt.Errorf("invalid allocation argument %d", i)
		}
		if err := d.Decode(new(any)); err != io.EOF {
			return "", fmt.Errorf("trailing allocation argument %d", i)
		}
	}
	intentFP, err := allocationjournal.Fingerprint(struct {
		Method string
		Args   any
	}{method, log.RedactSecrets(canonical)})
	return intentFP, err
}

func storageJournalSetNames(selection *StoragePlacementSelection) map[string]bool {
	names := map[string]bool{}
	for _, name := range []string{selection.FuturePersistentSet} {
		if name != "" {
			names[name] = true
		}
	}
	for _, role := range []*StorageRoleSelection{selection.Root, selection.Ephemeral, selection.Persistent} {
		if role == nil {
			continue
		}
		for _, name := range []string{role.SetName, role.BoundaryName} {
			if name != "" {
				names[name] = true
			}
		}
	}
	return names
}
