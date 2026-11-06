package handlers

import (
	"context"
	"fmt"
	"slices"
	"strings"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// findVMAuthoritative is pve.FindVMAuthoritative with the allocation journal
// as a source that can tie the guest to an offline member. Every handler that
// locates a VM through deps.PVE uses it, so the absence answer it gives on an
// index miss rests on the same evidence everywhere: two clean sweeps of every
// online member, no HA resource or CRM service for the VMID, and no open
// journal record placing the VMID on a member the sweeps had to skip.
func findVMAuthoritative(ctx context.Context, deps Deps, vmid int) (pve.VMLocation, error) {
	return pve.FindVMAuthoritative(ctx, deps.PVE, vmid, pve.WithOfflineMemberTie(journalOfflineMemberTie(deps.Config)))
}

// journalOfflineMemberTie returns the allocation journal's answer to
// pve.OfflineMemberTie, or nil when the journal is not configured. A journal
// that is configured but not yet enrolled holds no records and ties nothing.
// The read takes no locks, so it is safe on a path that already holds an
// allocation handle.
func journalOfflineMemberTie(cfg *config.CPIConfig) pve.OfflineMemberTie {
	if cfg == nil {
		return nil
	}
	directory := strings.TrimSpace(cfg.StorageAllocationJournalDir)
	namespace := strings.TrimSpace(cfg.StoragePlacementNamespace)
	if directory == "" || namespace == "" {
		return nil
	}
	return func(_ context.Context, vmid int, offline []string) (string, string, error) {
		records, enrolled, err := readAllocationJournalRecords(directory, namespace)
		if err != nil {
			return "", "", err
		}
		if !enrolled {
			return "", "", nil
		}
		member, detail := journalRecordOnOfflineMember(records, vmid, offline)
		return member, detail, nil
	}
}

// journalRecordOnOfflineMember finds an open record with a step that targets
// vmid on one of the offline members. A record that is closed, by delete,
// cleanup, or retention after the VM's delete, no longer says the VM exists,
// and a step marked external records work on another allocation's resource,
// never a VM this record owns. Every other step counts, a planned one
// included, because a create planned on a member just before it went dark may
// have run there.
func journalRecordOnOfflineMember(records []aj.Record, vmid int, offline []string) (member, detail string) {
	for i := range records {
		rec := &records[i]
		switch rec.State {
		case aj.Deleted, aj.Cleaned, aj.VMDeletedRetained:
			continue
		}
		for j := range rec.Steps {
			step := &rec.Steps[j]
			if step.Target.External || step.Target.VMID != vmid || !slices.Contains(offline, step.Target.Node) {
				continue
			}
			return step.Target.Node, fmt.Sprintf("allocation journal record %s (%s, %s, step %s)",
				rec.ID, rec.Kind, rec.State, step.Kind)
		}
	}
	return "", ""
}

// requireProxyTarget returns nil when PVE's cluster view says node can answer
// a call addressed to it. A guest config read goes to the node that owns the
// guest, so its config-missing answer speaks for that node alone, and only a
// node the cluster reports online, unfenced, and present can give one worth
// acting on. Any other node, or a cluster view that cannot be read, is a
// retriable error.
func requireProxyTarget(ctx context.Context, deps Deps, op string, vmid int, node string) error {
	if deps.PVE == nil || deps.PVE.Cluster() == nil || deps.PVE.Nodes() == nil {
		return cpierrors.Retriable(
			"%s: VM %d config is missing on node %s, and no cluster client is available to confirm that node answers for "+
				"its guests", op, vmid, node)
	}
	report, err := pve.NodeHealth(ctx, deps.PVE.Cluster(), deps.PVE.Nodes(), pve.NodeHealthOptions{})
	if err != nil {
		return cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"%s: VM %d config is missing on node %s, and the cluster status could not be read to confirm that node "+
				"answers for its guests", op, vmid, node))
	}
	if !report.ProxyTarget(node) {
		reason := "the cluster does not list it"
		if entry, ok := report.Node(node); ok && entry.Reason != "" {
			reason = entry.Reason
		}
		return cpierrors.Retriable(
			"%s: VM %d config is missing on node %s, but the cluster does not report that node able to answer for its "+
				"guests (%s); retry once it is", op, vmid, node, reason)
	}
	return nil
}

// locateAfterConfigMissing locates VM vmid again after a read on node, the
// node a scan or an earlier read placed it on, answered not found or config
// missing. That answer proves only that node holds no config for the VM now:
// the VM may have migrated since, and the index row that named node may be
// the stale one. So node must first be one the cluster reports able to
// answer, and then pve.FindVMByConfigProbe sweeps every member, skipping the
// contradicted index row. A hit is where the VM is now, and Found=false is a
// full absence proof. Every failure, a member the cluster reports offline
// included, is a retriable error.
func locateAfterConfigMissing(ctx context.Context, deps Deps, op string, vmid int, node string) (pve.VMLocation, error) {
	if err := requireProxyTarget(ctx, deps, op, vmid, node); err != nil {
		return pve.VMLocation{}, err
	}
	loc, err := pve.FindVMByConfigProbe(ctx, deps.PVE, vmid)
	if err != nil {
		return pve.VMLocation{}, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"%s: VM %d config is gone from node %s, and locating the VM again failed", op, vmid, node))
	}
	if loc.Found && loc.Node == "" {
		return pve.VMLocation{}, cpierrors.Retriable(
			"%s: VM %d config is gone from node %s, and the member that holds it now could not be named", op, vmid, node)
	}
	return loc, nil
}

// confirmVMGone is the answer of a delete step on node that came back not
// found or config missing, as a stop, a destroy, a task await, or a config
// read. It returns nil only when the VM is proven absent from the cluster, so
// the step may finish as an idempotent success. A VM found anywhere, on node
// again or on another member it moved to, is a retriable error, so the
// Director retries the delete and the next attempt locates the VM afresh
// instead of answering success for a VM that still runs. step names what came
// back missing, for the error text.
func confirmVMGone(ctx context.Context, deps Deps, op string, vmid int, node, step string) error {
	loc, err := locateAfterConfigMissing(ctx, deps, op, vmid, node)
	if err != nil {
		return err
	}
	if loc.Found {
		return cpierrors.Retriable(
			"%s: %s for VM %d on node %s answered that the VM is missing, but node %s holds its config now; retry so "+
				"the delete runs where the VM is", op, step, vmid, node, loc.Node)
	}
	return nil
}
