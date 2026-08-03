// Parker implements the persistent-disk parking mechanism: detached BOSH
// persistent disks are held on scsi0..scsi30 slots of a dedicated never-started
// "parker" VM instead of floating free in PVE storage. This gives each
// unattached disk a PVE-visible owner VM, making accidental deletion harder.
//
// # ParkerConfig construction
//
// Callers assemble ParkerConfig from CPIConfig accessors:
//
//	cfg := pve.ParkerConfig{
//	    VMIDRangeStart: cpiCfg.ParkedDiskVMIDRangeStartValue(),
//	    VMIDRangeEnd:   cpiCfg.ParkedDiskVMIDRangeEndValue(),
//	    DirectorID:     deps.RequestDirectorUUID, // empty = omit director scope tag
//	}
//
// # Tag constants
//
// ParkerTag ("bosh-parker") marks every parker VM. When DirectorID is set a
// second tag "director--<sanitized-id>" is appended (mirrors stemcell provenance
// convention from create_stemcell.go).
//
// # Slot capacity
//
// Each parker VM holds up to 31 disks (scsi0..scsi30). When every existing
// parker on a node is full, a fresh parker VMID is allocated in the same range.
// New parks always reuse the lowest existing parker that still has a free slot
// before creating another parker, so the VMID band is not exhausted one disk
// at a time.
package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// CpiOwnershipTag is the fixed ownership marker stamped on every VM and
// template created by this CPI. It mirrors handlers.ownershipTag ("bosh-cpi")
// and is defined here so the pve package (which cannot import handlers) can
// apply the same marker to parker VMs without an import cycle.
const CpiOwnershipTag = "bosh-cpi"

// ParkerTag is the PVE tag applied to every parker VM. Downstream code
// (set_disk_metadata, snapshot_disk, delete_vm) identifies parker VMs via
// IsParkerVM, which requires both a VMID-range check and this tag.
const ParkerTag = "bosh-parker"

// parkerMaxSlots is the number of scsi slots available on a single parker VM
// (scsi0 through scsi30 inclusive).
const parkerMaxSlots = 31

// ErrNoSlots is returned when all scsi0..scsi30 slots on a parker VM are
// occupied. Callers (ParkDisk) react by creating a second parker VM for the
// same node.
var ErrNoSlots = errors.New("parker VM has no free scsi slots")

// ParkerConfig holds the parker VMID range and optional director scope. Built
// by callers from CPIConfig accessors — see package-level doc comment.
type ParkerConfig struct {
	// VMIDRangeStart is the first VMID in the parker band (inclusive). Must be
	// ≥ 100 and less than VMIDRangeEnd.
	VMIDRangeStart int
	// VMIDRangeEnd is the last VMID in the parker band (inclusive). Must be
	// greater than VMIDRangeStart.
	VMIDRangeEnd int
	// DirectorID is the optional BOSH director identifier. When non-empty a
	// "director--<sanitized-id>" tag is added to newly created parker VMs so
	// operators can distinguish parkers per director in multi-director clusters.
	DirectorID string
	// DiskStorage is the CPI's configured pve.disk_storage pool. When set,
	// createParkerVM passes it to pve.WithStorageScan so parker VMID
	// allocation also scans that storage's volume content, closing the same
	// cross-cluster co-mingling gap WithStorageScan already closes for the
	// VM, disk, and template ranges (see WithStorageScan's doc comment):
	// without this, two independent PVE clusters sharing one storage backend
	// can each allocate a parker at the same VMID, and a later
	// DestroyUnreferencedDisks-driven destroy of one cluster's parker frees
	// the other cluster's parked disks by matching VMID. Empty (the
	// zero-value default) makes the scan a no-op — byte-identical to prior
	// releases for callers that do not set it.
	DiskStorage string
	// NowFunc returns the current time. Nil defaults to time.Now().UTC().
	// Tests inject a fixed clock to assert parked_at values deterministically.
	NowFunc func() time.Time
}

// parkerNow returns the configured clock time or time.Now().UTC() when NowFunc is nil.
func parkerNow(cfg ParkerConfig) time.Time {
	if cfg.NowFunc != nil {
		return cfg.NowFunc()
	}
	return time.Now().UTC()
}

// ---------------------------------------------------------------------------
// Provenance sentinel codec (local to pve package — no handlers import to avoid cycle)
// ---------------------------------------------------------------------------
//
// parseParkerSentinel/renderParkerSentinel are the bosh_parked_disks-specific
// wrapper around the shared ParseSentinel/RenderSentinel codec in sentinel.go
// (also used by the bosh_attached_disks codec in attached_disks.go). Same
// wire format as set_disk_metadata's independent codec; all these codecs
// coexist on one VM description by using distinct top-level JSON keys.

// ParkContext carries per-call attribution for provenance records written on
// park. Fields are optional: zero values are omitted from the sentinel JSON.
// DiskCID should be the encoded CID as the Director knows it (may include a
// metadata suffix). SourceVMCID is the VM the disk was detached from.
type ParkContext struct {
	// DiskCID is the full disk CID passed by the Director (may include encoded
	// metadata suffix). Stored as disk_cid in the provenance entry so disk-audit
	// can cross-reference back to the Director's view of the disk.
	DiskCID string
	// SourceVMCID is the BOSH VM CID from which the disk was detached. Omitted
	// when unknown (e.g. re-park on retry when only the bare volid is available).
	SourceVMCID string
}

// parkerProvEntry is a single parked-disk record stored in the sentinel.
// Optional fields are omitted when empty so the JSON stays minimal.
type parkerProvEntry struct {
	DiskCID     string `json:"disk_cid"`
	SourceVMCID string `json:"source_vm_cid,omitempty"`
	ParkedAt    string `json:"parked_at"`
	Node        string `json:"node"`
	DirectorID  string `json:"director_id,omitempty"`
}

// parseParkerSentinel extracts the description text outside the sentinel
// (nonBOSH) and the current bosh_parked_disks map. Corrupted JSON → fresh
// empty map (sentinel rebuilt from scratch; nonBOSH text preserved).
func parseParkerSentinel(desc string) (nonBOSH string, disks map[string]parkerProvEntry, raw map[string]json.RawMessage) {
	nonBOSH, raw = ParseSentinel(desc)
	disks = make(map[string]parkerProvEntry)

	// Extract our own key.
	if rawDisks, ok := raw["bosh_parked_disks"]; ok {
		_ = json.Unmarshal(rawDisks, &disks) // best-effort; corruption → empty map
		delete(raw, "bosh_parked_disks")     // will be re-serialised below
	}
	return
}

// renderParkerSentinel builds the full description string from the nonBOSH
// prefix, the updated bosh_parked_disks map, and the raw remainder of other
// codec keys. When both disks and raw are empty, returns nonBOSH unchanged
// (no sentinel block emitted — avoids writing an empty <!--BOSH:{}-->).
func renderParkerSentinel(nonBOSH string, disks map[string]parkerProvEntry, raw map[string]json.RawMessage) (string, error) {
	if len(disks) == 0 && len(raw) == 0 {
		return nonBOSH, nil
	}

	// Merge: start from raw (other keys), add our key.
	merged := make(map[string]json.RawMessage, len(raw)+1)
	for k, v := range raw {
		merged[k] = v
	}
	if len(disks) > 0 {
		b, err := json.Marshal(disks)
		if err != nil {
			return "", err
		}
		merged["bosh_parked_disks"] = json.RawMessage(b)
	}

	return RenderSentinel(nonBOSH, merged)
}

// updateParkerProvenance merges a parked-disk entry into the parker VM
// description sentinel and writes it back via UpdateQemuConfig.
//
// Best-effort: any failure is logged at WARN and the function returns nil.
// Park/unpark success is never gated on provenance writes.
//
// Concurrent-park lost-update is acceptable — two CPI processes racing to
// park different disks on the same parker may each overwrite the other's
// provenance entry. The disk itself remains correctly attached; provenance
// is advisory metadata for disk-audit.
func updateParkerProvenance(ctx context.Context, c Client, logger *log.Logger, node string, parkerVMID int, bareVolid string, cfg ParkerConfig, pctx ParkContext) {
	vmidStr := fmt.Sprintf("%d", parkerVMID)

	vmCfg, err := c.QEMU().Config(ctx, node, parkerVMID)
	if err != nil {
		if logger != nil {
			logger.Warn("parker provenance: config fetch failed — provenance not updated",
				log.Int("parker_vmid", parkerVMID),
				log.String("node", node),
				log.String("volid", bareVolid),
				log.Err(err),
			)
		}
		return
	}

	currentDesc := DescriptionFromConfig(vmCfg)

	nonBOSH, disks, rawOther := parseParkerSentinel(currentDesc)

	// disk_cid: prefer the full encoded CID from ParkContext (as the Director
	// knows it); fall back to bareVolid when context is absent.
	diskCIDField := bareVolid
	if pctx.DiskCID != "" {
		diskCIDField = pctx.DiskCID
	}
	entry := parkerProvEntry{
		DiskCID:     diskCIDField,
		SourceVMCID: pctx.SourceVMCID,
		ParkedAt:    parkerNow(cfg).Format(time.RFC3339),
		Node:        node,
		DirectorID:  cfg.DirectorID,
	}
	disks[bareVolid] = entry

	newDesc, marshalErr := renderParkerSentinel(nonBOSH, disks, rawOther)
	if marshalErr != nil {
		if logger != nil {
			logger.Warn("parker provenance: marshal failed — provenance not updated",
				log.Int("parker_vmid", parkerVMID),
				log.String("volid", bareVolid),
				log.Err(marshalErr),
			)
		}
		return
	}

	nodesSvc := c.Nodes()
	if nodesSvc == nil {
		// No nodes service available (e.g. test stub without injection). Skip silently.
		return
	}
	updateErr := nodesSvc.UpdateQemuConfig(ctx, node, vmidStr, &sdknodes.UpdateQemuConfigParams{
		Description: &newDesc,
	})
	if updateErr != nil {
		if logger != nil {
			logger.Warn("parker provenance: UpdateQemuConfig failed — provenance not updated",
				log.Int("parker_vmid", parkerVMID),
				log.String("node", node),
				log.String("volid", bareVolid),
				log.Err(updateErr),
			)
		}
	}
}

// removeParkerProvenance removes the bareVolid entry from the parker VM
// description sentinel. When no entry exists, no API call is made.
//
// Best-effort: any failure is logged at WARN and the function returns nil.
func removeParkerProvenance(ctx context.Context, c Client, logger *log.Logger, node string, parkerVMID int, bareVolid string, _ ParkerConfig) {
	vmidStr := fmt.Sprintf("%d", parkerVMID)

	vmCfg, err := c.QEMU().Config(ctx, node, parkerVMID)
	if err != nil {
		if logger != nil {
			logger.Warn("parker provenance: config fetch failed on remove — provenance not removed",
				log.Int("parker_vmid", parkerVMID),
				log.String("node", node),
				log.String("volid", bareVolid),
				log.Err(err),
			)
		}
		return
	}

	currentDesc := DescriptionFromConfig(vmCfg)

	nonBOSH, disks, rawOther := parseParkerSentinel(currentDesc)

	// Absent entry — nothing to remove; skip the API call.
	if _, exists := disks[bareVolid]; !exists {
		return
	}
	delete(disks, bareVolid)

	newDesc, marshalErr := renderParkerSentinel(nonBOSH, disks, rawOther)
	if marshalErr != nil {
		if logger != nil {
			logger.Warn("parker provenance: marshal failed on remove — provenance not removed",
				log.Int("parker_vmid", parkerVMID),
				log.String("volid", bareVolid),
				log.Err(marshalErr),
			)
		}
		return
	}

	nodesSvc := c.Nodes()
	if nodesSvc == nil {
		// No nodes service available (e.g. test stub without injection). Skip silently.
		return
	}
	updateErr := nodesSvc.UpdateQemuConfig(ctx, node, vmidStr, &sdknodes.UpdateQemuConfigParams{
		Description: &newDesc,
	})
	if updateErr != nil {
		if logger != nil {
			logger.Warn("parker provenance: UpdateQemuConfig failed on remove — provenance not removed",
				log.Int("parker_vmid", parkerVMID),
				log.String("node", node),
				log.String("volid", bareVolid),
				log.Err(updateErr),
			)
		}
	}
}

// parkerTagSanitizeRe removes characters that PVE rejects in tag values.
// PVE tags allow [a-zA-Z0-9._-]; anything else is stripped.
var parkerTagSanitizeRe = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

// sanitizeParkerTagValue strips PVE-illegal tag characters from v. Returns ""
// when the sanitized result is empty (so callers can omit the tag rather than
// emitting a blank one).
func sanitizeParkerTagValue(v string) string {
	return parkerTagSanitizeRe.ReplaceAllString(v, "")
}

// parkerVMName returns the canonical name for a parker VM with the given VMID.
func parkerVMName(vmid int) string {
	return fmt.Sprintf("bosh-parker-%d", vmid)
}

// buildParkerTags returns the semicolon-joined tag string for a new parker VM.
// CpiOwnershipTag ("bosh-cpi") is always first so operators can filter all
// CPI-managed guests by a single tag. ParkerTag follows; "director--<id>" is
// appended when DirectorID is set and sanitizes to a non-empty value (mirrors
// stemcell provenance pattern).
func buildParkerTags(cfg ParkerConfig) string {
	tags := []string{CpiOwnershipTag, ParkerTag}
	if cfg.DirectorID != "" {
		if sd := sanitizeParkerTagValue(cfg.DirectorID); sd != "" {
			tags = append(tags, "director--"+sd)
		}
	}
	return strings.Join(tags, ";")
}

// tagContainsParker reports whether a semicolon-separated PVE tag string
// contains ParkerTag as a whole token. Comparison is case-insensitive to
// tolerate PVE tag normalization.
func tagContainsParker(tagStr string) bool {
	for _, t := range strings.Split(tagStr, ";") {
		if strings.EqualFold(strings.TrimSpace(t), ParkerTag) {
			return true
		}
	}
	return false
}

// IsParkerVM reports whether vmid is a parker VM: both the VMID must fall
// within [cfg.VMIDRangeStart, cfg.VMIDRangeEnd] and tags must contain
// ParkerTag. The range check is fast and avoids a config read; the tag check
// confirms the VM was created as a parker (not an unrelated VM that happens
// to occupy a VMID in the range).
func IsParkerVM(vmid int, tags string, cfg ParkerConfig) bool {
	if vmid < cfg.VMIDRangeStart || vmid > cfg.VMIDRangeEnd {
		return false
	}
	return tagContainsParker(tags)
}

// FindParkerForNode scans cluster VM resources and returns the LOWEST parker
// VMID on node. It does NOT check slot capacity — the returned parker may be
// full. Callers that need a parker with a free slot (e.g. parkDiskOnNode) must
// use ListParkersForNode and iterate, attaching to the first parker that
// accepts the disk. EnsureParker uses this only to detect whether any parker
// already exists before creating one.
//
// Returns (vmid, true, nil) when at least one parker exists; (0, false, nil)
// when none found. Returns (0, false, err) on transport failure.
func FindParkerForNode(ctx context.Context, c Client, node string, cfg ParkerConfig) (int, bool, error) {
	parkers, err := ListParkersForNode(ctx, c, node, cfg)
	if err != nil {
		return 0, false, err
	}
	if len(parkers) == 0 {
		return 0, false, nil
	}
	return parkers[0], true, nil
}

// ListParkersForNode returns all parker VMIDs on node in ascending VMID order.
// It scans cluster resources for VMs in the parker VMID range, then fetches
// each VM's config to confirm the bosh-parker tag. VMs whose config is
// missing (deleted concurrently) are silently skipped.
//
// Returns an empty slice (not an error) when no parkers exist.
func ListParkersForNode(ctx context.Context, c Client, node string, cfg ParkerConfig) ([]int, error) {
	if c == nil {
		return nil, cpierrors.Cloud("ListParkersForNode: client must not be nil")
	}
	if node == "" {
		return nil, cpierrors.Cloud("ListParkersForNode: node must not be empty")
	}
	if cfg.VMIDRangeStart <= 0 || cfg.VMIDRangeEnd <= cfg.VMIDRangeStart {
		return nil, cpierrors.Cloud("ListParkersForNode: invalid VMID range [%d, %d]",
			cfg.VMIDRangeStart, cfg.VMIDRangeEnd)
	}

	used, err := listClusterVMIDs(ctx, c)
	if err != nil {
		return nil, cpierrors.Wrap(err, "ListParkersForNode: list cluster VMIDs")
	}

	var result []int
	for vmid := cfg.VMIDRangeStart; vmid <= cfg.VMIDRangeEnd; vmid++ {
		if _, exists := used[vmid]; !exists {
			continue
		}
		// Confirm this VMID lives on the target node and carries bosh-parker tag.
		vmNode, found, nodeErr := FindVMNodeViaCluster(ctx, c, vmid)
		if nodeErr != nil {
			return nil, cpierrors.WrapAs(nodeErr, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("ListParkersForNode: node lookup for vmid %d", vmid))
		}
		if !found || vmNode != node {
			continue
		}
		cfg2, cfgErr := c.QEMU().Config(ctx, node, vmid)
		if cfgErr != nil {
			if IsNotFound(cfgErr) {
				continue
			}
			return nil, cpierrors.WrapAs(cfgErr, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("ListParkersForNode: config fetch for vmid %d", vmid))
		}
		tagsRaw, _ := cfg2["tags"].(string)
		if tagContainsParker(tagsRaw) {
			result = append(result, vmid)
		}
	}
	return result, nil
}

// createParkerVM allocates a VMID in cfg's parker range and creates a fresh
// parker VM there, retrying past VMID conflicts by re-scanning and adopting
// whichever parker won the race. It is shared by EnsureParker (called after
// its existing-parker short-circuit) and EnsureFreshParker (called directly,
// since its whole contract is "always allocate a NEW parker" — it must never
// short-circuit on an existing one).
//
// The parker VM is created with:
//   - name: "bosh-parker-<vmid>"
//   - onboot: 0 (never auto-started)
//   - protection: 1 (prevent accidental deletion)
//   - tags: "bosh-parker[;director--<id>]"
//   - scsihw: "virtio-scsi-pci"
//   - memory: 16 MiB
//   - cores: 1
//   - no NIC, no disk
//
// opLabel identifies the caller in RetryOnTransientOrLock's log lines and in
// wrapped error messages (e.g. "ensure_parker_create", "ensure_fresh_parker_create").
//
// Validates cfg's VMID range before allocating: an invalid range (start<=0 or
// end<=start) fails loudly here rather than letting WithRange silently ignore
// it and AllocateWithRetry fall back to the general VM range [100,8999].
func createParkerVM(ctx context.Context, c Client, logger *log.Logger, node string, cfg ParkerConfig, opLabel string) (int, error) {
	if cfg.VMIDRangeStart <= 0 || cfg.VMIDRangeEnd <= cfg.VMIDRangeStart {
		return 0, cpierrors.Cloud("%s: invalid VMID range [%d, %d]",
			opLabel, cfg.VMIDRangeStart, cfg.VMIDRangeEnd)
	}

	tags := buildParkerTags(cfg)
	protection := 1
	onboot := 0
	memory := 16
	cores := 1
	scsihw := "virtio-scsi-pci"

	vmid, createErr := AllocateWithRetry(
		ctx,
		c,
		func(vmid int) error {
			params := map[string]any{
				"vmid":       vmid,
				"name":       parkerVMName(vmid),
				"tags":       tags,
				"protection": protection,
				"onboot":     onboot,
				"memory":     memory,
				"cores":      cores,
				"scsihw":     scsihw,
			}
			var upid string
			var innerErr error
			retryErr := RetryOnTransientOrLock(ctx, logger, opLabel, 0, func() error {
				upid, innerErr = c.QEMU().Create(ctx, node, params)
				return innerErr
			})
			if retryErr != nil {
				return retryErr
			}
			if upid != "" {
				if awaitErr := AwaitTask(ctx, c, node, upid); awaitErr != nil {
					return cpierrors.WrapAs(awaitErr, cpierrors.TypeRetriableCloud,
						fmt.Sprintf("%s: await create task for vmid %d", opLabel, vmid))
				}
			}
			return nil
		},
		// nil conflict predicate + single attempt: a VMID conflict means another
		// CPI created a parker at this VMID. We do NOT want AllocateWithRetry to
		// regenerate a fresh VMID (that would create a duplicate parker); instead
		// we surface the conflict so the branch below re-scans and ADOPTS the
		// winner (PC-4). Transient/lock errors are still retried inside the create
		// closure via RetryOnTransientOrLock.
		nil,
		1,
		WithRange(cfg.VMIDRangeStart, cfg.VMIDRangeEnd),
		WithNoBackoff(),
		WithStorageScan(node, cfg.DiskStorage),
	)
	if createErr != nil {
		// Create-conflict path: another CPI won the race. Re-find and adopt.
		if IsVMIDConflict(createErr) {
			winner, found, findErr := FindParkerForNode(ctx, c, node, cfg)
			if findErr != nil {
				return 0, findErr
			}
			if found {
				return winner, nil
			}
			return 0, cpierrors.Retriable("%s: VMID conflict but no parker found after re-scan on node %q", opLabel, node)
		}
		return 0, cpierrors.Wrap(createErr, fmt.Sprintf("%s: create parker VM", opLabel))
	}
	return vmid, nil
}

// EnsureParker returns a parker VMID for node, creating one if none exists.
// See createParkerVM for the VM's creation parameters and conflict-adoption
// behavior.
//
// Returns the parker VMID on success.
func EnsureParker(ctx context.Context, c Client, logger *log.Logger, node string, cfg ParkerConfig) (int, error) {
	if c == nil {
		return 0, cpierrors.Cloud("EnsureParker: client must not be nil")
	}
	if node == "" {
		return 0, cpierrors.Cloud("EnsureParker: node must not be empty")
	}

	// Check for existing parker first.
	existing, found, err := FindParkerForNode(ctx, c, node, cfg)
	if err != nil {
		return 0, err
	}
	if found {
		return existing, nil
	}

	return createParkerVM(ctx, c, logger, node, cfg, "ensure_parker_create")
}

// EnsureFreshParker is like EnsureParker but specifically allocates a new
// parker VM distinct from any existing parkers (used when all existing parkers
// are full). It intentionally skips the existing-parker short-circuit — the
// range-validation guard and create/allocate/await logic live in the shared
// createParkerVM helper.
func EnsureFreshParker(ctx context.Context, c Client, logger *log.Logger, node string, cfg ParkerConfig) (int, error) {
	if c == nil {
		return 0, cpierrors.Cloud("EnsureFreshParker: client must not be nil")
	}
	if node == "" {
		return 0, cpierrors.Cloud("EnsureFreshParker: node must not be empty")
	}

	return createParkerVM(ctx, c, logger, node, cfg, "ensure_fresh_parker_create")
}

// IsDiskParked reports whether bareVolid is currently held on a parker VM.
//
// Algorithm:
//  1. FindVMByDiskVolidOrNone — cluster-wide scan. Not found → false,nil.
//  2. Holder VMID not in [VMIDRangeStart, VMIDRangeEnd] → false,nil (no config read).
//  3. One config read: check bosh-parker tag. Missing → false + WARN.
//  4. Find slot via pve.FindDiskIDByVolID. Slot miss → retriable (config listed it).
//
// Returns (vmid, node, slot, parked, err).
func IsDiskParked(ctx context.Context, c Client, logger *log.Logger, bareVolid string, cfg ParkerConfig) (int, string, string, bool, error) {
	if c == nil {
		return 0, "", "", false, cpierrors.Cloud("IsDiskParked: client must not be nil")
	}
	if bareVolid == "" {
		return 0, "", "", false, cpierrors.Cloud("IsDiskParked: bareVolid must not be empty")
	}

	holderVMID, holderNode, found, err := FindVMByDiskVolidOrNone(ctx, c, "", bareVolid)
	if err != nil {
		return 0, "", "", false, cpierrors.Wrap(err, "IsDiskParked: cluster scan")
	}
	if !found {
		return 0, "", "", false, nil
	}

	// Range check: if holder is not in the parker range, it's a real VM — no
	// parker config read needed.
	if holderVMID < cfg.VMIDRangeStart || holderVMID > cfg.VMIDRangeEnd {
		return 0, "", "", false, nil
	}

	// One config read to confirm bosh-parker tag and find the slot.
	vmCfg, cfgErr := c.QEMU().Config(ctx, holderNode, holderVMID)
	if cfgErr != nil {
		return 0, "", "", false, cpierrors.WrapAs(cfgErr, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("IsDiskParked: config fetch for vmid %d on node %s", holderVMID, holderNode))
	}

	tagsRaw, _ := vmCfg["tags"].(string)
	if !tagContainsParker(tagsRaw) {
		if logger != nil {
			logger.Warn("IsDiskParked: vmid in parker range but missing bosh-parker tag — treating as non-parker",
				log.Int("vmid", holderVMID),
				log.String("node", holderNode),
				log.String("volid", bareVolid),
				log.String("tags", tagsRaw),
			)
		}
		return 0, "", "", false, nil
	}

	// Locate the slot the disk occupies. Miss here is unexpected (the scan just
	// confirmed the disk is on this VM). Treat as a transient config-read race.
	diskID, ok := FindDiskIDByVolID(qemu.ParseDisks(vmCfg), bareVolid)
	if !ok {
		return 0, "", "", false, cpierrors.Retriable(
			"IsDiskParked: disk %q confirmed on parker vmid %d but slot not found in config (possible race)",
			bareVolid, holderVMID,
		)
	}

	return holderVMID, holderNode, diskID, true, nil
}

// DiskHeldByRealVM reports whether bareVolid is currently attached to a VM that
// is NOT a parker (a workload VM, stemcell template, or any VMID outside the
// parker range, or an in-range VMID lacking the bosh-parker tag).
//
// It exists because IsDiskParked collapses "held by a non-parker VM" and "not
// held at all" into the same (false, nil) result, discarding a signal the park
// path needs: a disk still referenced by a running VM must never be parked, or
// PVE config would reference the same volume from two VMs (double-attach).
//
// Returns:
//   - held=true, vmid>0 when a non-parker VM holds the disk.
//   - held=false, vmid=0 when the disk is free-floating or held by a parker.
//   - err on transport/scan failure.
func DiskHeldByRealVM(ctx context.Context, c Client, logger *log.Logger, bareVolid string, cfg ParkerConfig) (held bool, vmid int, node string, err error) {
	if c == nil {
		return false, 0, "", cpierrors.Cloud("DiskHeldByRealVM: client must not be nil")
	}
	if bareVolid == "" {
		return false, 0, "", cpierrors.Cloud("DiskHeldByRealVM: bareVolid must not be empty")
	}

	holderVMID, holderNode, found, scanErr := FindVMByDiskVolidOrNone(ctx, c, "", bareVolid)
	if scanErr != nil {
		return false, 0, "", cpierrors.Wrap(scanErr, "DiskHeldByRealVM: cluster scan")
	}
	if !found {
		return false, 0, "", nil
	}

	// Out of parker range → definitely a real VM.
	if holderVMID < cfg.VMIDRangeStart || holderVMID > cfg.VMIDRangeEnd {
		return true, holderVMID, holderNode, nil
	}

	// In range: confirm via tag. A parker carries bosh-parker; anything else in
	// the range is a real (mis-placed) VM.
	vmCfg, cfgErr := c.QEMU().Config(ctx, holderNode, holderVMID)
	if cfgErr != nil {
		if IsNotFound(cfgErr) {
			// VM vanished mid-scan — treat as free-floating.
			return false, 0, "", nil
		}
		return false, 0, "", cpierrors.WrapAs(cfgErr, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("DiskHeldByRealVM: config fetch for vmid %d on node %s", holderVMID, holderNode))
	}
	tagsRaw, _ := vmCfg["tags"].(string)
	if tagContainsParker(tagsRaw) {
		// Held by a parker — not a real VM.
		return false, 0, "", nil
	}
	if logger != nil {
		logger.Warn("DiskHeldByRealVM: disk attached to VM in parker range without bosh-parker tag — treating as real VM",
			log.Int("vmid", holderVMID),
			log.String("node", holderNode),
			log.String("volid", bareVolid),
		)
	}
	return true, holderVMID, holderNode, nil
}

// ParkDisk attaches bareVolid to a parker VM on node. It is idempotent: if the
// disk is already parked on any parker VM the call returns nil immediately.
//
// pctx carries optional per-call attribution written into the provenance record.
// Pass a zero ParkContext when the source VM or full disk CID are unavailable.
//
// The algorithm:
//  1. IsDiskParked cluster-wide — already parked → nil.
//  2. EnsureParker for node.
//  3. Read parker VM config to find a free slot.
//  4. AttachDisk with explicit DiskID (scsiN).
//  5. ErrNoSlots → EnsureFreshParker + retry attach once.
//
// All PVE mutations are wrapped with RetryOnTransientOrLock.
func ParkDisk(ctx context.Context, c Client, logger *log.Logger, node, bareVolid string, cfg ParkerConfig, pctx ParkContext) error {
	if c == nil {
		return cpierrors.Cloud("ParkDisk: client must not be nil")
	}
	if node == "" {
		return cpierrors.Cloud("ParkDisk: node must not be empty")
	}
	if bareVolid == "" {
		return cpierrors.Cloud("ParkDisk: bareVolid must not be empty")
	}

	// Idempotency: already parked → nil.
	_, _, _, alreadyParked, checkErr := IsDiskParked(ctx, c, logger, bareVolid, cfg)
	if checkErr != nil {
		return cpierrors.Wrap(checkErr, "ParkDisk: idempotency check")
	}
	if alreadyParked {
		return nil
	}

	// Refuse to park a disk still attached to a real (non-parker) VM. Parking it
	// would add a second config reference to a volume a running VM owns
	// (double-attach). This guards a stale-Director-retry path where the disk
	// was re-attached elsewhere between the failed detach and this re-park.
	heldByReal, realVMID, _, heldErr := DiskHeldByRealVM(ctx, c, logger, bareVolid, cfg)
	if heldErr != nil {
		return cpierrors.Wrap(heldErr, "ParkDisk: real-VM holder check")
	}
	if heldByReal {
		if logger != nil {
			logger.Warn("ParkDisk: disk attached to a non-parker VM — refusing to park (idempotent no-op)",
				log.String("volid", bareVolid),
				log.Int("holder_vmid", realVMID),
			)
		}
		return nil
	}

	return parkDiskOnNode(ctx, c, logger, node, bareVolid, cfg, pctx)
}

// parkDiskOnNode performs the actual attach to a parker VM on node. Separated
// from ParkDisk so the EnsureFreshParker overflow path stays out of the
// idempotency pre-check.
//
// Capacity reuse: it lists every parker on node in ascending VMID order
// and attaches to the FIRST parker with a free slot. A fresh parker is created
// only when all existing parkers are full (or none exist). This prevents the
// parker-per-disk leak where each overflow disk would otherwise spawn a new
// parker VM, exhausting the VMID band.
func parkDiskOnNode(ctx context.Context, c Client, logger *log.Logger, node, bareVolid string, cfg ParkerConfig, pctx ParkContext) error {
	parkers, listErr := ListParkersForNode(ctx, c, node, cfg)
	if listErr != nil {
		return cpierrors.Wrap(listErr, "ParkDisk: list parkers")
	}

	// No parker exists yet → create the first one and attach there.
	if len(parkers) == 0 {
		parkerVMID, ensureErr := EnsureParker(ctx, c, logger, node, cfg)
		if ensureErr != nil {
			return cpierrors.Wrap(ensureErr, "ParkDisk: ensure parker")
		}
		if attachErr := attachToParker(ctx, c, logger, node, parkerVMID, bareVolid); attachErr != nil {
			return cpierrors.Wrap(attachErr, "ParkDisk: attach to parker")
		}
		updateParkerProvenance(ctx, c, logger, node, parkerVMID, bareVolid, cfg, pctx)
		return nil
	}

	// Try existing parkers in ascending VMID order. Attach to the first one
	// with a free slot. ErrNoSlots on a parker means it is full — move to the
	// next. Any other error is a real failure and propagates.
	for _, parkerVMID := range parkers {
		attachErr := attachToParker(ctx, c, logger, node, parkerVMID, bareVolid)
		if attachErr == nil {
			updateParkerProvenance(ctx, c, logger, node, parkerVMID, bareVolid, cfg, pctx)
			return nil
		}
		if errors.Is(attachErr, ErrNoSlots) {
			continue
		}
		return cpierrors.Wrap(attachErr, "ParkDisk: attach to parker")
	}

	// All existing parkers are full → create a fresh one and attach there.
	freshVMID, freshErr := EnsureFreshParker(ctx, c, logger, node, cfg)
	if freshErr != nil {
		return cpierrors.Wrap(freshErr, "ParkDisk: ensure fresh parker after all parkers full")
	}
	if attachErr := attachToParker(ctx, c, logger, node, freshVMID, bareVolid); attachErr != nil {
		return cpierrors.Wrap(attachErr, "ParkDisk: attach to fresh parker")
	}
	updateParkerProvenance(ctx, c, logger, node, freshVMID, bareVolid, cfg, pctx)
	return nil
}

// attachParkerVerifyRetries bounds the read-after-write slot-verify loop in
// attachToParker. Each iteration reads config, chooses a free slot, attaches,
// then re-reads to confirm the chosen slot holds our volid. A concurrent park
// that won the same slot demotes our disk to unusedN; on mismatch we retry the
// next free slot. The bound caps how many concurrent parkers we tolerate
// stealing slots before giving up retriably.
const attachParkerVerifyRetries = 5

// attachToParker reads the current config of parkerVMID, selects the first free
// scsiN slot, calls AttachDisk with an explicit DiskID, then re-reads the
// config to confirm the chosen slot actually holds bareVolid.
//
// Slot-race verify (F-W6-03): the PVE config PUT for an explicit DiskID is
// blind — two concurrent parks can both pick scsi0, and PVE replaces the slot
// for the second writer while demoting the first writer's volume to unusedN
// (silently un-parked, despite the first detach_disk already reporting
// success). After each attach this function re-reads the parker config and
// confirms bareVolid occupies the chosen slot. On mismatch it retries with the
// next free slot (excluding slots already proven stolen), bounded by
// attachParkerVerifyRetries. Exhausting free slots returns ErrNoSlots so the
// caller falls through to a fresh parker; exhausting the retry budget returns a
// retriable error so the Director re-drives the park.
func attachToParker(ctx context.Context, c Client, logger *log.Logger, node string, parkerVMID int, bareVolid string) error {
	// Slots proven to hold someone else's volid after our attach — never retry these.
	stolen := make(map[string]bool)

	for attempt := 0; attempt < attachParkerVerifyRetries; attempt++ {
		// Fresh config read for slot selection.
		vmCfg, cfgErr := c.QEMU().Config(ctx, node, parkerVMID)
		if cfgErr != nil {
			return cpierrors.WrapAs(cfgErr, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("attachToParker: config fetch for parker vmid %d", parkerVMID))
		}

		slot, slotErr := chooseParkSlotExcluding(qemu.ParseDisks(vmCfg), stolen)
		if slotErr != nil {
			return slotErr // ErrNoSlots — caller decides what to do
		}

		var upid string
		var attachErr error
		retryErr := RetryOnTransientOrLock(ctx, logger, "park_disk_attach", 0, func() error {
			upid, attachErr = c.QEMU().AttachDisk(ctx, node, parkerVMID, bareVolid, "scsi", &qemu.AttachOpts{
				DiskID: slot,
			})
			return attachErr
		})
		if retryErr != nil {
			return cpierrors.WrapAs(retryErr, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("attachToParker: attach %q at slot %s on parker vmid %d", bareVolid, slot, parkerVMID))
		}
		if upid != "" {
			if awaitErr := AwaitTask(ctx, c, node, upid); awaitErr != nil {
				return cpierrors.WrapAs(awaitErr, cpierrors.TypeRetriableCloud,
					fmt.Sprintf("attachToParker: await attach task for %q on parker vmid %d", bareVolid, parkerVMID))
			}
		}

		// Read-after-write: confirm our volid landed in the chosen slot.
		verifyCfg, verifyErr := c.QEMU().Config(ctx, node, parkerVMID)
		if verifyErr != nil {
			return cpierrors.WrapAs(verifyErr, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("attachToParker: verify config read for parker vmid %d", parkerVMID))
		}
		if slotHoldsVolid(qemu.ParseDisks(verifyCfg), slot, bareVolid) {
			return nil
		}

		// A concurrent park won this slot. Mark it stolen and retry the next free
		// slot. Our disk was demoted to unusedN by PVE; the next attach with an
		// explicit DiskID at a different slot re-references the same volid.
		stolen[slot] = true
		if logger != nil {
			logger.Warn("attachToParker: chosen slot lost to concurrent park, retrying next slot",
				log.Int("parker_vmid", parkerVMID),
				log.String("slot", slot),
				log.String("volid", bareVolid),
			)
		}
	}

	return cpierrors.Retriable(
		"attachToParker: slot verify failed after %d attempts for %q on parker vmid %d (concurrent park contention)",
		attachParkerVerifyRetries, bareVolid, parkerVMID,
	)
}

// chooseParkSlotExcluding scans the parsed disk map returned by
// qemu.ParseDisks and returns the first free scsiN slot in [scsi0, scsi30],
// skipping any slot in the exclude set (slots proven stolen by a concurrent
// park). A nil exclude set behaves as "exclude nothing". Returns ErrNoSlots
// when no non-excluded free slot remains.
//
// Inputs:
//   - disks: map[diskID]optString from qemu.ParseDisks; nil treated as empty.
//
// Failure modes:
//   - all non-excluded slots in scsi0..scsi30 occupied → ErrNoSlots.
func chooseParkSlotExcluding(disks map[string]string, exclude map[string]bool) (string, error) {
	for i := 0; i < parkerMaxSlots; i++ {
		slot := fmt.Sprintf("scsi%d", i)
		if _, occupied := disks[slot]; occupied {
			continue
		}
		if exclude[slot] {
			continue
		}
		return slot, nil
	}
	return "", ErrNoSlots
}

// slotHoldsVolid reports whether the given slot in the parsed disk map holds
// bareVolid (exact match or option-string "<volid>,..." form). PVE may append
// ",size=..." to the value, so an exact-only check would false-negative.
func slotHoldsVolid(disks map[string]string, slot, bareVolid string) bool {
	v, ok := disks[slot]
	if !ok {
		return false
	}
	return v == bareVolid || strings.HasPrefix(v, bareVolid+",")
}

// UnparkDisk detaches bareVolid from its parker VM. It is idempotent: if the
// disk is not parked the call returns nil.
//
// The algorithm:
//  1. IsDiskParked cluster-wide — not parked → nil.
//  2. DetachDisk(node, parkerVMID, slot) with RetryOnTransientOrLock.
func UnparkDisk(ctx context.Context, c Client, logger *log.Logger, bareVolid string, cfg ParkerConfig) error {
	if c == nil {
		return cpierrors.Cloud("UnparkDisk: client must not be nil")
	}
	if bareVolid == "" {
		return cpierrors.Cloud("UnparkDisk: bareVolid must not be empty")
	}

	parkerVMID, parkerNode, slot, parked, err := IsDiskParked(ctx, c, logger, bareVolid, cfg)
	if err != nil {
		return cpierrors.Wrap(err, "UnparkDisk: is-parked check")
	}
	if !parked {
		return nil
	}

	retryErr := RetryOnTransientOrLock(ctx, logger, "unpark_disk_detach", 0, func() error {
		return c.QEMU().DetachDisk(ctx, parkerNode, parkerVMID, slot)
	})
	if retryErr != nil {
		return cpierrors.WrapAs(retryErr, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("UnparkDisk: detach %q from parker vmid %d slot %s on node %s",
				bareVolid, parkerVMID, slot, parkerNode))
	}
	removeParkerProvenance(ctx, c, logger, parkerNode, parkerVMID, bareVolid, cfg)
	return nil
}
