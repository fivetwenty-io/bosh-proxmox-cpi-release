package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

type managedVMRecordReadback struct {
	deps        Deps
	record      aj.Record
	plan        *StorageAllocationPlan
	vmid        int
	node        string
	cfg         map[string]any
	recorded    map[string]bool
	backings    map[string]string
	definitions map[string]pve.StorageInfo
	devices     map[string]string
	volumes     []string
}

func (r *managedVMRecordReadback) fail(reason string) error {
	return cpierrors.Cloud("allocation %s requires reconciliation: %s", r.record.ID, reason)
}
func (r *managedVMRecordReadback) readTarget() error {
	record := r.record
	vmid := 0
	recorded := map[string]bool{}
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.Target.External || step.Attempt != record.ActiveAttempt() {
			continue
		}
		if step.Target.VMID > 0 {
			if vmid != 0 && vmid != step.Target.VMID {
				return r.fail("active VM targets disagree")
			}
			vmid = step.Target.VMID
		}
		for _, volume := range step.VolIDs {
			recorded[volume] = true
		}
	}
	if vmid <= 0 {
		return r.fail("no durable VM target exists")
	}
	if record.CID != "" && record.CID != strconv.Itoa(vmid) {
		return r.fail("recorded CID and VM target disagree")
	}

	r.vmid = vmid
	r.recorded = recorded
	return nil
}
func (r *managedVMRecordReadback) readLocation(ctx context.Context) error {
	deps, record, vmid := r.deps, r.record, r.vmid
	guests, skipped, err := pve.ListGuestsAuthoritativeTolerant(ctx, deps.PVE, deps.Log(ctx))
	if err != nil || len(skipped) > 0 {
		return r.fail("actual VM location could not be fully inspected")
	}
	node := ""
	for _, guest := range guests {
		if guest.VMID == vmid {
			if node != "" {
				return r.fail("VM identity has multiple locations")
			}
			node = guest.Node
		}
	}
	if node == "" {
		return r.fail("recorded VM disappeared; absence alone does not release its generation")
	}
	allowed := false
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.Attempt == record.ActiveAttempt() && storageAuditVMTargetMatches(record, *step, node, vmid) {
			allowed = true
		}
	}
	if !allowed {
		return r.fail("actual VM is outside recorded nodes")
	}

	r.node = node
	return nil
}
func (r *managedVMRecordReadback) readConfig(ctx context.Context) error {
	deps, record, node, vmid := r.deps, r.record, r.node, r.vmid
	cfg, err := deps.PVE.QEMU().Config(ctx, node, vmid)
	if err != nil || cfg == nil {
		return r.fail("actual VM configuration is unavailable")
	}
	marker, found, err := pve.ParseStorageAllocationMarker(pve.DescriptionFromConfig(cfg))
	hash := sha256.Sum256([]byte(record.AgentID))
	if err != nil || !found || marker.Namespace != record.Namespace || marker.AllocationID != record.ID || marker.Kind != "vm" || marker.AgentSHA256 != hex.EncodeToString(hash[:]) {
		return r.fail("actual VM lacks matching full allocation provenance")
	}

	r.cfg = cfg
	return nil
}
func (r *managedVMRecordReadback) readDefinitions(ctx context.Context) error {
	deps := r.deps
	definitions, err := deps.PVE.ClusterStorage().ListStorage(ctx, nil)
	if err != nil || definitions == nil || *definitions == nil {
		return r.fail("actual backing definitions are unavailable")
	}
	backings := map[string]string{}
	actualDefinitions := map[string]pve.StorageInfo{}
	for _, raw := range *definitions {
		def, e := pve.ParseStorageEntry(raw)
		if e != nil || backings[def.Name] != "" {
			return r.fail("actual backing definitions are malformed or ambiguous")
		}
		backings[def.Name] = def.BackingKey()
		actualDefinitions[def.Name] = def
	}

	r.backings = backings
	r.definitions = actualDefinitions
	return nil
}
func (r *managedVMRecordReadback) readDevices() error {
	cfg, recorded := r.cfg, r.recorded
	devices := map[string]string{}
	for key, value := range cfg {
		if !managedVMVolumeDevice(key) {
			continue
		}
		drive, ok := pve.ConfigStringValue(value)
		if !ok {
			return r.fail("VM disk configuration is malformed")
		}
		volume := strings.Split(drive, ",")[0]
		if strings.Contains(volume, ":") {
			if _, duplicate := devices[volume]; duplicate && recorded[volume] {
				return r.fail("recorded volume appears in multiple VM devices")
			}
			devices[volume] = key
		}
	}

	r.devices = devices
	return nil
}
func (r *managedVMRecordReadback) verifyVolume(ctx context.Context, volume string) error {
	deps, record, plan, node := r.deps, r.record, r.plan, r.node
	backings, actualDefinitions, devices := r.backings, r.definitions, r.devices
	storage, bare, e := pve.ParseDiskCID(volume)
	if e != nil {
		return r.fail("recorded volume identity is malformed")
	}
	expectedBacking := ""
	for i := range record.Steps {
		step := &record.Steps[i]
		if !step.Target.External && step.Attempt == record.ActiveAttempt() && slices.Contains(step.VolIDs, volume) && step.Target.Storage == storage && step.Target.Backing != "" {
			expectedBacking = step.Target.Backing
		}
	}
	if expectedBacking == "" {
		if def, ok := plan.Definitions[storage]; ok {
			expectedBacking = def.BackingKey()
		}
	}
	if expectedBacking == "" || backings[storage] != expectedBacking {
		return r.fail("recorded volume backing changed or disappeared")
	}
	physicalMatch := false
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.Attempt == record.ActiveAttempt() {
			physicalMatch = physicalMatch || storageAuditVolumeTargetMatches(record, *step, actualDefinitions, node, volume)
		}
	}
	if !physicalMatch {
		return r.fail("recorded volume physical node or backing changed")
	}
	if old, found := plan.Definitions[storage]; found && old.IsShared() != actualDefinitions[storage].IsShared() {
		return r.fail("recorded volume shared-storage scope changed")
	}

	info, e := deps.PVE.Nodes().GetStorageContent(ctx, node, storage, bare)
	if e != nil || info == nil || info.Size <= 0 {
		return r.fail("recorded volume cannot be observed")
	}
	if _, ok := devices[volume]; !ok {
		return r.fail("recorded volume is no longer present in VM configuration")
	}

	return nil
}
func (r *managedVMRecordReadback) verifyRole(ctx context.Context, target *StoragePlanTarget) error {
	deps, record, node, cfg, devices := r.deps, r.record, r.node, r.cfg, r.devices
	matched := false
	for i := range record.Steps {
		step := &record.Steps[i]
		prefix := "vm." + target.Role + "."
		device, ok := strings.CutPrefix(step.Kind, prefix)
		if !ok || step.Target.External || step.Attempt != record.ActiveAttempt() {
			continue
		}
		if !managedVMVolumeDevice(device) || step.State != aj.Observed || len(step.VolIDs) != 1 || step.Target.Storage != target.StorageID || step.Target.Backing != target.BackingKey {
			return r.fail("recorded role binding is inconsistent")
		}
		volume := step.VolIDs[0]
		if devices[volume] != device {
			return r.fail("recorded role device changed")
		}
		storage, bare, e := pve.ParseDiskCID(volume)
		if e != nil || storage != target.StorageID {
			return r.fail("recorded role storage changed")
		}
		info, e := deps.PVE.Nodes().GetStorageContent(ctx, node, storage, bare)
		if e != nil || info == nil || info.Size <= 0 || uint64(info.Size) < target.VirtualBytes {
			return r.fail("recorded role volume size is insufficient")
		}
		drive, _ := pve.ConfigString(cfg, device)
		if (target.Role == "iso") != strings.Contains(drive, "media=cdrom") {
			return r.fail("recorded role media type changed")
		}
		if matched {
			return r.fail("multiple bindings claim the same role")
		}
		matched = true
	}
	if !matched {
		return r.fail("completed VM is missing a planned role binding")
	}
	return nil
}
func (r *managedVMRecordReadback) readVolumes(ctx context.Context) error {
	for volume := range r.recorded {
		if err := r.verifyVolume(ctx, volume); err != nil {
			return err
		}
		r.volumes = append(r.volumes, volume)
	}
	if r.record.State != aj.ReadyToReturn && r.record.State != aj.Adopted {
		return nil
	}
	if len(r.volumes) == 0 {
		return r.fail("completed VM lacks recorded volumes")
	}
	for i := range r.plan.Targets {
		if err := r.verifyRole(ctx, &r.plan.Targets[i]); err != nil {
			return err
		}
	}
	return nil
}
func (r *managedVMRecordReadback) read(ctx context.Context) error {
	if err := r.readTarget(); err != nil {
		return err
	}
	if err := r.readLocation(ctx); err != nil {
		return err
	}
	if err := r.readConfig(ctx); err != nil {
		return err
	}
	if err := r.readDefinitions(ctx); err != nil {
		return err
	}
	if err := r.readDevices(); err != nil {
		return err
	}
	return r.readVolumes(ctx)
}
