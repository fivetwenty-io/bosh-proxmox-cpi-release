package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

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
	// VMScanIssues is the subset of Issues that left the VM scan incomplete.
	// A gate that needs only the VM scan reports these and not the storage
	// issues that it tolerates.
	VMScanIssues []string    `json:"vm_scan_issues"`
	Records      []aj.Record `json:"records"`
	// SkippedDisabledStorages lists image-capable storages that PVE reports
	// as disabled and that no retained record ever named. PVE refuses to
	// list a disabled storage's content, and neither PVE nor this CPI can
	// allocate on one, so leaving it uninspected does not weaken the absence
	// proof. The list is disclosed so an operator who re-enables one of them
	// knows it was never audited.
	SkippedDisabledStorages []string `json:"skipped_disabled_storages"`
	// ObservedMoves lists the VMs, disks, and parkers the audit found on a
	// node other than the recorded one and accepted as moves on shared
	// storage. An accepted move raises no conflict. The audit decides every
	// move afresh on each call and never records one in the journal.
	ObservedMoves []StorageAllocationMove `json:"observed_moves"`
	// briefs maps a conflict to the short form a gate error leads with, which
	// names the VM or volume before the allocation. It is not serialized,
	// because the conflict itself is the durable record.
	briefs map[string]string
	// listed holds the volids that each (node, storage) listing returned. A
	// listing that failed, or that returned a malformed entry, is absent,
	// because it cannot prove that a volume is present on that node.
	listed map[storageAuditTarget]map[string]bool
	// vms is the inventory the VM scan kept for the move rules.
	vms []storageAuditVM
	// pending holds the node mismatches that wait for the move rules, which
	// need the storage listings and so run after correlation.
	pending []storageAuditPendingMove
	// unsettled holds the sightings that no record of the first journal read
	// explains, which wait for the journal read that follows the scan.
	unsettled []storageAuditUnsettled
	// claims maps each volid a VM configuration holds to what PVE says about
	// the volume at each holder, so a listing can tell a reused name from
	// the volume a record once carried under it.
	claims map[string][]storageAuditClaim
}

// storageAuditClaim is what one VM's configuration says about a volume it
// holds. PVE reuses a freed name such as vm-2353-disk-1 for the next disk a
// VM receives, so a record's historical volids can name a volume that now
// belongs to another allocation. The drive serial and the VM's disk
// provenance identify the volume at its location; the name does not.
type storageAuditClaim struct {
	node string
	vmid int
	// cdrom marks a media=cdrom entry. Many VMs mount one ISO at once, so a
	// CD-ROM never counts as a second writer of a volume.
	cdrom bool
	// serial is the drive's stable-ID serial, or "" when it carries none.
	serial string
	// allocations are the audited allocations whose disk provenance on this
	// VM names the volume.
	allocations []string
}

// attributes reports whether the claim decides that the volume belongs to
// record, and whether the claim decides it at all. The serial is the disk's
// identity, so it decides first. Provenance decides for a drive without a
// serial. A volume with neither is undecided, and the caller falls back to
// the record's volids, so an unattributed volume that bears a name the
// record carried still counts as the record's.
func (c storageAuditClaim) attributes(record aj.Record) (holds, decided bool) {
	if c.serial != "" && record.DiskToken != "" {
		return c.serial == record.DiskToken, true
	}
	if len(c.allocations) > 0 {
		return slices.Contains(c.allocations, record.ID), true
	}
	return false, false
}

// storageAuditSharedReferences raises a conflict for every volume of ours
// that two or more VMs reference at once, because each of them can write the
// one disk. The check reads VM configurations rather than the attribution
// rules, so it catches a hand-edited slot or a copied serial that those rules
// would rule out as a holder of any record, and it counts every holder of such
// a volume, ours or not. A volume that only other guests share, such as a
// shared-disk cluster's data disk or a passed-through device, is not ours to
// judge and raises nothing. A reused name never trips it, because PVE reuses a
// name only after the volume that carried it has gone. On shared storage any
// two referencing VMs share the volume; on node-local storage, or storage the
// definitions do not name, only VMs on the same node do, because the same
// volid on two nodes names two volumes. CD-ROM entries are skipped.
func storageAuditSharedReferences(result *StorageAllocationAudit, stores map[string]pve.StorageInfo, knownVolumes map[string][]aj.Record, namespace string) {
	tokens := storageAuditLiveDiskTokens(result.Records)
	for volume, claims := range result.claims {
		if !storageAuditVolumeOurs(volume, claims, knownVolumes, tokens, namespace) {
			continue
		}
		shared := false
		if storage, _, err := pve.ParseDiskCID(volume); err == nil {
			shared = stores[storage].IsShared()
		}
		groups := map[string]map[int]string{}
		for _, claim := range claims {
			if claim.cdrom {
				continue
			}
			key := claim.node
			if shared {
				key = ""
			}
			if groups[key] == nil {
				groups[key] = map[int]string{}
			}
			groups[key][claim.vmid] = claim.node
		}
		for _, holders := range groups {
			if len(holders) < 2 {
				continue
			}
			subjects := make([]string, 0, len(holders))
			for vmid, node := range holders {
				subjects = append(subjects, fmt.Sprintf("VM %d on %s", vmid, node))
			}
			sort.Strings(subjects)
			result.addConflict(fmt.Sprintf("volume %s is referenced by more than one VM, so each can write the same disk: %s", volume, strings.Join(subjects, ", ")), fmt.Sprintf("volume %s is attached to %d VMs: %s", volume, len(holders), strings.Join(subjects, ", ")))
		}
	}
}

// storageAuditLiveDiskTokens collects the disk tokens of disk records that
// are neither deleted nor cleaned.
func storageAuditLiveDiskTokens(records []aj.Record) map[string]bool {
	tokens := map[string]bool{}
	for recordIndex := range records {
		record := records[recordIndex]
		if record.Kind == allocationKindDisk && record.DiskToken != "" && record.State != aj.Deleted && record.State != aj.Cleaned {
			tokens[record.DiskToken] = true
		}
	}
	return tokens
}

// storageAuditVolumeOurs reports whether a volume belongs to the audited
// namespace. It does when its name carries the namespace locator, when a
// record that is neither deleted nor cleaned names it, or when a holder ties
// it to one of our allocations through disk provenance or through a live disk
// token as its drive serial.
func storageAuditVolumeOurs(volume string, claims []storageAuditClaim, knownVolumes map[string][]aj.Record, tokens map[string]bool, namespace string) bool {
	if len(knownVolumes[volume]) > 0 {
		return true
	}
	if namespace != "" {
		ours := pve.AllocationNamespaceLocator(namespace)
		if locator, _, ok := pve.ParseAllocationVolumeID(volume); ok && locator == ours {
			return true
		}
		if locator, _, ok := pve.ParseManagedEphemeralVolumeID(volume); ok && locator == ours {
			return true
		}
	}
	return slices.ContainsFunc(claims, func(claim storageAuditClaim) bool {
		return len(claim.allocations) > 0 || tokens[claim.serial]
	})
}

// storageAuditNameDisowned reports whether the VMs holding a listed volume
// all attribute it to allocations other than record. Only a holder that
// sees the same physical volume counts: any holder on shared storage, and on
// node-local storage only a holder on the listing's own node. A volume no
// VM holds, or one with any undecided or agreeing holder, is not disowned.
func storageAuditNameDisowned(claims []storageAuditClaim, record aj.Record, node string, shared bool) bool {
	disowned := false
	for _, claim := range claims {
		if !shared && claim.node != node {
			continue
		}
		holds, decided := claim.attributes(record)
		if !decided || holds {
			return false
		}
		disowned = true
	}
	return disowned
}

// storageAuditVM is what the VM scan kept of one VM's configuration.
type storageAuditVM struct {
	node string
	vmid int
	// volumes maps each volume slot to its volid. It is nil when a slot held
	// a reference that managedVMConfigVolumes does not recognize.
	volumes map[string]string
	// hasVMState reports a top-level vmstate key, which a hibernated VM
	// carries; vmstate is the volid it names.
	hasVMState bool
	vmstate    string
	// disks maps each disk allocation whose provenance this VM carries in the
	// audited namespace to the volid that provenance names.
	disks map[string]string
}

// storageAuditPendingMove is a node mismatch that the move rules resolve. A
// refused move raises conflict, followed by the reason it was refused.
type storageAuditPendingMove struct {
	kind     string
	evidence StorageAllocationEvidence
	// recorded is the node the disk provenance names. VM mismatches take
	// their recorded nodes from the journal record instead.
	recorded        string
	conflict, brief string
}

// storageAuditUnsettled is a sighting with no record, or with no step, in the
// journal read that preceded the scan. A sibling create can write its record
// and step and then clone its VM or create its volume while the scan runs, so
// its conflict waits for a second journal read that follows the scan.
type storageAuditUnsettled struct {
	evidence        StorageAllocationEvidence
	conflict, brief string
}

// storageAuditUnreadGuest is a listed VM whose configuration read failed,
// with the issue the failure raises.
type storageAuditUnreadGuest struct {
	vmid  int
	issue string
}

// markVMScanIncomplete records an issue that leaves the VM scan, and so the
// whole audit, incomplete.
func markVMScanIncomplete(result *StorageAllocationAudit, issue string) {
	result.Complete = false
	result.VMScanComplete = false
	result.Issues = append(result.Issues, issue)
	result.VMScanIssues = append(result.VMScanIssues, issue)
}

// addConflict records a conflict and the brief a gate error shows for it.
func (r *StorageAllocationAudit) addConflict(conflict, brief string) {
	r.Conflicts = append(r.Conflicts, conflict)
	if brief == "" {
		return
	}
	if r.briefs == nil {
		r.briefs = map[string]string{}
	}
	r.briefs[conflict] = brief
}

// brief returns the short form of a finding, or the finding itself.
func (r StorageAllocationAudit) brief(finding string) string {
	if brief, ok := r.briefs[finding]; ok {
		return brief
	}
	return finding
}

// storageAuditSubject names what one evidence entry observed.
func storageAuditSubject(evidence StorageAllocationEvidence) string {
	switch {
	case evidence.VolumeID == "":
		return fmt.Sprintf("VM %d on %s", evidence.VMID, evidence.Node)
	case evidence.VMID > 0:
		return fmt.Sprintf("volume %s on %s (VM %d)", evidence.VolumeID, evidence.Node, evidence.VMID)
	default:
		return fmt.Sprintf("volume %s on %s", evidence.VolumeID, evidence.Node)
	}
}

// storageAuditField bounds free text read from a VM description, such as a
// provenance key or node, before a finding repeats it.
func storageAuditField(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '?'
		}
		return r
	}, strings.ToValidUTF8(s, "?"))
	if len(s) > 64 {
		cut := 64
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "..."
	}
	return s
}

// Reason codes name why a retained step did not match an observation.
const (
	storageAuditReasonExternal           = "external"
	storageAuditReasonNotInStep          = "not_in_step"
	storageAuditReasonStorageUnknown     = "storage_unknown"
	storageAuditReasonStorageChanged     = "storage_changed"
	storageAuditReasonBackingChanged     = "backing_changed"
	storageAuditReasonNodeLocalElsewhere = "node_local_elsewhere"
	storageAuditReasonNodeMismatch       = "node_mismatch"
)

// storageAuditReasonCloseness orders reasons by how close the step came to
// matching, so a finding can report the nearest miss.
var storageAuditReasonCloseness = map[string]int{
	storageAuditReasonExternal:           1,
	storageAuditReasonNotInStep:          2,
	storageAuditReasonStorageUnknown:     3,
	storageAuditReasonStorageChanged:     4,
	storageAuditReasonBackingChanged:     5,
	storageAuditReasonNodeLocalElsewhere: 6,
	storageAuditReasonNodeMismatch:       6,
}

// storageAuditMiss keeps the retained step that came closest to matching.
type storageAuditMiss struct {
	reason string
	step   aj.Step
}

func (m *storageAuditMiss) consider(step aj.Step, reason string) {
	if m.reason == "" || storageAuditReasonCloseness[reason] > storageAuditReasonCloseness[m.reason] {
		m.reason, m.step = reason, step
	}
}

// describe renders the nearest miss as "recorded <node> (<reason>)". A
// record with no steps at all reports not_in_step against no node.
func (m storageAuditMiss) describe(record aj.Record) string {
	reason := m.reason
	if reason == "" {
		reason = storageAuditReasonNotInStep
	}
	recorded := m.step.Target.Node
	if recorded == "" {
		recorded = "no node"
	}
	if reason == storageAuditReasonNodeMismatch {
		if plan, err := activeStorageAllocationPlan(record); err == nil && len(plan.HANodes) > 0 {
			recorded += ", HA nodes " + strings.Join(plan.HANodes, ",")
		}
	}
	return fmt.Sprintf("recorded %s (%s)", recorded, reason)
}

// storageAuditMoveHint names the usual cause of a node mismatch.
func storageAuditMoveHint(reason string) string {
	if reason == storageAuditReasonNodeMismatch || reason == storageAuditReasonNodeLocalElsewhere {
		return "; a migration outside BOSH is the usual cause"
	}
	return ""
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
	return auditStorageAllocationRecords(ctx, deps, records, journal.List, nodes)
}

// AuditStorageAllocationEnrollment scans for existing namespace provenance
// before explicit first enrollment. It never constructs an empty journal.
func AuditStorageAllocationEnrollment(ctx context.Context, deps Deps, nodes []string) (StorageAllocationAudit, error) {
	return auditStorageAllocationRecords(ctx, deps, nil, nil, nodes)
}

// auditStorageAllocationRecords audits the cluster against records. When
// reread is set, it reads the journal again after the scan to settle the
// sightings that records could not explain; nil leaves records as the only
// read.
func auditStorageAllocationRecords(ctx context.Context, deps Deps, records []aj.Record, reread func() ([]aj.Record, error), nodes []string) (StorageAllocationAudit, error) {
	result := StorageAllocationAudit{Complete: true, VMScanComplete: true, StartedAt: time.Now(), Records: records}
	if ctx == nil || deps.Config == nil || deps.PVE == nil {
		return result, fmt.Errorf("allocation audit requires configuration and client")
	}
	if deps.PVE.QEMU() == nil || deps.PVE.Nodes() == nil || deps.PVE.ClusterStorage() == nil {
		return result, fmt.Errorf("allocation audit requires read services")
	}
	visibility, canVerifyVisibility := deps.PVE.(pve.StorageAuditVisibilityReader)
	if !canVerifyVisibility {
		markVMScanIncomplete(&result, "cluster-wide VM and storage audit visibility is unproven: PVE client cannot prove audit visibility (CPI defect)")
	} else if err := visibility.StorageAuditVisibility(ctx); err != nil {
		markVMScanIncomplete(&result, "cluster-wide VM and storage audit visibility is unproven: "+pve.DescribeAuditError(err))
	}
	byID, historical, knownVolumes := storageAuditRecordIndex(records)
	diskHolders, err := auditStorageVMs(ctx, deps, records, knownVolumes, &result)
	if err != nil {
		return result, err
	}
	stores := auditStorageDefinitions(ctx, deps, records, &result)
	storageAuditSharedReferences(&result, stores, knownVolumes, deps.Config.StoragePlacementNamespace)
	targets := storageAuditTargets(ctx, deps, nodes, historical, stores, &result)
	if err := collectStorageAuditContent(ctx, deps, targets, stores, knownVolumes, &result); err != nil {
		return result, err
	}
	for id, holders := range diskHolders {
		if len(holders) > 1 {
			subjects := make([]string, 0, len(holders))
			for _, holder := range holders {
				subjects = append(subjects, storageAuditSubject(holder))
			}
			sort.Strings(subjects)
			result.addConflict("disk allocation "+id+" has multiple active holders or duplicate stable tokens: "+strings.Join(subjects, ", "), fmt.Sprintf("disk allocation %s has %d holders: %s", id, len(holders), strings.Join(subjects, ", ")))
		}
		result.Evidence = append(result.Evidence, holders...)
	}
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
	namespace := deps.Config.StoragePlacementNamespace
	correlateStorageAuditEvidence(&result, byID, stores, namespace)
	settleStorageAuditRaces(ctx, deps, &result, reread, stores, namespace)
	resolveStorageAuditMoves(ctx, deps, &result, storageAuditMoveIndex{byID: byID, knownVolumes: knownVolumes, stores: stores, namespace: namespace})
	sort.Strings(result.Issues)
	result.Issues = slices.Compact(result.Issues)
	sort.Strings(result.Conflicts)
	result.Conflicts = slices.Compact(result.Conflicts)
	sort.Strings(result.VMScanIssues)
	result.VMScanIssues = slices.Compact(result.VMScanIssues)
	sort.Strings(result.SkippedDisabledStorages)
	result.SkippedDisabledStorages = slices.Compact(result.SkippedDisabledStorages)
	if len(result.Conflicts) > 0 {
		result.Complete = false
	}
	result.CompletedAt = time.Now()
	return result, nil
}

// admitStorageAllocation runs AuditStorageAllocations and admits the result.
// The returned audit carries the same scan (including Records) that sibling
// accounting reads, so a caller never needs a second List. On any error the
// zero audit is returned alongside it, so a failed scan can never be used.
func admitStorageAllocation(ctx context.Context, deps Deps, journal *aj.Journal, nodes []string) (StorageAllocationAudit, error) {
	report, err := AuditStorageAllocations(ctx, deps, journal, nodes)
	if err != nil {
		return StorageAllocationAudit{}, err
	}
	if err := storageAuditGateError(ctx, deps, "storage allocation admission", report, storageAuditGateConflicts); err != nil {
		return StorageAllocationAudit{}, err
	}
	if !report.Complete {
		deps.Log(ctx).Warn("storage provenance inspection incomplete; no historical absence is certified", log.Int("unavailable_observations", len(report.Issues)), log.String("issues", log.ScrubMessage(strings.Join(report.Issues, " | "))))
	}
	return report, nil
}

// admitStorageVMAllocation runs AuditStorageAllocations and admits the result
// for a VM allocation. It returns no audit on purpose: the VM path reads its
// siblings under the journal index lock, which is fresher than this scan, so
// nothing downstream may rank from the audit's records.
func admitStorageVMAllocation(ctx context.Context, deps Deps, journal *aj.Journal, nodes []string, agentID string) error {
	report, err := AuditStorageAllocations(ctx, deps, journal, nodes)
	if err != nil {
		return err
	}
	if err := storageAuditGateError(ctx, deps, "create_vm", report, storageAuditGateVMScan|storageAuditGateConflicts); err != nil {
		return err
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
	// ObservedMoves is omitted when empty, so evidence from an audit that saw
	// no move stays byte-identical to what earlier releases retained.
	evidenceID, evidenceJSON, err := aj.VerificationEvidence(struct {
		Version       int                         `json:"version"`
		StartedAt     time.Time                   `json:"started_at"`
		CompletedAt   time.Time                   `json:"completed_at"`
		Evidence      []StorageAllocationEvidence `json:"evidence"`
		ObservedMoves []StorageAllocationMove     `json:"observed_moves,omitempty"`
		Facts         any                         `json:"facts"`
	}{1, report.StartedAt, report.CompletedAt, report.Evidence, report.ObservedMoves, log.RedactSecrets(facts)})
	if err != nil {
		return aj.Verification{}, err
	}
	return aj.Verification{EvidenceID: evidenceID, Complete: true, EvidenceJSON: evidenceJSON}, nil
}

// storageStepNamesRetentionParker reports whether a step names a retention
// parker rather than the VM record's own guest. Retention moves the ephemeral
// volume onto a parker, and explicit cleanup later deletes it there under the
// delete_disk operation. Both record the parker's VMID as an owned target, and
// records written by 0.8.0 already carry them, so readers classify by kind.
func storageStepNamesRetentionParker(record aj.Record, step aj.Step) bool {
	return strings.HasPrefix(step.Kind, "lifecycle_delete_vm_retain_ephemeral_") || record.Kind == "vm" && strings.HasPrefix(step.Kind, "lifecycle_delete_disk_")
}

// Recorded HA placement permits a VM to move between its original allowed
// nodes. A matching VMID on another node still requires reconciliation. On a
// miss, the second result is the reason code.
func storageAuditVMTargetMatches(record aj.Record, step aj.Step, node string, vmid int) (bool, string) {
	if step.Target.External || storageStepNamesRetentionParker(record, step) {
		return false, storageAuditReasonExternal
	}
	if step.Target.VMID != vmid || vmid <= 0 {
		return false, storageAuditReasonNotInStep
	}
	if step.Target.Node == node {
		return true, ""
	}
	if plan, err := activeStorageAllocationPlan(record); err == nil && slices.Contains(plan.HANodes, node) {
		return true, ""
	}
	return false, storageAuditReasonNodeMismatch
}

// Matching a logical volid is insufficient for node-local storage. The volume,
// backing, and physical node must be corroborated by the same retained step.
// On a miss, the second result is the reason code.
func storageAuditVolumeTargetMatches(record aj.Record, step aj.Step, stores map[string]pve.StorageInfo, node, volume string) (bool, string) {
	if step.Target.External {
		return false, storageAuditReasonExternal
	}
	if step.Target.IntendedVolume != volume && !slices.Contains(step.VolIDs, volume) {
		return false, storageAuditReasonNotInStep
	}
	storage, _, err := pve.ParseDiskCID(volume)
	if err != nil {
		return false, storageAuditReasonStorageUnknown
	}
	current, found := stores[storage]
	if !found {
		return false, storageAuditReasonStorageUnknown
	}
	if step.Target.Storage != "" && step.Target.Storage != storage {
		return false, storageAuditReasonStorageChanged
	}
	expected := step.Target.Backing
	if expected == "" {
		plan, err := activeStorageAllocationPlan(record)
		if err != nil {
			return false, storageAuditReasonStorageUnknown
		}
		old, ok := plan.Definitions[storage]
		if !ok {
			return false, storageAuditReasonStorageUnknown
		}
		expected = old.BackingKey()
	}
	if expected == "" {
		return false, storageAuditReasonStorageUnknown
	}
	if current.BackingKey() != expected {
		return false, storageAuditReasonBackingChanged
	}
	if !current.IsShared() && step.Target.Node != node {
		return false, storageAuditReasonNodeLocalElsewhere
	}
	return true, ""
}

type storageAuditTarget struct{ node, storage string }

// collectAuditDiskHolders counts each drive of one VM as a holder of the
// disk records it belongs to, and returns what the VM says about each volume
// it holds. provenance maps each allocation whose disk provenance this VM
// carries to the volid that provenance names. A drive belongs to a record
// when its serial or, without a serial, the VM's provenance names the
// record. Only a drive that neither identifies falls back to the record's
// volids, because PVE reuses freed names and a historical volid alone would
// count another allocation's disk as a second holder.
func collectAuditDiskHolders(records []aj.Record, knownVolumes map[string][]aj.Record, cfg map[string]any, provenance map[string]string, node string, vmid int, diskHolders map[string][]StorageAllocationEvidence) map[string]storageAuditClaim {
	named := map[string][]string{}
	for id, volume := range provenance {
		named[volume] = append(named[volume], id)
	}
	claims := map[string]storageAuditClaim{}
	for _, drive := range qemu.ParseDisks(cfg) {
		volume := strings.Split(drive, ",")[0]
		serial, _ := pve.StableIDFromDriveOptStr(drive)
		allocations := slices.Sorted(slices.Values(named[volume]))
		claim := storageAuditClaim{node: node, vmid: vmid, cdrom: slices.Contains(strings.Split(drive, ",")[1:], "media=cdrom"), serial: serial, allocations: allocations}
		claims[volume] = claim
		for recordIndex := range records {
			record := records[recordIndex]
			if record.Kind != allocationKindDisk || record.State == aj.Deleted || record.State == aj.Cleaned {
				continue
			}
			holds, decided := claim.attributes(record)
			if !decided {
				holds = slices.ContainsFunc(knownVolumes[volume], func(known aj.Record) bool { return known.ID == record.ID })
			}
			if holds {
				diskHolders[record.ID] = append(diskHolders[record.ID], StorageAllocationEvidence{AllocationID: record.ID, Kind: allocationKindDisk, Node: node, VMID: vmid, VolumeID: volume})
			}
		}
	}
	return claims
}

func collectAuditVMProvenance(result *StorageAllocationAudit, records []aj.Record, namespace, node string, vmid int, description string) {
	marker, found, e := pve.ParseStorageAllocationMarker(description)
	if e != nil {
		markVMScanIncomplete(result, fmt.Sprintf("VM %d has malformed allocation provenance on %s: %s", vmid, node, pve.DescribeAuditError(e)))
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
			if matched, _ := storageAuditVMTargetMatches(record, step, node, vmid); matched && (!found || e != nil || marker.Namespace != namespace || marker.AllocationID != record.ID) {
				var problem string
				switch {
				case e != nil:
					problem = "carries a malformed allocation marker"
				case !found:
					problem = "carries no allocation marker"
				case marker.Namespace != namespace:
					problem = "carries a marker from namespace " + strconv.Quote(storageAuditField(marker.Namespace))
				default:
					problem = "carries the marker of allocation " + marker.AllocationID
				}
				result.addConflict(fmt.Sprintf("recorded VM target for allocation %s lacks matching ownership provenance: VM %d on %s %s", record.ID, vmid, node, problem), fmt.Sprintf("VM %d on %s lacks the marker of allocation %s", vmid, node, record.ID))
			}
		}
	}

}

// collectAuditDiskProvenance records the disk provenance one VM carries and
// returns each audited allocation's volid. A provenance entry that names
// another node waits in pending for the move rules, which need the storage
// listings that the VM scan runs before.
func collectAuditDiskProvenance(result *StorageAllocationAudit, namespace, node string, vmid int, description string) map[string]string {
	// Legacy ParseSentinel deliberately tolerates corruption. Absence audits
	// must first use the strict managed-provenance parser so malformed or
	// duplicated carriers cannot silently disappear from the inventory.
	if _, _, parseErr := pve.FindDiskAllocationProvenance(description, ""); parseErr != nil {
		result.Complete = false
		result.Issues = append(result.Issues, fmt.Sprintf("VM %d has malformed disk provenance on %s: %s", vmid, node, pve.DescribeAuditError(parseErr)))
		return nil
	}
	_, sentinel := pve.ParseSentinel(description)
	keys := map[string]bool{}
	parked := map[string]bool{}
	for _, carrier := range []string{"bosh_parked_disks", "bosh_disk_allocations"} {
		raw, found := sentinel[carrier]
		if !found {
			continue
		}
		var entries map[string]json.RawMessage
		if json.Unmarshal(raw, &entries) != nil || entries == nil {
			result.Complete = false
			result.Issues = append(result.Issues, fmt.Sprintf("VM %d has malformed disk provenance on %s: carrier %s is not an object", vmid, node, carrier))
			continue
		}
		for key := range entries {
			keys[key] = true
			parked[key] = parked[key] || carrier == "bosh_parked_disks"
		}
	}
	disks := map[string]string{}
	for key := range keys {
		entry, found, parseErr := pve.FindDiskAllocationProvenance(description, key)
		if parseErr != nil {
			result.Complete = false
			result.Issues = append(result.Issues, fmt.Sprintf("VM %d has malformed disk provenance on %s: key %q: %s", vmid, node, storageAuditField(key), pve.DescribeAuditError(parseErr)))
			continue
		}
		if found && entry.AllocationNamespace == namespace {
			evidence := StorageAllocationEvidence{AllocationID: entry.AllocationID, Kind: allocationKindDisk, Node: node, VMID: vmid, VolumeID: entry.Volid}
			if entry.Node != node {
				kind := allocationKindDisk
				if parked[key] {
					kind = storageMoveKindParker
				}
				recorded := storageAuditField(entry.Node)
				result.pending = append(result.pending, storageAuditPendingMove{
					kind:     kind,
					evidence: evidence,
					recorded: entry.Node,
					conflict: fmt.Sprintf("disk ownership provenance disagrees with actual holder node: disk allocation %s (volume %s) held by VM %d on %s, provenance names %s; a migration outside BOSH is the usual cause", entry.AllocationID, entry.Volid, vmid, node, recorded),
					brief:    fmt.Sprintf("VM %d on %s holds disk allocation %s, provenance names %s", vmid, node, entry.AllocationID, recorded),
				})
			}
			disks[entry.AllocationID] = entry.Volid
			result.Evidence = append(result.Evidence, evidence)
		}
	}
	return disks
}

func auditStorageVMs(ctx context.Context, deps Deps, records []aj.Record, knownVolumes map[string][]aj.Record, result *StorageAllocationAudit) (map[string][]StorageAllocationEvidence, error) {
	guests, skipped, err := pve.ListGuestsAuthoritativeTolerant(ctx, deps.PVE, deps.Log(ctx))
	if err != nil {
		markVMScanIncomplete(result, "cluster VM enumeration failed: "+pve.DescribeAuditError(err))
	}
	if len(skipped) > 0 {
		markVMScanIncomplete(result, "some cluster nodes could not be inspected: "+strings.Join(skipped, ", ")+" (reported offline by /cluster/status)")
	}
	diskHolders := map[string][]StorageAllocationEvidence{}
	namespace := deps.Config.StoragePlacementNamespace
	var unread []storageAuditUnreadGuest
	for _, guest := range guests {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cfg, e := deps.PVE.QEMU().Config(ctx, guest.Node, guest.VMID)
		if e != nil {
			unread = append(unread, storageAuditUnreadGuest{vmid: guest.VMID, issue: fmt.Sprintf("VM %d configuration could not be inspected on %s: %s", guest.VMID, guest.Node, pve.DescribeAuditError(e))})
			continue
		}
		if cfg == nil {
			markVMScanIncomplete(result, fmt.Sprintf("VM %d configuration could not be inspected on %s: PVE returned an empty configuration", guest.VMID, guest.Node))
			continue
		}
		description := pve.DescriptionFromConfig(cfg)
		collectAuditVMProvenance(result, records, namespace, guest.Node, guest.VMID, description)
		inventory := storageAuditVM{node: guest.Node, vmid: guest.VMID}
		inventory.disks = collectAuditDiskProvenance(result, namespace, guest.Node, guest.VMID, description)
		for volume, claim := range collectAuditDiskHolders(records, knownVolumes, cfg, inventory.disks, guest.Node, guest.VMID, diskHolders) {
			if result.claims == nil {
				result.claims = map[string][]storageAuditClaim{}
			}
			result.claims[volume] = append(result.claims[volume], claim)
		}
		if volumes, err := managedVMConfigVolumes(cfg); err == nil {
			inventory.volumes = volumes
		}
		inventory.vmstate, inventory.hasVMState = storageAuditVMState(cfg)
		result.vms = append(result.vms, inventory)
	}
	if len(unread) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		storageAuditUnreadGuests(ctx, deps, unread, result)
	}

	return diskHolders, nil

}

// storageAuditUnreadGuests skips each unread guest that was deleted after the
// first listing and marks the VM scan incomplete for every other one. Skipping
// a guest claims that it is absent, so the check lists guests again with the
// strict ListGuestsAuthoritative, which fails rather than leave out a node. A
// guest is skipped only when that listing succeeds and no node carries its
// VMID; one listed anywhere, even on another node, keeps its issue.
func storageAuditUnreadGuests(ctx context.Context, deps Deps, unread []storageAuditUnreadGuest, result *StorageAllocationAudit) {
	guests, err := pve.ListGuestsAuthoritative(ctx, deps.PVE, deps.Log(ctx))
	listed := map[int]bool{}
	for _, guest := range guests {
		listed[guest.VMID] = true
	}
	for _, guest := range unread {
		if err == nil && !listed[guest.vmid] {
			continue
		}
		markVMScanIncomplete(result, guest.issue)
	}
}

// storageAuditVMState returns the volid a configuration's vmstate key names.
// A hibernated VM, and a snapshot taken with RAM, carry one.
func storageAuditVMState(cfg map[string]any) (string, bool) {
	value, found := pve.ConfigString(cfg, "vmstate")
	if !found {
		return "", false
	}
	return strings.Split(value, ",")[0], true
}

func auditStorageDefinitions(ctx context.Context, deps Deps, records []aj.Record, result *StorageAllocationAudit) map[string]pve.StorageInfo {
	definitions, err := deps.PVE.ClusterStorage().ListStorage(ctx, nil)
	switch {
	case err != nil:
		result.Complete = false
		result.Issues = append(result.Issues, "storage definitions could not be inspected: "+pve.DescribeAuditError(err))
	case definitions == nil || *definitions == nil:
		result.Complete = false
		result.Issues = append(result.Issues, "storage definitions could not be inspected: PVE returned no storage index")
	}
	stores := map[string]pve.StorageInfo{}
	if definitions != nil && err == nil {
		for index, raw := range *definitions {
			def, e := pve.ParseStorageEntry(raw)
			if e != nil {
				result.Complete = false
				result.Issues = append(result.Issues, "a storage definition was malformed: "+storageAuditDefinitionName(raw, index))
				continue
			}
			if _, duplicate := stores[def.Name]; duplicate {
				result.Complete = false
				result.Issues = append(result.Issues, fmt.Sprintf("storage definitions contain a duplicate identity: storage %q", def.Name))
				continue
			}
			stores[def.Name] = def
		}
	}
	auditHistoricalDefinitions(records, stores, result)

	return stores

}

// auditHistoricalDefinitions compares each retained step's backing and each
// frozen plan definition with the storage definitions PVE reports now.
func auditHistoricalDefinitions(records []aj.Record, stores map[string]pve.StorageInfo, result *StorageAllocationAudit) {
	for recordIndex := range records {
		record := records[recordIndex]
		for stepIndex := range record.Steps {
			step := record.Steps[stepIndex]
			if step.Target.Storage != "" && step.Target.Backing != "" {
				def, found := stores[step.Target.Storage]
				if !found || def.BackingKey() != step.Target.Backing {
					current := "absent"
					if found {
						current = def.BackingKey()
					}
					result.Complete = false
					result.Issues = append(result.Issues, fmt.Sprintf("historical mutation backing for %q changed or disappeared: allocation %s step %s on %s recorded %s, current %s", step.Target.Storage, record.ID, step.ID, step.Target.Node, step.Target.Backing, current))
				}
			}
		}
		plan, decodeErr := activeStorageAllocationPlan(record)
		if decodeErr != nil {
			result.Complete = false
			result.Issues = append(result.Issues, "historical allocation plan could not be decoded: allocation "+record.ID)
			continue
		}
		for id := range plan.Definitions {
			old := plan.Definitions[id]
			current, exists := stores[id]
			if !exists || current.BackingKey() != old.BackingKey() || current.IsShared() != old.IsShared() {
				now := "absent"
				if exists {
					now = fmt.Sprintf("%s shared=%t", current.BackingKey(), current.IsShared())
				}
				result.Complete = false
				result.Issues = append(result.Issues, fmt.Sprintf("historical storage %q changed or disappeared; original backing needs audit: allocation %s recorded %s shared=%t, current %s", id, record.ID, old.BackingKey(), old.IsShared(), now))
			}
		}
	}
}

// storageAuditRecordedBy names the allocations whose steps target storage, as
// " by allocation <id>" or " by allocations <id>, <id>", or "" when the
// records in scope name none.
func storageAuditRecordedBy(records []aj.Record, storage string) string {
	var ids []string
	for recordIndex := range records {
		record := records[recordIndex]
		if slices.ContainsFunc(record.Steps, func(step aj.Step) bool { return step.Target.Storage == storage }) {
			ids = append(ids, record.ID)
		}
	}
	sort.Strings(ids)
	ids = slices.Compact(ids)
	switch len(ids) {
	case 0:
		return ""
	case 1:
		return " by allocation " + ids[0]
	}
	return " by allocations " + strings.Join(ids, ", ")
}

// storageAuditDefinitionName names a storage definition that failed to parse,
// by its storage name when that much decodes and by its position otherwise.
func storageAuditDefinitionName(raw json.RawMessage, index int) string {
	var probe struct {
		Storage string `json:"storage"`
	}
	if json.Unmarshal(raw, &probe) == nil && probe.Storage != "" {
		return fmt.Sprintf("storage %q", storageAuditField(probe.Storage))
	}
	return fmt.Sprintf("entry %d has no storage name", index)
}

func storageAuditTargets(ctx context.Context, deps Deps, nodes []string, historical map[string]map[string]bool, stores map[string]pve.StorageInfo, result *StorageAllocationAudit) []storageAuditTarget {
	allNodes := slices.Clone(nodes)
	nodeResponse, nodeErr := deps.PVE.Nodes().ListNodes(ctx)
	switch {
	case nodeErr != nil:
		result.Complete = false
		result.Issues = append(result.Issues, "storage audit node enumeration failed: "+pve.DescribeAuditError(nodeErr))
	case nodeResponse == nil || *nodeResponse == nil:
		result.Complete = false
		result.Issues = append(result.Issues, "storage audit node enumeration failed: PVE returned no node list")
	default:
		for index, raw := range *nodeResponse {
			var n struct {
				Node string `json:"node"`
			}
			if json.Unmarshal(raw, &n) != nil || n.Node == "" {
				result.Complete = false
				result.Issues = append(result.Issues, fmt.Sprintf("storage audit node entry malformed: entry %d has no node name", index))
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
		// A disabled storage cannot be listed or allocated on, so it is
		// skipped and disclosed rather than counted as an inspection failure.
		// A disabled storage that a retained record names is still audited,
		// and the listing failure that follows keeps the audit incomplete.
		if def.Disabled && historical[id] == nil {
			result.SkippedDisabledStorages = append(result.SkippedDisabledStorages, id)
			continue
		}
		for _, node := range allNodes {
			if node != "" && (len(def.Nodes) == 0 || slices.Contains(def.Nodes, node) || historical[id][node]) {
				targets = append(targets, storageAuditTarget{node, id})
			}
		}
	}
	for id := range historical {
		if _, ok := stores[id]; ok {
			continue
		}
		recorded := make([]string, 0, len(historical[id]))
		for node := range historical[id] {
			if node != "" {
				recorded = append(recorded, node)
			}
		}
		sort.Strings(recorded)
		on := "no recorded node"
		if len(recorded) > 0 {
			on = strings.Join(recorded, ", ")
		}
		result.Complete = false
		result.Issues = append(result.Issues, fmt.Sprintf("historical storage %q is absent from definitions: recorded on %s%s", id, on, storageAuditRecordedBy(result.Records, id)))
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].node != targets[j].node {
			return targets[i].node < targets[j].node
		}
		return targets[i].storage < targets[j].storage
	})

	return targets

}

// storageAuditKnownVolume correlates one listed volume with the records whose
// volids name it. It returns the evidence, the issues, and the IDs of the
// records it evidenced. A volume that every VM holding it attributes to
// another allocation is a reused name, not the record's volume, so it is
// neither evidence of that record nor a disagreement with its targets.
func storageAuditKnownVolume(target storageAuditTarget, stores map[string]pve.StorageInfo, records []aj.Record, claims []storageAuditClaim, volume string) ([]StorageAllocationEvidence, []string, map[string]bool) {
	var observations []StorageAllocationEvidence
	var issues []string
	seen := map[string]bool{}
	for recordIndex := range records {
		record := records[recordIndex]
		if storageAuditNameDisowned(claims, record, target.node, stores[target.storage].IsShared()) {
			continue
		}
		matched := false
		var miss storageAuditMiss
		for stepIndex := range record.Steps {
			step := record.Steps[stepIndex]
			match, reason := storageAuditVolumeTargetMatches(record, step, stores, target.node, volume)
			matched = matched || match
			if !match {
				miss.consider(step, reason)
			}
		}
		if !matched {
			issues = append(issues, fmt.Sprintf("known volume %s on storage %q on node %q disagrees with recorded physical target: allocation %s %s", volume, target.storage, target.node, record.ID, miss.describe(record)))
			continue
		}
		if !seen[record.ID] {
			observations = append(observations, StorageAllocationEvidence{AllocationID: record.ID, Kind: record.Kind, Node: target.node, VolumeID: volume})
			seen[record.ID] = true
		}
	}
	return observations, issues, seen
}

// storageAuditContent lists one storage on one node. Beside the evidence and
// issues, it returns every volid the listing held, or nil when the listing
// failed or held a malformed entry and so proves nothing about its content.
func storageAuditContent(ctx context.Context, deps Deps, target storageAuditTarget, stores map[string]pve.StorageInfo, knownVolumes map[string][]aj.Record, claims map[string][]storageAuditClaim, namespace string) ([]StorageAllocationEvidence, map[string]bool, []string) {
	response, e := deps.PVE.Nodes().ListStorageContent(ctx, target.node, target.storage, nil)
	observations := []StorageAllocationEvidence{}
	var issues []string
	listed := map[string]bool{}
	malformed := false
	switch {
	case e != nil:
		issues = append(issues, fmt.Sprintf("storage %q on node %q could not be inspected: %s", target.storage, target.node, pve.DescribeAuditError(e)))
	case response == nil || *response == nil:
		issues = append(issues, fmt.Sprintf("storage %q on node %q could not be inspected: PVE returned no content listing", target.storage, target.node))
	default:
		for index, raw := range *response {
			var item struct {
				VolID string `json:"volid"`
			}
			if json.Unmarshal(raw, &item) != nil || item.VolID == "" || !strings.HasPrefix(item.VolID, target.storage+":") {
				issues = append(issues, fmt.Sprintf("storage %q on node %q returned malformed content: entry %d has no volid on this storage", target.storage, target.node, index))
				malformed = true
				continue
			}
			listed[item.VolID] = true
			known, knownIssues, seenRecords := storageAuditKnownVolume(target, stores, knownVolumes[item.VolID], claims[item.VolID], item.VolID)
			observations = append(observations, known...)
			issues = append(issues, knownIssues...)
			locator, id, ok := pve.ParseAllocationVolumeID(item.VolID)
			if ok && !seenRecords[id] && locator == pve.AllocationNamespaceLocator(namespace) {
				observations = append(observations, StorageAllocationEvidence{AllocationID: id, Kind: allocationKindDisk, Node: target.node, VolumeID: item.VolID})
			}
			locator, id, ok = pve.ParseManagedEphemeralVolumeID(item.VolID)
			if ok && !seenRecords[id] && locator == pve.AllocationNamespaceLocator(namespace) {
				observations = append(observations, StorageAllocationEvidence{AllocationID: id, Kind: "vm", Node: target.node, VolumeID: item.VolID})
			}
		}
		if !malformed {
			return observations, listed, issues
		}
	}

	return observations, nil, issues

}

func collectStorageAuditContent(ctx context.Context, deps Deps, targets []storageAuditTarget, stores map[string]pve.StorageInfo, knownVolumes map[string][]aj.Record, result *StorageAllocationAudit) error {
	// The VM scan has finished, so the workers only read the claims.
	claims := result.claims
	jobs := make(chan storageAuditTarget)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for range min(4, len(targets)) {
		wg.Go(func() {
			for target := range jobs {
				observations, listed, issues := storageAuditContent(ctx, deps, target, stores, knownVolumes, claims, deps.Config.StoragePlacementNamespace)
				mu.Lock()
				result.Evidence = append(result.Evidence, observations...)
				if listed != nil {
					if result.listed == nil {
						result.listed = map[storageAuditTarget]map[string]bool{}
					}
					result.listed[target] = listed
				}
				if len(issues) > 0 {
					result.Complete = false
					result.Issues = append(result.Issues, issues...)
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
		subject := storageAuditSubject(evidence)
		record, ok := byID[evidence.AllocationID]
		if !ok {
			result.unsettled = append(result.unsettled, storageAuditUnsettled{evidence: evidence, conflict: "remote allocation " + evidence.AllocationID + " is missing from retained journal; audit required: observed " + subject, brief: subject + " carries unknown allocation " + evidence.AllocationID})
			continue
		}
		if record.State == aj.Deleted || record.State == aj.Cleaned {
			result.addConflict(fmt.Sprintf("terminal allocation %s still has remote provenance; audit required: %s record, observed %s", record.ID, record.State, subject), fmt.Sprintf("%s belongs to %s allocation %s", subject, record.State, record.ID))
		}
		correlateRetainedAuditEvidence(result, record, stores, evidence)
		if record.Namespace != namespace || record.Kind != evidence.Kind {
			result.addConflict(fmt.Sprintf("remote allocation %s disagrees with journal identity: record kind %s in namespace %q, observed %s evidence %s", evidence.AllocationID, record.Kind, record.Namespace, evidence.Kind, subject), fmt.Sprintf("%s disagrees with the journal identity of allocation %s", subject, evidence.AllocationID))
			continue
		}
		matchedTarget, activeTarget := false, false
		var miss storageAuditMiss
		for stepIndex := range record.Steps {
			step := record.Steps[stepIndex]
			match, reason := false, storageAuditReasonNotInStep
			if evidence.Kind == "vm" && evidence.VolumeID == "" {
				match, reason = storageAuditVMTargetMatches(record, step, evidence.Node, evidence.VMID)
			} else if evidence.VolumeID != "" {
				match, reason = storageAuditVolumeTargetMatches(record, step, stores, evidence.Node, evidence.VolumeID)
			}
			if !match {
				miss.consider(step, reason)
			}
			matchedTarget = matchedTarget || match
			activeTarget = activeTarget || match && step.Attempt == record.ActiveAttempt()
		}
		if matchedTarget && !activeTarget {
			result.addConflict(fmt.Sprintf("allocation %s has resources from a closed attempt: %s, active attempt %d", record.ID, subject, record.ActiveAttempt()), fmt.Sprintf("%s is from a closed attempt of allocation %s", subject, record.ID))
		}

		if !matchedTarget {
			storageAuditTargetMiss(result, record, evidence, miss)
		}
		if evidence.Kind == "vm" && evidence.VolumeID == "" {
			sum := sha256.Sum256([]byte(record.AgentID))
			if evidence.AgentSHA256 != hex.EncodeToString(sum[:]) {
				result.addConflict(fmt.Sprintf("VM allocation %s has inconsistent agent provenance: %s carries a different agent digest", record.ID, subject), fmt.Sprintf("%s carries an agent digest that differs from allocation %s", subject, record.ID))
			}
		}
	}

}

// storageAuditTargetMiss raises the conflict for evidence that no retained
// step matched. A VM sighted only on another node waits for the move rules,
// which need the storage listings, instead of raising its conflict now. A
// sighting that no step names waits for the second journal read, because the
// record may have gained its step during the scan.
func storageAuditTargetMiss(result *StorageAllocationAudit, record aj.Record, evidence StorageAllocationEvidence, miss storageAuditMiss) {
	observed := fmt.Sprintf("volume %s", evidence.VolumeID)
	if evidence.VolumeID == "" {
		observed = fmt.Sprintf("VM %d", evidence.VMID)
	}
	recorded := miss.describe(record)
	conflict := fmt.Sprintf("remote allocation %s (%s) is outside recorded mutation targets: observed on %s, %s%s", record.ID, observed, evidence.Node, recorded, storageAuditMoveHint(miss.reason))
	brief := fmt.Sprintf("%s on %s, %s", observed, evidence.Node, recorded)
	if evidence.Kind == "vm" && evidence.VolumeID == "" && miss.reason == storageAuditReasonNodeMismatch {
		result.pending = append(result.pending, storageAuditPendingMove{kind: "vm", evidence: evidence, conflict: conflict, brief: brief})
		return
	}
	if miss.reason == "" || miss.reason == storageAuditReasonNotInStep {
		result.unsettled = append(result.unsettled, storageAuditUnsettled{evidence: evidence, conflict: conflict, brief: brief})
		return
	}
	result.addConflict(conflict, brief)
}

// settleStorageAuditRaces drops the conflict of each unsettled sighting that a
// record in a second journal read explains, and raises every other one as
// written. The read follows the VM scan and every storage listing, so it sees
// any record a sibling create wrote before it cloned a VM or created a volume
// the scan saw. With nothing unsettled or no reread it reads nothing. A failed
// read settles nothing and adds no issue; the next audit reads afresh.
func settleStorageAuditRaces(ctx context.Context, deps Deps, result *StorageAllocationAudit, reread func() ([]aj.Record, error), stores map[string]pve.StorageInfo, namespace string) {
	unsettled := result.unsettled
	result.unsettled = nil
	byID := map[string]aj.Record{}
	if len(unsettled) > 0 && reread != nil {
		records, err := reread()
		if err != nil {
			deps.Log(ctx).Warn("storage allocation audit could not read the journal again; its unsettled conflicts stand", log.Int("unsettled_conflicts", len(unsettled)), log.ErrScrubbed(err))
			records = nil
		}
		for recordIndex := range records {
			byID[records[recordIndex].ID] = records[recordIndex]
		}
	}
	for _, candidate := range unsettled {
		if record, ok := byID[candidate.evidence.AllocationID]; ok && storageAuditActiveStepExplains(record, stores, namespace, candidate.evidence) {
			continue
		}
		result.addConflict(candidate.conflict, candidate.brief)
	}
}

// storageAuditActiveStepExplains reports whether a live record of this
// namespace and kind owns evidence through a step of its active attempt. A VM
// marker must also carry the record's agent. A Planned step counts, because a
// create saves its step before it clones the VM or creates the volume.
func storageAuditActiveStepExplains(record aj.Record, stores map[string]pve.StorageInfo, namespace string, evidence StorageAllocationEvidence) bool {
	if record.State == aj.Deleted || record.State == aj.Cleaned || record.Namespace != namespace || record.Kind != evidence.Kind {
		return false
	}
	marker := evidence.Kind == "vm" && evidence.VolumeID == ""
	if marker {
		sum := sha256.Sum256([]byte(record.AgentID))
		if evidence.AgentSHA256 != hex.EncodeToString(sum[:]) {
			return false
		}
	}
	for stepIndex := range record.Steps {
		step := record.Steps[stepIndex]
		if step.Attempt != record.ActiveAttempt() {
			continue
		}
		match := false
		if marker {
			match, _ = storageAuditVMTargetMatches(record, step, evidence.Node, evidence.VMID)
		} else if evidence.VolumeID != "" {
			match, _ = storageAuditVolumeTargetMatches(record, step, stores, evidence.Node, evidence.VolumeID)
		}
		if match {
			return true
		}
	}
	return false
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
					if evidence.VolumeID != "" && target.IntendedVolume == evidence.VolumeID {
						matched, _ := storageAuditVolumeTargetMatches(record, aj.Step{Target: target}, stores, evidence.Node, evidence.VolumeID)
						retained = retained || matched
					}
				}
			}
			break
		}
		if !retained {
			subject := storageAuditSubject(evidence)
			result.addConflict("VM-deleted allocation "+record.ID+" has provenance outside retained artifacts: observed "+subject, subject+" is outside the retained artifacts of allocation "+record.ID)
		}
	}

}
