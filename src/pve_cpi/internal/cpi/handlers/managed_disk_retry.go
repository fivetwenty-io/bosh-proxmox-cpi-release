package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	inv "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
	pveerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

type managedDiskRejected struct {
	volume string
	fields []string
}

func (*managedDiskRejected) Error() string {
	return "persistent allocation request rejected before execution"
}

// Only PVE's typed schema-validation rejection is a known unexecuted request.
// Message matching without the typed HTTP response is deliberately insufficient.
func managedDiskValidationRejection(err error) ([]string, bool) {
	var api *pveerrors.APIError
	if !errors.As(err, &api) || api.HTTPCode != 400 || len(api.Errors) == 0 || strings.TrimSuffix(strings.ToLower(api.Message), ".") != "parameter verification failed" {
		return nil, false
	}
	fields := make([]string, 0, len(api.Errors))
	for key := range api.Errors {
		switch key {
		case "filename", "size", "format", metadataKeyVMID:
			fields = append(fields, key)
		default:
			return nil, false
		}
	}
	sort.Strings(fields)
	return fields, true
}

func (m *managedDiskRequest) execute(ctx context.Context, handle *aj.Handle) (any, error) {
	if handle == nil {
		return nil, cpierrors.Cloud("create_disk: allocation ownership is required")
	}
	if handle.Resumed {
		return nil, storageAllocationUncertain(handle, "independent disk requests do not resume prior allocations")
	}
	active, err := activeStorageAllocationPlan(handle.Record())
	if err != nil {
		return nil, err
	}
	if handle.Record().Kind != "disk" || handle.Record().ID != m.id || active.AllocationKey != m.id || active.PolicyFingerprint != m.plan.PolicyFingerprint {
		return nil, storageAllocationUncertain(handle, "persistent plan identity check")
	}
	budget := m.budget
	if budget <= 0 {
		budget = 1
	}
	for attempt := 0; attempt < budget; attempt++ {
		result, err := m.executeAttempt(ctx, handle)
		var rejected *managedDiskRejected
		if !errors.As(err, &rejected) {
			return result, err
		}
		if attempt+1 >= budget {
			return nil, storageAllocationUncertain(handle, "validated rejection retry budget exhausted")
		}
		if err = m.retryRejected(ctx, handle, rejected); err != nil {
			return nil, storageAllocationUncertain(handle, "validated rejection absence audit or replanning")
		}
	}
	return nil, cpierrors.Cloud("create_disk: managed attempt budget exhausted")
}

func (m *managedDiskRequest) retryRejected(ctx context.Context, handle *aj.Handle, rejected *managedDiskRejected) error {
	if m.journal == nil || m.iterator == nil {
		return fmt.Errorf("retry requires retained journal and frozen planning inputs")
	}
	target := m.plan.Targets[0]
	exists, err := managedVolumePresent(ctx, m.deps, target.Node, rejected.volume)
	if err != nil || exists {
		return fmt.Errorf("failed allocation absence unproven")
	}
	report, err := AuditStorageAllocations(ctx, m.deps, m.journal, m.inventory.Nodes())
	if err != nil || !report.Complete || len(report.Conflicts) > 0 {
		return fmt.Errorf("failed allocation historical audit incomplete")
	}
	for _, evidence := range report.Evidence {
		if evidence.AllocationID == m.id {
			return fmt.Errorf("failed allocation has retained resources")
		}
	}
	verification, err := storageAllocationVerification(report, map[string]any{"operation": "Storage.CreateVolume", "outcome": "request_validation_rejected_before_execution", "http_code": 400, "validation_fields": rejected.fields, "allocation_id": m.id, "expected_volume": rejected.volume, "exact_volume_absence": true})
	if err != nil {
		return err
	}
	verification.AbsenceVerified = true
	verification.ArtifactDispositionVerified = true
	proof := aj.AttemptVerification{Verification: verification, OutcomesKnown: true, NoSubmissionVerified: true}
	if m.failedTargets == nil {
		m.failedTargets = map[string]bool{}
	}
	m.failedTargets[target.Node+"\x00"+target.StorageID] = true
	snapshot, err := m.collector.Refresh(ctx, m.inventory)
	if err != nil {
		return err
	}
	request := m.iterator.req
	request.Inventory = snapshot
	if m.hint != "" {
		node, e := managedDiskHintNode(ctx, m.deps, m.hint)
		if e != nil {
			return e
		}
		request.Groups = []StoragePlanNodeGroup{{AZ: m.az, Nodes: []string{node}}}
	}
	request.OnCandidateRejected = m.deps.storageCandidateRejectionObserver(ctx, m.selection)
	iterator, err := NewStoragePlanIterator(request)
	if err != nil {
		return err
	}
	var plan *StorageAllocationPlan
	for {
		plan, err = iterator.Next(ctx)
		if err != nil {
			return err
		}
		t := plan.Targets[0]
		if !m.failedTargets[t.Node+"\x00"+t.StorageID] {
			break
		}
	}
	record := handle.Record()
	if plan.PolicyFingerprint != record.Intent.PolicyFingerprint {
		return fmt.Errorf("retry changed original policy")
	}
	payload, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	next := aj.AttemptPlan{Version: aj.AttemptVersion, PolicyFingerprint: record.Intent.PolicyFingerprint, FrozenInputsFingerprint: record.Intent.FrozenInputsFingerprint, PlanVersion: plan.Version, Plan: payload}
	if err := handle.BeginAttempt(next, proof); err != nil {
		return err
	}
	m.deps.recordStoragePlacement(ctx, m.selection, "fallback")
	ledger := inv.NewLedger()
	for chargeIndex := range plan.Charges {
		charge := &plan.Charges[chargeIndex]
		ledger, err = ledger.WithPlanned(snapshot, charge.Charge)
		if err != nil {
			return err
		}
	}
	m.plan = plan
	m.inventory = snapshot
	m.iterator = iterator
	m.ledger = ledger
	return nil
}
