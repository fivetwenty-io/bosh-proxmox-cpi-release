package handlers

import (
	"context"
	"errors"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

type managedDiskIdentity struct {
	terminalAbsent bool
	absent         bool
	record         aj.Record
	provenance     pve.DiskAllocationProvenance
}

func resolveManagedDiskIdentity(ctx context.Context, deps Deps, rd resolvedDisk) (resolvedDisk, error) {
	locator, id, managed := pve.ParseAllocationVolumeID(rd.birth)
	if !managed {
		if strings.Contains(rd.birth, "-bosh-") && strings.Contains(rd.birth, "-alloc-") {
			return resolvedDisk{}, cpierrors.Cloud("malformed managed disk allocation identity; audit required")
		}
		markerID, namespace, found, err := managedDiskMarkerIdentity(ctx, deps, rd)
		if err != nil {
			return resolvedDisk{}, err
		}
		if !found {
			return rd, nil
		}
		id = markerID
		locator = pve.AllocationNamespaceLocator(namespace)
	}
	if deps.Config == nil || deps.Config.StoragePlacementNamespace == "" || locator != pve.AllocationNamespaceLocator(deps.Config.StoragePlacementNamespace) {
		return resolvedDisk{}, cpierrors.Cloud("managed disk namespace differs from configured authority; audit required")
	}
	node := deps.Config.Node
	if rd.holder != nil {
		node = rd.holder.Node
	} else if rd.intent != nil {
		node = rd.intent.ParkerNode
	}
	journal, err := openStorageAllocationJournal(ctx, deps, []string{node})
	if err != nil {
		return resolvedDisk{}, cpierrors.Cloud("managed disk journal authority unavailable; audit required")
	}
	record, inspectErr := journal.Inspect(id)
	err = errors.Join(inspectErr, journal.Close())
	if err != nil {
		return resolvedDisk{}, cpierrors.Cloud("managed disk allocation record unavailable; audit required")
	}
	if record.Kind != "disk" || record.Namespace != deps.Config.StoragePlacementNamespace || rd.stableID != "" && record.DiskToken != rd.stableID {
		return resolvedDisk{}, cpierrors.Cloud("managed disk allocation identity conflicts; audit required")
	}
	if rd.stableID == "" {
		// A bare historical CID still carries the full allocation UUID. Recover
		// its immutable token from the journal before locating or moving it.
		metadata := pve.DiskCIDMeta{}
		if rd.meta != nil {
			metadata = *rd.meta
		}
		metadata.ID = record.DiskToken
		return resolveDiskForOp(ctx, deps, "managed_disk_identity", rd.diskCID, rd.birth, &metadata)
	}
	return resolveManagedDiskRecord(ctx, deps, rd, record, node)
}
func resolveManagedDiskRecord(ctx context.Context, deps Deps, rd resolvedDisk, record aj.Record, node string) (resolvedDisk, error) {
	id := record.ID
	storage, _, err := pve.ParseDiskCID(rd.volid)
	if err != nil {
		return resolvedDisk{}, err
	}
	if record.State == aj.Deleted || record.State == aj.Cleaned {
		if rd.holder != nil || rd.intent != nil {
			return resolvedDisk{}, cpierrors.Cloud("terminal managed disk still has ownership provenance; audit required")
		}
		exists, err := managedVolumePresent(ctx, deps, node, rd.volid)
		if err != nil || exists {
			return resolvedDisk{}, cpierrors.Cloud("terminal managed disk absence cannot be verified; audit required")
		}
		rd.allocation = &managedDiskIdentity{record: record, terminalAbsent: true}
		return rd, nil
	}
	backing, err := managedDiskActualBacking(ctx, deps, storage)
	if err != nil {
		return resolvedDisk{}, err
	}
	if err := validateManagedDiskJournalIdentity(record, rd.birth, rd.volid, backing); err != nil {
		return resolvedDisk{}, err
	}
	exists := false
	if rd.holder == nil && rd.intent == nil {
		node, exists, err = observeManagedFreeDisk(ctx, deps, record, rd.volid, backing, rd.meta, node)
	} else {
		exists, err = observeManagedDiskVolume(ctx, deps, node, rd.volid, rd.meta)
	}
	if err != nil {
		return resolvedDisk{}, err
	}

	if !exists && (rd.holder != nil || rd.intent != nil) {
		return resolvedDisk{}, cpierrors.Cloud("managed disk ownership provenance references a missing volume; audit required")
	}
	provenance := pve.DiskAllocationProvenance{Version: 1, AllocationID: id, AllocationNamespace: record.Namespace, Node: node, Volid: rd.volid, Backing: backing}
	if rd.holder != nil {
		cfg, err := deps.PVE.QEMU().Config(ctx, rd.holder.Node, rd.holder.VMID)
		if err != nil || cfg == nil {
			return resolvedDisk{}, cpierrors.Cloud("managed disk holder cannot be verified; audit required")
		}
		actual, found, err := pve.FindDiskAllocationProvenance(pve.DescriptionFromConfig(cfg), rd.sentinelKey())
		if err != nil {
			return resolvedDisk{}, err
		}
		if found {
			if actual.AllocationID != id || actual.AllocationNamespace != record.Namespace || actual.Volid != rd.volid || actual.Backing != "" && actual.Backing != backing {
				return resolvedDisk{}, cpierrors.Cloud("managed disk holder provenance conflicts; audit required")
			}
		} else if rd.volid != rd.birth {
			return resolvedDisk{}, cpierrors.Cloud("renamed managed disk lacks full ownership provenance; audit required")
		}
	} else if rd.intent != nil {
		if rd.intent.AllocationID != id || rd.intent.AllocationNamespace != record.Namespace || rd.intent.AllocationBacking != "" && rd.intent.AllocationBacking != backing {
			return resolvedDisk{}, cpierrors.Cloud("managed disk transfer provenance conflicts; audit required")
		}
	}
	rd.allocation = &managedDiskIdentity{record: record, provenance: provenance, absent: !exists}
	return rd, nil
}

func managedDiskActualBacking(ctx context.Context, deps Deps, storage string) (string, error) {
	info, err := managedDiskActualDefinition(ctx, deps, storage)
	if err != nil {
		return "", err
	}
	backing := info.BackingKey()
	if backing == "" {
		return "", cpierrors.Cloud("managed disk backing identity unavailable")
	}
	return backing, nil
}
func managedDiskActualDefinition(ctx context.Context, deps Deps, storage string) (pve.StorageInfo, error) {
	if deps.PVE == nil || deps.PVE.ClusterStorage() == nil {
		return pve.StorageInfo{}, cpierrors.Cloud("managed disk storage definition unavailable")
	}
	all, err := deps.PVE.ClusterStorage().ListStorage(ctx, nil)
	if err != nil || all == nil || *all == nil {
		return pve.StorageInfo{}, cpierrors.Cloud("managed disk storage definition unavailable")
	}
	var found pve.StorageInfo
	for _, raw := range *all {
		info, err := pve.ParseStorageEntry(raw)
		if err != nil {
			return pve.StorageInfo{}, cpierrors.Cloud("managed disk storage definition malformed")
		}
		if info.Name == storage {
			if found.Name != "" {
				return pve.StorageInfo{}, cpierrors.Cloud("managed disk storage definition ambiguous")
			}
			found = info
		}
	}
	if found.Name == "" {
		return pve.StorageInfo{}, cpierrors.Cloud("managed disk actual storage is unavailable")
	}
	return found, nil
}
func managedDiskParkContext(rd resolvedDisk, pctx pve.ParkContext) pve.ParkContext {
	if rd.allocation != nil {
		pctx.AllocationID = rd.allocation.record.ID
		pctx.AllocationNamespace = rd.allocation.record.Namespace
		pctx.AllocationBacking = rd.allocation.provenance.Backing
	}
	return pctx
}
func writeManagedDiskHolder(ctx context.Context, deps Deps, rd resolvedDisk, node string, vmid int, volid string) error {
	if rd.allocation == nil {
		return nil
	}
	entry := rd.allocation.provenance
	entry.Node = node
	entry.Volid = volid
	if err := pve.WriteDiskAllocationProvenance(ctx, deps.PVE, node, vmid, rd.sentinelKey(), entry); err != nil {
		return cpierrors.Cloud("managed disk holder provenance was not verified; audit required")
	}
	return nil
}
func verifyManagedDiskParked(ctx context.Context, deps Deps, rd resolvedDisk, volid string) error {
	if rd.allocation == nil {
		return nil
	}
	if err := pve.VerifyAllocationParked(ctx, deps.PVE, deps.Log(ctx), volid, rd.stableID, rd.allocation.record.Namespace, rd.allocation.record.ID, parkerReadConfigFor(deps)); err != nil {
		return cpierrors.Cloud("managed disk parker provenance requires reconciliation")
	}
	return nil
}

// A volume and its backing must be associated by the same durable step. Separate
// historical steps cannot be combined to invent ownership after a transfer.
func validateManagedDiskJournalIdentity(record aj.Record, birth, actual, backing string) error {
	if record.State == aj.Deleted || record.State == aj.Cleaned {
		return cpierrors.Cloud("terminal managed disk allocation still has a live resource; audit required")
	}
	birthRecorded, actualRecorded := false, false
	for stepIndex := range record.Steps {
		step := &record.Steps[stepIndex]
		volumes := append([]string{step.Target.IntendedVolume}, step.VolIDs...)
		for _, volume := range volumes {
			birthRecorded = birthRecorded || volume == birth
			actualRecorded = actualRecorded || volume == actual && step.Target.Backing == backing
		}
	}
	if !birthRecorded || !actualRecorded {
		return cpierrors.Cloud("managed disk physical backing or volume conflicts with journal; audit required")
	}
	return nil
}

// observeManagedDiskVolume reads the exact storage-content endpoint. A filename,
// journal record, or dangling VM config entry alone never proves live ownership.
func observeManagedDiskVolume(ctx context.Context, deps Deps, node, volume string, meta *pve.DiskCIDMeta) (bool, error) {
	present, err := managedVolumePresent(ctx, deps, node, volume)
	if err != nil || !present {
		return false, err
	}
	storage, bare, err := pve.ParseDiskCID(volume)
	if err != nil {
		return false, err
	}
	info, err := deps.PVE.Nodes().GetStorageContent(ctx, node, storage, bare)
	if err != nil {
		if pve.IsNotFound(err) {
			return false, nil
		}
		return false, cpierrors.Cloud("managed disk exact volume observation failed")
	}
	if info == nil || info.Size <= 0 || info.Format == "" {
		return false, cpierrors.Cloud("managed disk exact volume observation malformed")
	}
	if meta != nil && meta.Format != "" && info.Format != meta.Format {
		return false, cpierrors.Cloud("managed disk format differs from recorded identity")
	}
	return true, nil
}

// Free disks on nonshared backings must be checked at their recorded physical
// nodes. The configured default node cannot certify absence on another node.
func observeManagedFreeDisk(ctx context.Context, deps Deps, record aj.Record, volume, backing string, meta *pve.DiskCIDMeta, fallback string) (string, bool, error) {
	storage, _, err := pve.ParseDiskCID(volume)
	if err != nil {
		return "", false, err
	}
	definition, err := managedDiskActualDefinition(ctx, deps, storage)
	if err != nil {
		return "", false, err
	}
	candidates := []string{}
	for stepIndex := range record.Steps {
		step := &record.Steps[stepIndex]
		if step.Target.Backing != backing || step.Target.Node == "" {
			continue
		}
		matches := step.Target.IntendedVolume == volume
		for _, id := range step.VolIDs {
			matches = matches || id == volume
		}
		if matches {
			seen := false
			for _, node := range candidates {
				seen = seen || node == step.Target.Node
			}
			if !seen {
				candidates = append(candidates, step.Target.Node)
			}
		}
	}
	if len(candidates) == 0 {
		candidates = append(candidates, fallback)
	}
	if definition.IsShared() {
		candidates = candidates[len(candidates)-1:]
	}
	chosen := ""
	for _, node := range candidates {
		continuity, err := pve.ObserveStorageClusterIdentity(ctx, deps.PVE.Nodes(), []string{node})
		if err != nil || continuity.ID() != record.ClusterID {
			return "", false, cpierrors.Cloud("managed free disk cluster continuity differs")
		}
		exists, err := observeManagedDiskVolume(ctx, deps, node, volume, meta)
		if err != nil {
			return "", false, err
		}
		if exists {
			if chosen != "" {
				return "", false, cpierrors.Cloud("managed local disk has multiple retained copies; audit required")
			}
			chosen = node
		}
	}
	if chosen != "" {
		return chosen, true, nil
	}
	return candidates[len(candidates)-1], false, nil
}

// A known holder or transfer marker can activate managed ownership even when a
// caller supplies an older plain volume CID. Unmarked legacy disks incur no
// journal access, and absent holder information does not invent ownership.
func managedDiskMarkerIdentity(ctx context.Context, deps Deps, rd resolvedDisk) (string, string, bool, error) {
	if rd.intent != nil && (rd.intent.AllocationID != "" || rd.intent.AllocationNamespace != "") {
		if rd.intent.AllocationID == "" || rd.intent.AllocationNamespace == "" {
			return "", "", false, cpierrors.Cloud("incomplete managed transfer identity")
		}
		return rd.intent.AllocationID, rd.intent.AllocationNamespace, true, nil
	}
	if rd.holder == nil {
		return "", "", false, nil
	}
	cfg, err := deps.PVE.QEMU().Config(ctx, rd.holder.Node, rd.holder.VMID)
	if err != nil || cfg == nil {
		return "", "", false, cpierrors.Cloud("disk holder provenance unavailable")
	}
	description := pve.DescriptionFromConfig(cfg)
	if !strings.Contains(description, "bosh_disk_allocations") && !strings.Contains(description, "allocation_id") {
		return "", "", false, nil
	}
	entry, found, err := pve.FindDiskAllocationProvenance(description, rd.sentinelKey())
	if err != nil {
		return "", "", false, err
	}
	if !found {
		return "", "", false, nil
	}
	return entry.AllocationID, entry.AllocationNamespace, true, nil
}
