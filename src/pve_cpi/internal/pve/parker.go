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
//	    DirectorID:     cpiCfg.StemcellDirectorID(), // empty = omit director scope tag
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
// Each parker VM holds up to 31 disks (scsi0..scsi30). When a parker is full
// EnsureParker allocates a fresh parker VMID in the same range.
package pve

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

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
// ParkerTag is always present; "director--<id>" is appended when DirectorID is
// set and sanitizes to a non-empty value (mirrors stemcell provenance pattern).
func buildParkerTags(cfg ParkerConfig) string {
	tags := []string{ParkerTag}
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

// chooseParkSlot scans the parsed disk map returned by qemu.ParseDisks and
// returns the first free scsiN slot in [scsi0, scsi30]. Returns ErrNoSlots
// when all 31 slots are occupied.
//
// Inputs:
//   - disks: map[diskID]optString from qemu.ParseDisks; nil treated as empty.
//
// Failure modes:
//   - all scsi0..scsi30 occupied → ErrNoSlots.
func chooseParkSlot(disks map[string]string) (string, error) {
	for i := 0; i < parkerMaxSlots; i++ {
		slot := fmt.Sprintf("scsi%d", i)
		if _, occupied := disks[slot]; !occupied {
			return slot, nil
		}
	}
	return "", ErrNoSlots
}

// FindParkerForNode scans cluster VM resources and returns the VMID of the
// first parker VM that exists on node with free slots (or any parker if the
// caller will verify slots itself). It returns the first parker VMID found in
// the range, so callers that need to find multiple parkers (e.g. for the
// full-slot fallback) should use ListParkersForNode.
//
// Returns (vmid, true, nil) when found; (0, false, nil) when none found.
// Returns (0, false, err) on transport failure.
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

// EnsureParker returns a parker VMID for node, creating one if none exists.
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
// If VMID allocation races with another CPI process (IsVMIDConflict), the
// function re-scans and adopts the winner.
//
// Returns the parker VMID on success.
func EnsureParker(ctx context.Context, c Client, logger *log.Logger, node string, cfg ParkerConfig) (int, error) {
	if c == nil {
		return 0, cpierrors.Cloud("EnsureParker: client must not be nil")
	}
	if node == "" {
		return 0, cpierrors.Cloud("EnsureParker: node must not be empty")
	}
	if cfg.VMIDRangeStart <= 0 || cfg.VMIDRangeEnd <= cfg.VMIDRangeStart {
		return 0, cpierrors.Cloud("EnsureParker: invalid VMID range [%d, %d]",
			cfg.VMIDRangeStart, cfg.VMIDRangeEnd)
	}

	// Check for existing parker first.
	existing, found, err := FindParkerForNode(ctx, c, node, cfg)
	if err != nil {
		return 0, err
	}
	if found {
		return existing, nil
	}

	// Allocate a VMID in the parker range and create the VM.
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
			retryErr := RetryOnTransientOrLock(ctx, logger, "ensure_parker_create", 0, func() error {
				upid, innerErr = c.QEMU().Create(ctx, node, params)
				return innerErr
			})
			if retryErr != nil {
				return retryErr
			}
			if upid != "" {
				if awaitErr := AwaitTask(ctx, c, node, upid); awaitErr != nil {
					return cpierrors.WrapAs(awaitErr, cpierrors.TypeRetriableCloud,
						fmt.Sprintf("EnsureParker: await create task for vmid %d", vmid))
				}
			}
			return nil
		},
		IsVMIDConflict,
		3,
		WithRange(cfg.VMIDRangeStart, cfg.VMIDRangeEnd),
		WithNoBackoff(),
	)
	if createErr != nil {
		// Create-conflict path: another CPI won the race. Re-find and adopt.
		if IsVMIDConflict(createErr) {
			winner, found2, findErr := FindParkerForNode(ctx, c, node, cfg)
			if findErr != nil {
				return 0, findErr
			}
			if found2 {
				return winner, nil
			}
			return 0, cpierrors.Retriable("EnsureParker: VMID conflict but no parker found after re-scan on node %q", node)
		}
		return 0, cpierrors.Wrap(createErr, "EnsureParker: create parker VM")
	}
	return vmid, nil
}

// EnsureFreshParker is like EnsureParker but specifically allocates a new
// parker VM distinct from any existing parkers (used when all existing parkers
// are full). It allocates the next free VMID in the range not already used by
// an existing parker.
func EnsureFreshParker(ctx context.Context, c Client, logger *log.Logger, node string, cfg ParkerConfig) (int, error) {
	if c == nil {
		return 0, cpierrors.Cloud("EnsureFreshParker: client must not be nil")
	}
	if node == "" {
		return 0, cpierrors.Cloud("EnsureFreshParker: node must not be empty")
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
			retryErr := RetryOnTransientOrLock(ctx, logger, "ensure_fresh_parker_create", 0, func() error {
				upid, innerErr = c.QEMU().Create(ctx, node, params)
				return innerErr
			})
			if retryErr != nil {
				return retryErr
			}
			if upid != "" {
				if awaitErr := AwaitTask(ctx, c, node, upid); awaitErr != nil {
					return cpierrors.WrapAs(awaitErr, cpierrors.TypeRetriableCloud,
						fmt.Sprintf("EnsureFreshParker: await create task for vmid %d", vmid))
				}
			}
			return nil
		},
		IsVMIDConflict,
		3,
		WithRange(cfg.VMIDRangeStart, cfg.VMIDRangeEnd),
		WithNoBackoff(),
	)
	if createErr != nil {
		if IsVMIDConflict(createErr) {
			winner, found, findErr := FindParkerForNode(ctx, c, node, cfg)
			if findErr != nil {
				return 0, findErr
			}
			if found {
				return winner, nil
			}
			return 0, cpierrors.Retriable("EnsureFreshParker: VMID conflict but no parker found after re-scan on node %q", node)
		}
		return 0, cpierrors.Wrap(createErr, "EnsureFreshParker: create fresh parker VM")
	}
	return vmid, nil
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

// ParkDisk attaches bareVolid to a parker VM on node. It is idempotent: if the
// disk is already parked on any parker VM the call returns nil immediately.
//
// The algorithm:
//  1. IsDiskParked cluster-wide — already parked → nil.
//  2. EnsureParker for node.
//  3. Read parker VM config to find a free slot.
//  4. AttachDisk with explicit DiskID (scsiN).
//  5. ErrNoSlots → EnsureFreshParker + retry attach once.
//
// All PVE mutations are wrapped with RetryOnTransientOrLock.
func ParkDisk(ctx context.Context, c Client, logger *log.Logger, node, bareVolid string, cfg ParkerConfig) error {
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

	return parkDiskOnNode(ctx, c, logger, node, bareVolid, cfg)
}

// parkDiskOnNode performs the actual attach to a parker VM on node. Separated
// from ParkDisk so EnsureFreshParker overflow can recurse once without the
// idempotency pre-check.
func parkDiskOnNode(ctx context.Context, c Client, logger *log.Logger, node, bareVolid string, cfg ParkerConfig) error {
	parkerVMID, ensureErr := EnsureParker(ctx, c, logger, node, cfg)
	if ensureErr != nil {
		return cpierrors.Wrap(ensureErr, "ParkDisk: ensure parker")
	}

	slotErr := attachToParker(ctx, c, logger, node, parkerVMID, bareVolid)
	if slotErr == nil {
		return nil
	}

	// Full parker → create a fresh one and attach there.
	if errors.Is(slotErr, ErrNoSlots) {
		freshVMID, freshErr := EnsureFreshParker(ctx, c, logger, node, cfg)
		if freshErr != nil {
			return cpierrors.Wrap(freshErr, "ParkDisk: ensure fresh parker after ErrNoSlots")
		}
		attachErr := attachToParker(ctx, c, logger, node, freshVMID, bareVolid)
		if attachErr != nil {
			return cpierrors.Wrap(attachErr, "ParkDisk: attach to fresh parker")
		}
		return nil
	}

	return cpierrors.Wrap(slotErr, "ParkDisk: attach to parker")
}

// attachToParker reads the current config of parkerVMID, selects the first
// free scsiN slot, and calls AttachDisk with an explicit DiskID.
func attachToParker(ctx context.Context, c Client, logger *log.Logger, node string, parkerVMID int, bareVolid string) error {
	// Fresh config read for slot selection.
	vmCfg, cfgErr := c.QEMU().Config(ctx, node, parkerVMID)
	if cfgErr != nil {
		return cpierrors.WrapAs(cfgErr, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("attachToParker: config fetch for parker vmid %d", parkerVMID))
	}

	disks := qemu.ParseDisks(vmCfg)
	slot, slotErr := chooseParkSlot(disks)
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
	return nil
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
	return nil
}
