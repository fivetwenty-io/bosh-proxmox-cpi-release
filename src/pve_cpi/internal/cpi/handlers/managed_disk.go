package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	inv "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageplacement"
)

type managedDiskRequest struct {
	deps                        Deps
	selection                   *StoragePlacementSelection
	collector                   *inv.Collector
	inventory                   *inv.Snapshot
	plan                        *StorageAllocationPlan
	ledger                      inv.Ledger
	id, token, format, hint, az string
	sizeGiB                     int
	opts                        map[string]string
	tags                        map[string]string
	iterator                    *StoragePlanIterator
	journal                     *aj.Journal
	budget                      int
	failedTargets               map[string]bool
}

func createManagedDisk(ctx context.Context, deps Deps, args []json.RawMessage, selection *StoragePlacementSelection, sizeMB int, cp createDiskCloudProperties, hint string, resolver *layeredResolver) (result any, retErr error) {
	defer func() {
		event := "allocation"
		if retErr != nil {
			event = "rejection"
		}
		deps.recordStoragePlacement(ctx, selection, event)
	}()
	defer func() {
		if retErr == nil {
			return
		}
		var cloud *cpierrors.Error
		if errors.As(retErr, &cloud) {
			return
		}
		var plan *StoragePlanError
		if errors.As(retErr, &plan) {
			retErr = plan.CPI()
			return
		}
		retErr = cpierrors.Cloud("create_disk: managed allocation failed; inspect the allocation journal and PVE read permissions")
	}()
	if err := deps.Config.ValidateStoragePlacementAllocation(); err != nil {
		return nil, err
	}
	m, err := prepareManagedDisk(ctx, deps, selection, sizeMB, cp, hint, resolver)
	if err != nil {
		return nil, err
	}
	journal, err := openStorageAllocationJournal(ctx, deps, m.inventory.Nodes())
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := journal.Close(); err != nil {
			result = nil
			retErr = cpierrors.Cloud("allocation %s journal close failed; inspect retained evidence before retrying", m.id)
		}
	}()
	// Admission validates retained remote provenance before a new allocation is
	// introduced. CreateDisk also checks all historical shortened-token values.
	if err := admitStorageAllocation(ctx, deps, journal, m.inventory.Nodes()); err != nil {
		return nil, err
	}
	intent, err := storageJournalIntent("create_disk", args, selection, m.inventory, m.plan)
	if err != nil {
		return nil, err
	}
	handle, err := journal.CreateDisk(ctx, m.id, intent)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := handle.Close(); err != nil {
			result = nil
			retErr = cpierrors.Cloud("allocation %s ownership lock close failed; inspect retained evidence before retrying", m.id)
		}
	}()
	m.journal = journal
	return m.execute(ctx, handle)
}

func prepareManagedDisk(ctx context.Context, deps Deps, selection *StoragePlacementSelection, sizeMB int, cp createDiskCloudProperties, hint string, resolver *layeredResolver) (*managedDiskRequest, error) {
	bytes, gib, err := managedDiskSize(sizeMB)
	if err != nil {
		return nil, err
	}
	format, err := managedDiskFormat(deps, resolver)
	if err != nil {
		return nil, err
	}
	// Allocation identity and deterministic seed exist before the first rank.
	id, err := aj.NewAllocationID()
	if err != nil {
		return nil, err
	}
	token, err := aj.DiskCorrelationToken(id)
	if err != nil {
		return nil, err
	}
	seed, err := storageplacement.NewSeed()
	if err != nil {
		return nil, err
	}
	nodes, err := managedDiskNodes(ctx, deps, cp, hint)
	if err != nil {
		return nil, err
	}
	collector, err := inv.NewCollector(inv.PVESource{Client: deps.PVE}, inv.Options{})
	if err != nil {
		return nil, err
	}
	discovery := managedDiskDiscovery(selection.Persistent, nodes)
	snapshot, err := collector.Discover(ctx, selection.Policy, discovery)
	if err != nil {
		return nil, err
	}
	observed := &managedDiskRequest{deps: deps, selection: selection, collector: collector, inventory: snapshot}
	budget, _ := createDiskAttemptBudgets(deps)
	for attempt := 0; attempt < budget; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		planningNodes, e := managedDiskPlanningNodes(ctx, deps, hint, nodes, snapshot.Nodes())
		if e != nil {
			return nil, e
		}
		observed.inventory = snapshot
		iterator, e := NewStoragePlanIterator(StoragePlanRequest{OnCandidateRejected: deps.storageCandidateRejectionObserver(ctx, selection), Selection: selection, Inventory: snapshot, Groups: []StoragePlanNodeGroup{{AZ: cp.AvailabilityZone, Nodes: planningNodes}}, Namespace: deps.Config.StoragePlacementNamespace, AllocationKey: id, Seed: seed, SeedSet: true, PersistentBytes: bytes, SearchBudget: 10000})
		if e != nil {
			return observed, e
		}
		observed.iterator = iterator
		plan, e := iterator.Next(ctx)
		if e != nil {
			return observed, e
		}
		if hint != "" {
			node, e := managedDiskHintNode(ctx, deps, hint)
			if e != nil {
				return nil, e
			}
			if node != plan.Node {
				snapshot, e = collector.Refresh(ctx, snapshot)
				if e != nil {
					return nil, e
				}
				continue
			}
		}
		ledger := inv.NewLedger()
		for chargeIndex := range plan.Charges {
			charge := &plan.Charges[chargeIndex]
			ledger, e = ledger.WithPlanned(snapshot, charge.Charge)
			if e != nil {
				return nil, e
			}
		}
		opts, e := managedDiskOptions(resolver, deps, format, cp.RetainOnDelete)
		if e != nil {
			return nil, e
		}
		return &managedDiskRequest{deps: deps, selection: selection, collector: collector, inventory: snapshot, plan: plan, ledger: ledger, id: id, token: token, format: format, hint: hint, az: cp.AvailabilityZone, sizeGiB: gib, opts: opts, tags: cp.Tags, iterator: iterator, budget: budget}, nil
	}
	return nil, cpierrors.Cloud("create_disk: hinted VM kept migrating; no allocation submitted")
}

func managedDiskSize(sizeMB int) (uint64, int, error) {
	if sizeMB <= 0 || uint64(sizeMB) > uint64(math.MaxInt64)/(1<<20) {
		return 0, 0, cpierrors.Cloud("create_disk: size exceeds supported allocation range")
	}
	bytes, err := inv.RoundBytes(uint64(sizeMB)*(1<<20), 1<<30)
	if err != nil || bytes > math.MaxInt64 {
		return 0, 0, cpierrors.Cloud("create_disk: rounded size exceeds supported allocation range")
	}
	return bytes, int(bytes / (1 << 30)), nil
}

func managedDiskHintNode(ctx context.Context, deps Deps, hint string) (string, error) {
	vmid, err := strconv.Atoi(hint)
	if err != nil || vmid <= 0 {
		return "", cpierrors.Cloud("create_disk: vm_cid hint must identify an existing VM")
	}
	guests, err := pve.ListGuestsAuthoritative(ctx, deps.PVE, deps.Log(ctx))
	if err != nil {
		return "", cpierrors.Cloud("create_disk: current hinted VM location observation failed")
	}
	node := ""
	for _, guest := range guests {
		if guest.VMID == vmid {
			if node != "" && node != guest.Node {
				return "", cpierrors.Cloud("create_disk: hinted VM has ambiguous current location")
			}
			node = guest.Node
		}
	}
	if node == "" {
		return "", cpierrors.Cloud("create_disk: hinted VM does not exist")
	}
	return node, nil
}

func managedDiskNodes(ctx context.Context, deps Deps, cp createDiskCloudProperties, hint string) ([]string, error) {
	nodes := []string{}
	add := func(node string) {
		if node != "" && !slices.Contains(nodes, node) {
			nodes = append(nodes, node)
		}
	}
	switch {
	case hint != "":
		node, err := managedDiskHintNode(ctx, deps, hint)
		if err != nil {
			return nil, err
		}
		add(node)
	case cp.Node != "":
		add(cp.Node)
	case cp.AvailabilityZone != "":
		azNodes, ok := deps.Config.AZCandidates(cp.AvailabilityZone)
		if !ok {
			return nil, cpierrors.Cloud("create_disk: unknown availability zone %q", cp.AvailabilityZone)
		}
		for _, node := range azNodes {
			add(node)
		}
	default:
		add(deps.Config.Node)
		if deps.Config.Placement != nil {
			zones := make([]string, 0, len(deps.Config.Placement.AZMap))
			for zone := range deps.Config.Placement.AZMap {
				zones = append(zones, zone)
			}
			sort.Strings(zones)
			for _, zone := range zones {
				for _, node := range deps.Config.Placement.AZMap[zone] {
					add(node)
				}
			}
		}
	}
	// Observe the configured migration destinations at the initial generation.
	if hint != "" {
		add(deps.Config.Node)
		if deps.Config.Placement != nil {
			for _, ns := range deps.Config.Placement.AZMap {
				for _, node := range ns {
					add(node)
				}
			}
		}
	}
	if len(nodes) == 0 {
		return nil, cpierrors.Cloud("create_disk: no configured node for shared storage allocation")
	}
	return nodes, nil
}

func (m *managedDiskRequest) executeAttempt(ctx context.Context, handle *aj.Handle) (any, error) {
	if len(m.plan.Targets) != 1 || m.plan.Targets[0].Role != "persistent" {
		return nil, cpierrors.Cloud("create_disk: invalid persistent allocation plan")
	}
	target := m.plan.Targets[0]
	release, err := m.deps.Inflight.acquire(ctx, target.Node, m.deps.Config.MaxInflightPerNodeLimit())
	if err != nil {
		return nil, err
	}
	defer release()
	vmid, err := pve.NextDiskVMID(ctx, m.deps.PVE, target.Node, target.StorageID, pve.WithRange(m.deps.Config.DiskVMIDRangeStart, m.deps.Config.DiskVMIDRangeEnd))
	if err != nil {
		return nil, cpierrors.Cloud("allocation %s: allocation VMID observation failed; no mutation submitted", m.id)
	}
	name, err := pve.AllocationVolumeName(vmid, m.plan.Namespace, m.id, m.format)
	if err != nil {
		return nil, err
	}
	volume := fmt.Sprintf("%s:%d/%s", target.StorageID, vmid, name)
	cid, err := m.cid(volume)
	if err != nil {
		return nil, err
	}
	if m.deps.Config.DetachedDiskParkedEnabled() {
		identity, e := pve.ResolveDiskIdentity(ctx, m.deps.PVE, m.deps.Log(ctx), volume, m.token, parkerReadConfigFor(m.deps))
		if e != nil {
			return nil, cpierrors.Cloud("allocation %s: shortened-token ownership observation failed; no mutation submitted", m.id)
		}
		if identity.Holder.Found || identity.Intent != nil {
			return nil, cpierrors.Cloud("allocation %s shortened token collision; no volume allocated", m.id)
		}
	}
	if err := m.verifyBirthAbsence(ctx, target.Node, volume); err != nil {
		return nil, err
	}
	if err := m.revalidate(ctx); err != nil {
		return nil, err
	}
	birthTarget := aj.Target{Node: target.Node, Storage: target.StorageID, Backing: target.BackingKey, VMID: vmid, IntendedVolume: volume}
	parameters, err := aj.MutationParameters(map[string]any{"version": 1, "kind": "persistent_birth", "absence_verified": true})
	if err != nil {
		return nil, err
	}
	step, err := storageMutationIntent(handle, "create_persistent_volume", birthTarget, m.plan.Charges, parameters)
	if err != nil {
		return nil, err
	}
	// Check location immediately before the synchronous mutation. A change here
	// stops with retained intent, rather than falling through to legacy storage.
	if m.hint != "" {
		node, e := managedDiskHintNode(ctx, m.deps, m.hint)
		if e != nil || node != target.Node {
			return nil, storageAllocationUncertain(handle, "pre-allocation hint recheck")
		}
	}
	for chargeIndex := range m.plan.Charges {
		charge := &m.plan.Charges[chargeIndex]
		m.ledger, err = m.ledger.Submit(charge.Charge.ID)
		if err != nil {
			return nil, err
		}
	}
	returned, err := m.deps.PVE.Storage().CreateVolume(ctx, target.Node, target.StorageID, m.sizeGiB, m.format, vmid, name)
	if err != nil {
		if fields, known := managedDiskValidationRejection(err); known {
			return nil, &managedDiskRejected{volume: volume, fields: fields}
		}
		return nil, storageAllocationUncertain(handle, "persistent volume submission")
	}
	if returned != "" && returned != volume {
		return nil, storageAllocationUncertain(handle, "persistent volume identity mismatch")
	}
	if err = m.observeVolume(ctx, volume); err != nil {
		return nil, storageAllocationUncertain(handle, "persistent volume readback")
	}
	if err = storageMutationObserved(handle, step, []string{volume}, true); err != nil {
		return nil, storageAllocationUncertain(handle, "persistent acquired-charge persistence")
	}
	records := m.ledger.Records()
	for chargeIndex := range records {
		charge := &records[chargeIndex]
		proof, e := m.collector.MarkCompletion(*charge, volume, true, true)
		if e != nil {
			return nil, storageAllocationUncertain(handle, "persistent completion proof")
		}
		m.ledger, e = m.ledger.Acquire(charge.Charge.ID, proof)
		if e != nil {
			return nil, storageAllocationUncertain(handle, "persistent ledger acquisition")
		}
	}
	if m.deps.Config.DetachedDiskParkedEnabled() {
		if err = m.park(ctx, handle, cid, volume); err != nil {
			return nil, storageAllocationUncertain(handle, "persistent parker completion")
		}
	}
	if err = m.applyTags(ctx, handle, cid, volume); err != nil {
		return nil, storageAllocationUncertain(handle, "persistent VM metadata completion")
	}
	record := handle.Record()
	record.CID = cid
	record.State = aj.ReadyToReturn
	if err = handle.Save(record); err != nil {
		return nil, storageAllocationUncertain(handle, "exact disk CID persistence")
	}
	return cid, nil
}

func (m *managedDiskRequest) cid(volume string) (string, error) {
	// The canonical volid already records the storage ID. Repeating it in
	// metadata can push ordinary NFS names past the Director's CID limit.
	meta := &pve.DiskCIDMeta{AZ: m.az, Opts: m.opts, Format: m.format, Anchor: m.deps.Config.DetachedDiskParkedEnabled()}
	if meta.Anchor {
		meta.ID = m.token
	}
	cid, err := pve.EncodeDiskCIDCompressed(volume, meta)
	if err != nil {
		return "", err
	}
	if len(cid) > pve.DiskCIDLengthTarget {
		return "", cpierrors.Cloud("create_disk: encoded managed disk CID exceeds %d characters before allocation", pve.DiskCIDLengthTarget)
	}
	return cid, nil
}

func (m *managedDiskRequest) revalidate(ctx context.Context) error {
	snapshot, ledger, err := RevalidateStorageAllocationPlan(ctx, m.collector, m.inventory, m.inventory, m.selection, m.plan, m.ledger, time.Now)
	if err != nil {
		return err
	}
	m.inventory = snapshot
	m.ledger = ledger
	return nil
}

func (m *managedDiskRequest) observeVolume(ctx context.Context, volume string) error {
	locator, id, ok := pve.ParseAllocationVolumeID(volume)
	if !ok || id != m.id || locator != pve.AllocationNamespaceLocator(m.plan.Namespace) {
		return fmt.Errorf("allocation provenance mismatch")
	}
	target := m.plan.Targets[0]
	_, bare, err := pve.ParseDiskCID(volume)
	if err != nil {
		return err
	}
	info, err := m.deps.PVE.Nodes().GetStorageContent(ctx, target.Node, target.StorageID, bare)
	if err != nil {
		return err
	}
	if info == nil || info.Size <= 0 || uint64(info.Size) != target.VirtualBytes || info.Format != m.format {
		return fmt.Errorf("volume format or size readback mismatch")
	}
	return nil
}

// Atomic selection can win without activating a set-managed allocation. Keep
// legacy format/performance/profile resolution intact while passing only the
// atomic winning pool or tier into the old storage selector.
func managedDiskLegacyResolver(r *layeredResolver, selection *StoragePlacementSelection) *layeredResolver {
	if selection.Persistent == nil || !selection.Persistent.Atomic {
		return r
	}
	cloned := *r
	cloned.layers = make([]map[string]any, len(r.layers))
	for i, layer := range r.layers {
		cloned.layers[i] = map[string]any{}
		for key, value := range layer {
			if key != "storage_pool" && key != "storage" && key != "storage_tier" {
				cloned.layers[i][key] = value
			}
		}
	}
	key := "storage_pool"
	if selection.Persistent.Kind == "tier" {
		key = "storage_tier"
	}
	if len(cloned.layers) == 0 {
		cloned.layers = append(cloned.layers, map[string]any{})
	}
	cloned.layers[0][key] = strings.TrimSpace(selection.Persistent.Value)
	return &cloned
}

func managedDiskFormat(deps Deps, resolver *layeredResolver) (string, error) {
	format, found := resolver.String("disk_format")
	if !found {
		format = deps.Config.VMDiskFormat
	}
	if format == "" {
		format = diskFormatQCOW2
	}
	if format != "raw" && format != "qcow2" && format != "vmdk" {
		return "", cpierrors.Cloud("create_disk: unsupported managed disk format %q", format)
	}
	return format, nil
}

func managedDiskDiscovery(r *StorageRoleSelection, nodes []string) inv.Request {
	discovery := inv.Request{Nodes: nodes}
	if r.SetName != "" {
		discovery.SetNames = append(discovery.SetNames, r.SetName)
	}
	if r.BoundaryName != "" {
		discovery.SetNames = append(discovery.SetNames, r.BoundaryName)
	}
	if r.Kind == "pool" {
		discovery.CompanionStorageIDs = []string{r.Value}
	}
	if r.Kind == "tier" {
		discovery.TierNames = []string{r.Value}
	}
	return discovery
}

func managedDiskOptions(resolver *layeredResolver, deps Deps, format string, retain *bool) (map[string]string, error) {
	opts, e := resolveDiskPerfOptions(resolver, deps.Config, "nfs", format)
	if e != nil {
		return nil, e
	}
	if retain != nil && *retain {
		if opts == nil {
			opts = map[string]string{}
		}
		opts[diskOptRetainOnDelete] = "1"
	}
	return opts, nil
}

func managedDiskPlanningNodes(ctx context.Context, deps Deps, hint string, nodes, observed []string) ([]string, error) {
	if hint == "" {
		return nodes, nil
	}
	node, err := managedDiskHintNode(ctx, deps, hint)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(observed, node) {
		return nil, cpierrors.Cloud("create_disk: hinted VM migrated outside observed nodes; no allocation submitted")
	}
	return []string{node}, nil
}

func (m *managedDiskRequest) verifyBirthAbsence(ctx context.Context, node, volume string) error {
	// The existence probe protects against an externally introduced UUID/name
	// collision. Its absence alone is never used to resume an old planned record.
	exists, err := managedVolumePresent(ctx, m.deps, node, volume)
	if err != nil {
		return cpierrors.Cloud("allocation %s: intended volume existence observation failed; no mutation submitted", m.id)
	}
	if exists {
		return cpierrors.Cloud("allocation %s target name already exists; ownership audit required", m.id)
	}
	return nil
}
