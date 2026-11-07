package handlers

import (
	"context"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// vmIdentity is what tells one VM at a VMID apart from a later VM at the same
// VMID: the SMBIOS UUID, the creation time PVE records in meta, and the VM
// generation ID. PVE writes all three when it creates or clones a VM, and a
// clone gets fresh values, so two VMs that held one VMID in turn differ on
// each. The post-destroy cleanups key on the VMID alone, so they check the
// identity first and leave a VMID that already belongs to another VM alone.
type vmIdentity struct {
	node  string
	vmid  int
	marks map[string]string
}

// vmIdentityMarkKinds lists the marks in the order the comparison trusts them.
// The SMBIOS UUID never changes for the life of a VM. The creation time does
// not either, but PVE releases before 7 do not record it. The generation ID
// changes on a snapshot rollback, so it decides only when neither of the
// others is on both configs.
var vmIdentityMarkKinds = []string{"smbios_uuid", "meta_ctime", "vmgenid"}

// captureVMIdentity records the identity of the VM whose config cfg is, on
// node.
func captureVMIdentity(node string, vmid int, cfg map[string]any) vmIdentity {
	return vmIdentity{node: node, vmid: vmid, marks: vmIdentityMarks(cfg)}
}

// vmIdentityMarks extracts the identity marks from a VM config. A mark that is
// absent or empty is left out.
func vmIdentityMarks(cfg map[string]any) map[string]string {
	marks := map[string]string{}
	if v, ok := pve.ConfigString(cfg, "smbios1"); ok {
		if uuid := propertyValue(v, "uuid"); uuid != "" {
			marks["smbios_uuid"] = strings.ToLower(uuid)
		}
	}
	if v, ok := pve.ConfigString(cfg, "meta"); ok {
		if ctime := propertyValue(v, "ctime"); ctime != "" {
			marks["meta_ctime"] = ctime
		}
	}
	if v, ok := pve.ConfigString(cfg, "vmgenid"); ok {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" && v != "0" && v != "1" {
			marks["vmgenid"] = v
		}
	}
	return marks
}

// propertyValue returns the value of key in a PVE property string such as
// "uuid=…,manufacturer=…", or "" when the string does not set it.
func propertyValue(prop, key string) string {
	for part := range strings.SplitSeq(prop, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// sameVMIdentity reports whether cfg is the config of the VM id names. The
// first mark both sides carry decides. With no mark in common the configs
// cannot be told apart, and that reads as not the same VM.
func (id vmIdentity) sameVMIdentity(cfg map[string]any) bool {
	current := vmIdentityMarks(cfg)
	for _, kind := range vmIdentityMarkKinds {
		want, ok := id.marks[kind]
		if !ok {
			continue
		}
		if got, ok := current[kind]; ok {
			return got == want
		}
	}
	return false
}

// vmIdentityReadConfig reads the config of vmid on node. It reports found
// false with a nil error when the node holds no config for the VMID.
func vmIdentityReadConfig(ctx context.Context, deps Deps, node string, vmid int) (map[string]any, bool, error) {
	holding, err := pve.ReadQemuHolding(ctx, deps.PVE, node, vmid)
	if err != nil {
		if pve.IsNotFound(err) || pve.IsPmxcfsConfigMissing(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return holding.Config, true, nil
}

// cleanupTargetStillOurs reports whether a post-destroy cleanup keyed on
// id.vmid may run. It may when the VMID holds no VM, which is the normal case
// after a destroy, and when it still holds the VM id names, which is the case
// while a destroy that was issued without an await is still running. It may
// not when the VMID now holds a different VM, or when the check cannot
// finish, because a cleanup run against a reused VMID would remove the agent
// state, HA rules, or routes of a VM this request never touched.
//
// The check reads the config on the node the VM was on. When that node holds
// none, it asks the cluster index where the VMID lives, and reads the config
// there when the index names another node. The index can lag behind a VM that
// was created on another node seconds ago. A create through this CPI cannot
// take the VMID while the caller holds its VMID claim, so that lag only
// matters for a VM made outside the CPI.
func cleanupTargetStillOurs(ctx context.Context, deps Deps, id vmIdentity, cleanup string, logger *log.Logger) bool {
	skip := func(reason string, err error) bool {
		fields := []log.Field{log.String("cleanup", cleanup), log.String("reason", reason)}
		if err != nil {
			fields = append(fields, log.Err(err))
		}
		logger.Warn("post-destroy cleanup skipped: the VMID is not proven to be free or to hold the destroyed VM",
			fields...)
		return false
	}
	if deps.PVE == nil || id.vmid <= 0 {
		return skip("no PVE client or VMID to check", nil)
	}
	node := id.node
	cfg, found, err := vmIdentityReadConfig(ctx, deps, node, id.vmid)
	if err != nil {
		return skip("the config read on the VM's node failed", err)
	}
	if !found {
		indexNode, indexed, lookErr := pve.FindVMNodeViaCluster(ctx, deps.PVE, id.vmid)
		if lookErr != nil {
			return skip("the cluster index lookup failed", lookErr)
		}
		if !indexed || indexNode == "" || indexNode == node {
			// No VM at the VMID, or only the destroyed VM's index row,
			// which lags behind the destroy on the node that held it.
			return true
		}
		node = indexNode
		cfg, found, err = vmIdentityReadConfig(ctx, deps, node, id.vmid)
		if err != nil {
			return skip("the config read on the node the index names failed", err)
		}
		if !found {
			return true
		}
	}
	if id.sameVMIdentity(cfg) {
		return true
	}
	return skip("the VMID now holds a different VM, or one whose identity cannot be compared", nil)
}

// runPostDestroyCleanup runs cleanup when cleanupTargetStillOurs allows it.
func runPostDestroyCleanup(
	ctx context.Context, deps Deps, id vmIdentity, name string, logger *log.Logger, cleanup func(),
) {
	if cleanupTargetStillOurs(ctx, deps, id, name, logger) {
		cleanup()
	}
}
