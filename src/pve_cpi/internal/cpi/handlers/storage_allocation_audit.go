package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

// StorageAllocationEvidence is an observation, not proof permitting deletion.
// Locators and markers must agree with the journal and actual physical target.
type StorageAllocationEvidence struct {
	AllocationID string `json:"allocation_id"`
	Kind         string `json:"kind"`
	Node         string `json:"node"`
	VMID         int    `json:"vmid,omitempty"`
	VolumeID     string `json:"volume_id,omitempty"`
	AgentSHA256  string `json:"agent_sha256,omitempty"`
}

// StorageAllocationAudit reports partial observations explicitly. A caller must
// require Complete before certifying historical absence, recovery or cleanup.
type StorageAllocationAudit struct {
	StartedAt      time.Time                   `json:"started_at"`
	CompletedAt    time.Time                   `json:"completed_at"`
	Complete       bool                        `json:"complete"`
	VMScanComplete bool                        `json:"vm_scan_complete"`
	Evidence       []StorageAllocationEvidence `json:"evidence"`
	Issues         []string                    `json:"issues"`
	Conflicts      []string                    `json:"conflicts"`
	Records        []aj.Record                 `json:"records"`
}

// AuditStorageAllocations reads current definitions and retained historical
// targets, including stores outside current regex membership. It never writes
// journals, reserves identities, resumes transfers, or submits PVE operations.
func AuditStorageAllocations(ctx context.Context, deps Deps, journal *aj.Journal, nodes []string) (StorageAllocationAudit, error) {
	if journal == nil {
		return StorageAllocationAudit{}, fmt.Errorf("allocation audit requires an enrolled journal")
	}
	records, err := journal.List()
	if err != nil {
		return StorageAllocationAudit{}, err
	}
	return auditStorageAllocationRecords(ctx, deps, records, nodes)
}

// AuditStorageAllocationEnrollment scans for existing namespace provenance
// before explicit first enrollment. It never constructs an empty journal.
func AuditStorageAllocationEnrollment(ctx context.Context, deps Deps, nodes []string) (StorageAllocationAudit, error) {
	return auditStorageAllocationRecords(ctx, deps, nil, nodes)
}

func auditStorageAllocationRecords(ctx context.Context, deps Deps, records []aj.Record, nodes []string) (StorageAllocationAudit, error) {
	result := StorageAllocationAudit{Complete: true, VMScanComplete: true, StartedAt: time.Now(), Records: records}
	if ctx == nil || deps.Config == nil || deps.PVE == nil {
		return result, fmt.Errorf("allocation audit requires configuration and client")
	}
	if deps.PVE.QEMU() == nil || deps.PVE.Nodes() == nil || deps.PVE.ClusterStorage() == nil {
		return result, fmt.Errorf("allocation audit requires read services")
	}
	visibility, canVerifyVisibility := deps.PVE.(pve.StorageAuditVisibilityReader)
	if !canVerifyVisibility || visibility.StorageAuditVisibility(ctx) != nil {
		result.Complete = false
		result.VMScanComplete = false
		result.Issues = append(result.Issues, "cluster-wide VM and storage audit visibility is unproven")
	}
	byID, historical, knownVolumes := storageAuditRecordIndex(records)
	diskHolders, err := auditStorageVMs(ctx, deps, records, knownVolumes, &result)
	if err != nil {
		return result, err
	}
	stores := auditStorageDefinitions(ctx, deps, records, &result)
	targets := storageAuditTargets(ctx, deps, nodes, historical, stores, &result)
	if err := collectStorageAuditContent(ctx, deps, targets, stores, knownVolumes, &result); err != nil {
		return result, err
	}
	for id, holders := range diskHolders {
		if len(holders) > 1 {
			result.Conflicts = append(result.Conflicts, "disk allocation "+id+" has multiple active holders or duplicate stable tokens")
		}
		result.Evidence = append(result.Evidence, holders...)
	}
	correlateStorageAuditEvidence(&result, byID, stores, deps.Config.StoragePlacementNamespace)
	sort.Slice(result.Evidence, func(i, j int) bool {
		a, b := result.Evidence[i], result.Evidence[j]
		if a.AllocationID != b.AllocationID {
			return a.AllocationID < b.AllocationID
		}
		if a.Node != b.Node {
			return a.Node < b.Node
		}
		if a.VMID != b.VMID {
			return a.VMID < b.VMID
		}
		return a.VolumeID < b.VolumeID
	})
	result.Evidence = slices.Compact(result.Evidence)
	sort.Strings(result.Issues)
	result.Issues = slices.Compact(result.Issues)
	sort.Strings(result.Conflicts)
	result.Conflicts = slices.Compact(result.Conflicts)
	if len(result.Conflicts) > 0 {
		result.Complete = false
	}
	result.CompletedAt = time.Now()
	return result, nil
}

func admitStorageAllocation(ctx context.Context, deps Deps, journal *aj.Journal, nodes []string) error {
	report, err := AuditStorageAllocations(ctx, deps, journal, nodes)
	if err != nil {
		return err
	}
	if len(report.Conflicts) > 0 {
		return cpierrors.Cloud("storage allocation admission: %s", strings.Join(report.Conflicts, "; "))
	}
	if !report.Complete {
		deps.Log(ctx).Warn("storage provenance inspection incomplete; no historical absence is certified", log.Int("unavailable_observations", len(report.Issues)))
	}
	return nil
}
func admitStorageVMAllocation(ctx context.Context, deps Deps, journal *aj.Journal, nodes []string, agentID string) error {
	report, err := AuditStorageAllocations(ctx, deps, journal, nodes)
	if err != nil {
		return err
	}
	if !report.VMScanComplete || len(report.Conflicts) > 0 {
		return cpierrors.Cloud("VM allocation admission requires complete VM provenance inspection and consistent retained history")
	}
	sum := sha256.Sum256([]byte(agentID))
	digest := hex.EncodeToString(sum[:])
	for _, evidence := range report.Evidence {
		if evidence.Kind == "vm" && evidence.AgentSHA256 == digest {
			return cpierrors.Cloud("agent allocation %s already has remote VM provenance; resume or audit it before creating another generation", evidence.AllocationID)
		}
	}
	return nil
}

// storageAllocationVerification retains nonsecret observations and explicit
// executor facts. Callers set ownership/absence flags only after their separate
// target-specific proof succeeds; a complete inventory alone proves neither.
func storageAllocationVerification(report StorageAllocationAudit, facts map[string]any) (aj.Verification, error) {
	if !report.Complete || !report.VMScanComplete || len(report.Conflicts) > 0 || report.StartedAt.IsZero() || report.CompletedAt.Before(report.StartedAt) {
		return aj.Verification{}, fmt.Errorf("complete consistent allocation audit required for durable verification")
	}
	allowed := map[string]bool{allocationEvidenceOperationField: true, "outcome": true, "http_code": true, "validation_fields": true, allocationEvidenceIDField: true, "expected_volume": true, "exact_volume_absence": true, resourceTypeNode: true, metadataKeyVMID: true, "owned_volumes": true, "task_verdicts": true}
	for key := range facts {
		if !allowed[key] {
			return aj.Verification{}, fmt.Errorf("unrecognized allocation verification fact")
		}
	}
	evidenceID, evidenceJSON, err := aj.VerificationEvidence(struct {
		Version     int                         `json:"version"`
		StartedAt   time.Time                   `json:"started_at"`
		CompletedAt time.Time                   `json:"completed_at"`
		Evidence    []StorageAllocationEvidence `json:"evidence"`
		Facts       any                         `json:"facts"`
	}{1, report.StartedAt, report.CompletedAt, report.Evidence, log.RedactSecrets(facts)})
	if err != nil {
		return aj.Verification{}, err
	}
	return aj.Verification{EvidenceID: evidenceID, Complete: true, EvidenceJSON: evidenceJSON}, nil
}

// Recorded HA placement permits a VM to move between its original allowed
// nodes. A matching VMID on another node still requires reconciliation.
func storageAuditVMTargetMatches(record aj.Record, step aj.Step, node string, vmid int) bool {
	if step.Target.External || strings.HasPrefix(step.Kind, "lifecycle_delete_vm_retain_ephemeral_") || step.Target.VMID != vmid || vmid <= 0 {
		return false
	}
	if step.Target.Node == node {
		return true
	}
	plan, err := activeStorageAllocationPlan(record)
	return err == nil && slices.Contains(plan.HANodes, node)
}

// Matching a logical volid is insufficient for node-local storage. The volume,
// backing, and physical node must be corroborated by the same retained step.
func storageAuditVolumeTargetMatches(record aj.Record, step aj.Step, stores map[string]pve.StorageInfo, node, volume string) bool {
	if step.Target.External || step.Target.IntendedVolume != volume && !slices.Contains(step.VolIDs, volume) {
		return false
	}
	storage, _, err := pve.ParseDiskCID(volume)
	if err != nil {
		return false
	}
	current, found := stores[storage]
	if !found {
		return false
	}
	if step.Target.Storage != "" && step.Target.Storage != storage {
		return false
	}
	expected := step.Target.Backing
	if expected == "" {
		plan, err := activeStorageAllocationPlan(record)
		if err != nil {
			return false
		}
		old, ok := plan.Definitions[storage]
		if !ok {
			return false
		}
		expected = old.BackingKey()
	}
	if expected == "" || current.BackingKey() != expected {
		return false
	}
	return current.IsShared() || step.Target.Node == node
}

type storageAuditTarget struct{ node, storage string }

func collectAuditDiskHolders(records []aj.Record, knownVolumes map[string][]aj.Record, cfg map[string]any, node string, vmid int, diskHolders map[string][]StorageAllocationEvidence) {
	for recordIndex := range records {
		record := records[recordIndex]
		if record.Kind != allocationKindDisk || record.State == aj.Deleted || record.State == aj.Cleaned {
			continue
		}
		for _, drive := range qemu.ParseDisks(cfg) {
			volume := strings.Split(drive, ",")[0]
			serial, serialFound := pve.StableIDFromDriveOptStr(drive)
			matched := serialFound && serial == record.DiskToken
			for knownIndex := range knownVolumes[volume] {
				known := knownVolumes[volume][knownIndex]
				matched = matched || known.ID == record.ID
			}
			if matched {
				diskHolders[record.ID] = append(diskHolders[record.ID], StorageAllocationEvidence{AllocationID: record.ID, Kind: allocationKindDisk, Node: node, VMID: vmid, VolumeID: volume})
			}
		}
	}

}

func collectAuditVMProvenance(result *StorageAllocationAudit, records []aj.Record, namespace, node string, vmid int, description string) {
	marker, found, e := pve.ParseStorageAllocationMarker(description)
	if e != nil {
		result.Complete = false
		result.VMScanComplete = false
		result.Issues = append(result.Issues, fmt.Sprintf("VM %d has malformed allocation provenance", vmid))
	} else if found && marker.Namespace == namespace {
		result.Evidence = append(result.Evidence, StorageAllocationEvidence{AllocationID: marker.AllocationID, Kind: "vm", Node: node, VMID: vmid, AgentSHA256: marker.AgentSHA256})
	}
	for recordIndex := range records {
		record := records[recordIndex]
		if record.Kind != "vm" || record.State == aj.Deleted || record.State == aj.Cleaned {
			continue
		}
		for stepIndex := range record.Steps {
			step := record.Steps[stepIndex]
			if storageAuditVMTargetMatches(record, step, node, vmid) && (!found || e != nil || marker.Namespace != namespace || marker.AllocationID != record.ID) {
				result.Conflicts = append(result.Conflicts, "recorded VM target for allocation "+record.ID+" lacks matching ownership provenance")
			}
		}
	}

}

func collectAuditDiskProvenance(result *StorageAllocationAudit, namespace, node string, vmid int, description string) {
	// Legacy ParseSentinel deliberately tolerates corruption. Absence audits
	// must first use the strict managed-provenance parser so malformed or
	// duplicated carriers cannot silently disappear from the inventory.
	if _, _, parseErr := pve.FindDiskAllocationProvenance(description, ""); parseErr != nil {
		result.Complete = false
		result.Issues = append(result.Issues, fmt.Sprintf("VM %d has malformed disk provenance", vmid))
		return
	}
	_, sentinel := pve.ParseSentinel(description)
	keys := map[string]bool{}
	for _, carrier := range []string{"bosh_parked_disks", "bosh_disk_allocations"} {
		raw, found := sentinel[carrier]
		if !found {
			continue
		}
		var entries map[string]json.RawMessage
		if json.Unmarshal(raw, &entries) != nil || entries == nil {
			result.Complete = false
			result.Issues = append(result.Issues, fmt.Sprintf("VM %d has malformed disk provenance", vmid))
			continue
		}
		for key := range entries {
			keys[key] = true
		}
	}
	for key := range keys {
		entry, found, parseErr := pve.FindDiskAllocationProvenance(description, key)
		if parseErr != nil {
			result.Complete = false
			result.Issues = append(result.Issues, fmt.Sprintf("VM %d has malformed disk provenance", vmid))
			continue
		}
		if found && entry.AllocationNamespace == namespace {
			if entry.Node != node {
				result.Conflicts = append(result.Conflicts, "disk ownership provenance disagrees with actual holder node")
			}
			result.Evidence = append(result.Evidence, StorageAllocationEvidence{AllocationID: entry.AllocationID, Kind: allocationKindDisk, Node: node, VMID: vmid, VolumeID: entry.Volid})
		}
	}
}

func auditStorageVMs(ctx context.Context, deps Deps, records []aj.Record, knownVolumes map[string][]aj.Record, result *StorageAllocationAudit) (map[string][]StorageAllocationEvidence, error) {
	guests, skipped, err := pve.ListGuestsAuthoritativeTolerant(ctx, deps.PVE, deps.Log(ctx))
	if err != nil {
		result.Complete = false
		result.VMScanComplete = false
		result.Issues = append(result.Issues, "cluster VM enumeration failed")
	}
	if len(skipped) > 0 {
		result.Complete = false
		result.VMScanComplete = false
		result.Issues = append(result.Issues, "some cluster nodes could not be inspected")
	}
	diskHolders := map[string][]StorageAllocationEvidence{}
	namespace := deps.Config.StoragePlacementNamespace
	for _, guest := range guests {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cfg, e := deps.PVE.QEMU().Config(ctx, guest.Node, guest.VMID)
		if e != nil || cfg == nil {
			result.Complete = false
			result.VMScanComplete = false
			result.Issues = append(result.Issues, fmt.Sprintf("VM %d configuration could not be inspected", guest.VMID))
			continue
		}
		collectAuditDiskHolders(records, knownVolumes, cfg, guest.Node, guest.VMID, diskHolders)
		description := pve.DescriptionFromConfig(cfg)
		collectAuditVMProvenance(result, records, namespace, guest.Node, guest.VMID, description)
		collectAuditDiskProvenance(result, namespace, guest.Node, guest.VMID, description)
	}

	return diskHolders, nil

}

func auditStorageDefinitions(ctx context.Context, deps Deps, records []aj.Record, result *StorageAllocationAudit) map[string]pve.StorageInfo {
	definitions, err := deps.PVE.ClusterStorage().ListStorage(ctx, nil)
	if err != nil || definitions == nil || *definitions == nil {
		result.Complete = false
		result.Issues = append(result.Issues, "storage definitions could not be inspected")
	}
	stores := map[string]pve.StorageInfo{}
	if definitions != nil && err == nil {
		for _, raw := range *definitions {
			def, e := pve.ParseStorageEntry(raw)
			if e != nil {
				result.Complete = false
				result.Issues = append(result.Issues, "a storage definition was malformed")
				continue
			}
			if _, duplicate := stores[def.Name]; duplicate {
				result.Complete = false
				result.Issues = append(result.Issues, "storage definitions contain a duplicate identity")
				continue
			}
			stores[def.Name] = def
		}
	}
	for recordIndex := range records {
		record := records[recordIndex]
		for stepIndex := range record.Steps {
			step := record.Steps[stepIndex]
			if step.Target.Storage != "" && step.Target.Backing != "" {
				def, found := stores[step.Target.Storage]
				if !found || def.BackingKey() != step.Target.Backing {
					result.Complete = false
					result.Issues = append(result.Issues, fmt.Sprintf("historical mutation backing for %q changed or disappeared", step.Target.Storage))
				}
			}
		}
		plan, decodeErr := activeStorageAllocationPlan(record)
		if decodeErr != nil {
			result.Complete = false
			result.Issues = append(result.Issues, "historical allocation plan could not be decoded")
			continue
		}
		for id := range plan.Definitions {
			old := plan.Definitions[id]
			current, exists := stores[id]
			if !exists || current.BackingKey() != old.BackingKey() || current.IsShared() != old.IsShared() {
				result.Complete = false
				result.Issues = append(result.Issues, fmt.Sprintf("historical storage %q changed or disappeared; original backing needs audit", id))
			}
		}
	}

	return stores

}

func storageAuditTargets(ctx context.Context, deps Deps, nodes []string, historical map[string]map[string]bool, stores map[string]pve.StorageInfo, result *StorageAllocationAudit) []storageAuditTarget {
	allNodes := slices.Clone(nodes)
	nodeResponse, nodeErr := deps.PVE.Nodes().ListNodes(ctx)
	if nodeErr != nil || nodeResponse == nil || *nodeResponse == nil {
		result.Complete = false
		result.Issues = append(result.Issues, "storage audit node enumeration failed")
	} else {
		for _, raw := range *nodeResponse {
			var n struct {
				Node string `json:"node"`
			}
			if json.Unmarshal(raw, &n) != nil || n.Node == "" {
				result.Complete = false
				result.Issues = append(result.Issues, "storage audit node entry malformed")
				continue
			}
			allNodes = append(allNodes, n.Node)
		}
	}
	for _, perNode := range historical {
		for node := range perNode {
			allNodes = append(allNodes, node)
		}
	}
	sort.Strings(allNodes)
	allNodes = slices.Compact(allNodes)
	targets := []storageAuditTarget{}
	for id := range stores {
		def := stores[id]
		if !strings.Contains(","+def.Content+",", ",images,") && !strings.Contains(","+def.Content+",", ",iso,") && historical[id] == nil {
			continue
		}
		for _, node := range allNodes {
			if node != "" && (len(def.Nodes) == 0 || slices.Contains(def.Nodes, node) || historical[id][node]) {
				targets = append(targets, storageAuditTarget{node, id})
			}
		}
	}
	for id := range historical {
		if _, ok := stores[id]; !ok {
			result.Complete = false
			result.Issues = append(result.Issues, fmt.Sprintf("historical storage %q is absent from definitions", id))
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].node != targets[j].node {
			return targets[i].node < targets[j].node
		}
		return targets[i].storage < targets[j].storage
	})

	return targets

}

func storageAuditContent(ctx context.Context, deps Deps, target storageAuditTarget, stores map[string]pve.StorageInfo, knownVolumes map[string][]aj.Record, namespace string) ([]StorageAllocationEvidence, string) {
	response, e := deps.PVE.Nodes().ListStorageContent(ctx, target.node, target.storage, nil)
	observations := []StorageAllocationEvidence{}
	issue := ""
	if e != nil || response == nil || *response == nil {
		issue = fmt.Sprintf("storage %q on node %q could not be inspected", target.storage, target.node)
	} else {
		for _, raw := range *response {
			var item struct {
				VolID string `json:"volid"`
			}
			if json.Unmarshal(raw, &item) != nil || item.VolID == "" || !strings.HasPrefix(item.VolID, target.storage+":") {
				issue = fmt.Sprintf("storage %q on node %q returned malformed content", target.storage, target.node)
				continue
			}
			seenRecords := map[string]bool{}
			for recordIndex := range knownVolumes[item.VolID] {
				record := knownVolumes[item.VolID][recordIndex]
				matched := false
				for stepIndex := range record.Steps {
					step := record.Steps[stepIndex]
					matched = matched || storageAuditVolumeTargetMatches(record, step, stores, target.node, item.VolID)
				}
				if !matched {
					issue = fmt.Sprintf("known volume on %q/%q disagrees with recorded physical target", target.node, target.storage)
					continue
				}
				if !seenRecords[record.ID] {
					observations = append(observations, StorageAllocationEvidence{AllocationID: record.ID, Kind: record.Kind, Node: target.node, VolumeID: item.VolID})
					seenRecords[record.ID] = true
				}
			}
			locator, id, ok := pve.ParseAllocationVolumeID(item.VolID)
			if ok && !seenRecords[id] && locator == pve.AllocationNamespaceLocator(namespace) {
				observations = append(observations, StorageAllocationEvidence{AllocationID: id, Kind: allocationKindDisk, Node: target.node, VolumeID: item.VolID})
			}
			locator, id, ok = pve.ParseManagedEphemeralVolumeID(item.VolID)
			if ok && !seenRecords[id] && locator == pve.AllocationNamespaceLocator(namespace) {
				observations = append(observations, StorageAllocationEvidence{AllocationID: id, Kind: "vm", Node: target.node, VolumeID: item.VolID})
			}
		}
	}

	return observations, issue

}

func collectStorageAuditContent(ctx context.Context, deps Deps, targets []storageAuditTarget, stores map[string]pve.StorageInfo, knownVolumes map[string][]aj.Record, result *StorageAllocationAudit) error {
	jobs := make(chan storageAuditTarget)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for range min(4, len(targets)) {
		wg.Go(func() {
			for target := range jobs {
				observations, issue := storageAuditContent(ctx, deps, target, stores, knownVolumes, deps.Config.StoragePlacementNamespace)
				mu.Lock()
				result.Evidence = append(result.Evidence, observations...)
				if issue != "" {
					result.Complete = false
					result.Issues = append(result.Issues, issue)
				}
				mu.Unlock()
			}
		})
	}
dispatch:
	for _, target := range targets {
		select {
		case jobs <- target:
		case <-ctx.Done():
			break dispatch
		}
	}
	close(jobs)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}

	return nil

}

func correlateStorageAuditEvidence(result *StorageAllocationAudit, byID map[string]aj.Record, stores map[string]pve.StorageInfo, namespace string) {
	for _, evidence := range result.Evidence {
		record, ok := byID[evidence.AllocationID]
		if !ok {
			result.Conflicts = append(result.Conflicts, "remote allocation "+evidence.AllocationID+" is missing from retained journal; audit required")
			continue
		}
		if record.State == aj.Deleted || record.State == aj.Cleaned {
			result.Conflicts = append(result.Conflicts, "terminal allocation "+record.ID+" still has remote provenance; audit required")
		}
		correlateRetainedAuditEvidence(result, record, stores, evidence)
		if record.Namespace != namespace || record.Kind != evidence.Kind {
			result.Conflicts = append(result.Conflicts, "remote allocation "+evidence.AllocationID+" disagrees with journal identity")
			continue
		}
		matchedTarget, activeTarget := false, false
		for stepIndex := range record.Steps {
			step := record.Steps[stepIndex]
			match := false
			if evidence.Kind == "vm" && evidence.VolumeID == "" {
				match = storageAuditVMTargetMatches(record, step, evidence.Node, evidence.VMID)
			} else if evidence.VolumeID != "" {
				match = storageAuditVolumeTargetMatches(record, step, stores, evidence.Node, evidence.VolumeID)
			}
			matchedTarget = matchedTarget || match
			activeTarget = activeTarget || match && step.Attempt == record.ActiveAttempt()
		}
		if matchedTarget && !activeTarget {
			result.Conflicts = append(result.Conflicts, "allocation "+record.ID+" has resources from a closed attempt")
		}

		if !matchedTarget {
			result.Conflicts = append(result.Conflicts, "remote allocation "+record.ID+" is outside recorded mutation targets")
		}
		if evidence.Kind == "vm" && evidence.VolumeID == "" {
			sum := sha256.Sum256([]byte(record.AgentID))
			if evidence.AgentSHA256 != hex.EncodeToString(sum[:]) {
				result.Conflicts = append(result.Conflicts, "VM allocation "+record.ID+" has inconsistent agent provenance")
			}
		}
	}

}

func storageAuditRecordIndex(records []aj.Record) (map[string]aj.Record, map[string]map[string]bool, map[string][]aj.Record) {
	byID := map[string]aj.Record{}
	historical := map[string]map[string]bool{}
	knownVolumes := map[string][]aj.Record{}
	for recordIndex := range records {
		record := records[recordIndex]
		byID[record.ID] = record
		for stepIndex := range record.Steps {
			step := record.Steps[stepIndex]
			if !step.Target.External && record.State != aj.Deleted && record.State != aj.Cleaned {
				for _, volume := range append(slices.Clone(step.VolIDs), step.Target.IntendedVolume) {
					if volume != "" {
						knownVolumes[volume] = append(knownVolumes[volume], record)
					}
				}
			}
			if step.Target.Storage != "" {
				if historical[step.Target.Storage] == nil {
					historical[step.Target.Storage] = map[string]bool{}
				}
				historical[step.Target.Storage][step.Target.Node] = true
			}
		}
	}

	return byID, historical, knownVolumes
}

func correlateRetainedAuditEvidence(result *StorageAllocationAudit, record aj.Record, stores map[string]pve.StorageInfo, evidence StorageAllocationEvidence) {
	if record.State == aj.VMDeletedRetained {
		retained := false
		for i := len(record.Verifications) - 1; i >= 0; i-- {
			verification := record.Verifications[i]
			if !verification.VMAbsenceVerified || !verification.ArtifactDispositionVerified || verification.AbsenceVerified {
				continue
			}
			var disposition aj.VMRetentionEvidence
			if json.Unmarshal([]byte(verification.EvidenceJSON), &disposition) == nil {
				for _, target := range disposition.RetainedArtifacts {
					retained = retained || evidence.VolumeID != "" && target.IntendedVolume == evidence.VolumeID && storageAuditVolumeTargetMatches(record, aj.Step{Target: target}, stores, evidence.Node, evidence.VolumeID)
				}
			}
			break
		}
		if !retained {
			result.Conflicts = append(result.Conflicts, "VM-deleted allocation "+record.ID+" has provenance outside retained artifacts")
		}
	}

}
