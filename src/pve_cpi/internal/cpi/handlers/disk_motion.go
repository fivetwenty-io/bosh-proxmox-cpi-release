package handlers

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

// The disk handlers share one shape under HA. Before they act on a VM they wait
// for it to settle, which follows a migration to the VM's new node and refuses
// a node the cluster does not report able to answer, and every config change
// then goes through pve.MutateVMConfig, which handles a migration that starts
// after the wait. The helpers here are that shared shape.
//
// With pve.ha.mode "off" none of them reads any HA state. The settle wait and
// the mutation protocol ignore the HA state in the scan row, the foreign-holder
// refusal leaves the holder's HA state out, and the node health check reads
// /cluster/status alone, with no manager status, status rows, or node clock. A
// node that is online in cluster status but fenced by HA reads as able to
// answer in that mode, and the call then meets the config-missing or lock
// answers that the mutation protocol handles.

// settleDiskVM waits until VM vmid is safe to act on and returns where it is
// now. It differs from settleVMForCall in one way. A VM that is absent from the
// cluster index after the wait comes back as Found=false with no error, because
// the disk handlers each have their own answer for a missing VM (an idempotent
// success, a VMNotFound, a skipped step) and each of those runs its own absence
// proof. The wait proves nothing about absence, so it never makes that answer.
//
// A VM that is found must sit on a node the cluster reports able to answer, or
// the call is a retriable error. A node in maintenance can answer.
//
// The handlers that call this have written nothing yet, so a cluster scan that
// keeps failing ends the wait after diskScanFailureBudget with a retriable
// error, not the minute that a lock earns.
func settleDiskVM(
	ctx context.Context, deps Deps, op, vmCID string, vmid int, probeFoundSettles bool,
) (pve.VMLocation, error) {
	if vmid <= 0 || deps.PVE == nil {
		return pve.VMLocation{}, nil
	}
	settled, err := pve.AwaitVMSettled(ctx, deps.PVE, deps.Log(ctx), pve.VMSettleRequest{
		VMID: vmid, MotionWait: haMotionWait(deps), Label: op, ProbeFoundSettles: probeFoundSettles,
		ScanFailureBudget: diskScanFailureBudget, IgnoreHAState: haStateUnread(deps),
	})
	if err != nil {
		return pve.VMLocation{}, cpierrors.Wrap(err, fmt.Sprintf("%s: wait for VM %s to settle", op, vmCID))
	}
	if !settled.Found || settled.Node == "" {
		return pve.VMLocation{}, nil
	}
	if err := requireNodeAnswers(ctx, deps, op, fmt.Sprintf("VM %s", vmCID), settled.Node); err != nil {
		return pve.VMLocation{}, err
	}
	return settled, nil
}

// diskScanFailureBudget is how long a disk handler's settle polls a cluster
// scan that keeps failing before it fails closed. A handler waits here before
// its first write, so a cluster API that is down costs the operator this much
// and no more, and the retry finds the cluster when it is back.
const diskScanFailureBudget = 15 * time.Second

// haStateUnread reports that pve.ha.mode is off, where the disk handlers read
// no HA state and defer on none. A deps with no config reads as HA on, the
// safer side.
func haStateUnread(deps Deps) bool {
	return deps.Config != nil && !deps.Config.HAWritesEnabled()
}

// diskHealthOptions are the node health inputs of the disk handlers. With
// pve.ha.mode off they read no HA source, so cluster status alone says whether
// a node can answer.
func diskHealthOptions(deps Deps) pve.NodeHealthOptions {
	opts := haHealthOptions(deps)
	opts.SkipHA = haStateUnread(deps)
	return opts
}

// requireNodeAnswers returns nil when the cluster reports node able to answer a
// call addressed to it, and a retriable error naming what for otherwise. A PVE
// client with no cluster or nodes service has nothing to ask, so it passes, the
// same way settleVMForCall passes it.
func requireNodeAnswers(ctx context.Context, deps Deps, op, what, node string) error {
	if deps.PVE == nil || deps.PVE.Cluster() == nil || deps.PVE.Nodes() == nil {
		return nil
	}
	report, err := pve.NodeHealth(ctx, deps.PVE.Cluster(), deps.PVE.Nodes(), diskHealthOptions(deps))
	if err != nil {
		return cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"%s: %s is on node %s, and the cluster status could not be read to confirm that node can answer",
			op, what, node))
	}
	if report.ProxyTarget(node) {
		return nil
	}
	reason := "the cluster does not list it"
	if entry, ok := report.Node(node); ok && entry.Reason != "" {
		reason = entry.Reason
	}
	return cpierrors.Retriable(
		"%s: %s is on node %s, which the cluster does not report able to answer (%s); retry once it can",
		op, what, node, reason)
}

// diskVMMutation is one VM-config change a disk handler sends through
// pve.MutateVMConfig.
type diskVMMutation struct {
	op    string
	vmCID string
	vmid  int
	// node is the node the caller last resolved the VM on, usually from
	// settleDiskVM.
	node string
	// apply sends the change to the node it is given, once. It must be safe to
	// run again on another node, so it re-reads whatever it chose from a
	// config, such as a slot, rather than keeping the choice from before.
	apply func(ctx context.Context, node string) error
	// landed reports whether the change is already in the config on node. It is
	// what stops a change whose answer was lost from being sent twice. Nil means
	// an ambiguous answer is returned as a retriable error.
	landed func(ctx context.Context, node string) (bool, error)
	// reresolve re-resolves every other identity the change names, and checks
	// that the storage it uses still fits the VM's node. Nil means it names no
	// other identity.
	reresolve func(ctx context.Context, vm pve.VMLocation) error
	// budget caps the time the mutation spends. Zero leaves the protocol's own
	// bound, which is twice the motion wait. A caller that holds a lock for
	// the mutation sets it, so a longer context deadline cannot lengthen the
	// hold.
	budget time.Duration
	// ownsOutcome is set by a change that is a protocol of its own, such as a
	// disk transfer between VMs, which resumes from its own record and gives a
	// definite answer for each way it ends. The protocol would read a timeout
	// or lock word in such an answer as a lost reply and send the transfer
	// again, so only a VM config lock or a missing config is handed to it.
	// Every other answer ends the mutation and comes back to the caller as the
	// change gave it.
	ownsOutcome bool
}

// mutateDiskVM sends m through pve.MutateVMConfig and returns the node the
// change landed on. A failure comes back as the protocol gave it, and the
// caller maps a not-found answer with diskMutationFailure.
func mutateDiskVM(ctx context.Context, deps Deps, m diskVMMutation) (string, error) {
	lastNode := m.node
	var own error
	loc, err := pve.MutateVMConfig(ctx, deps.PVE, deps.Log(ctx), pve.VMConfigMutation{
		VMID:          m.vmid,
		Node:          m.node,
		MotionWait:    haMotionWait(deps),
		Label:         m.op,
		Budget:        m.budget,
		IgnoreHAState: haStateUnread(deps),
		Apply: func(ctx context.Context, at string) error {
			lastNode = at
			applyErr := m.apply(ctx, at)
			if applyErr != nil && m.ownsOutcome && !pve.IsVMConfigLocked(applyErr) && !pve.IsPmxcfsConfigMissing(applyErr) {
				own = applyErr
				return nil
			}
			return applyErr
		},
		Landed:    m.landed,
		Reresolve: m.reresolve,
	})
	if err != nil {
		return lastNode, err
	}
	if own != nil {
		return lastNode, own
	}
	if loc.Node != "" {
		return loc.Node, nil
	}
	return lastNode, nil
}

// diskMutationFailure maps the error of a mutation or of a call to the VM's
// node to the handler's answer. A raw not-found or config-missing answer speaks
// for the node that gave it alone, and the VM may have moved, so it is
// VMNotFound only when the absence proof finds the VM nowhere, and a VM found
// elsewhere is a retriable error. That holds for a 404 that pve.WrapError has
// already typed as a permanent error, because its cause still says 404 and the
// type does not say the VM is gone. Only an error that is already VMNotFound or
// DiskNotFound is a settled answer and passes through without the proof. Every
// other error is already typed or is the caller's to wrap, and it returns
// unchanged.
func diskMutationFailure(ctx context.Context, deps Deps, op, vmCID string, vmid int, node, step string, err error) error {
	if isConfigMissing(err) && !cpierrors.IsNotFound(err) {
		if goneErr := confirmVMGone(ctx, deps, op, vmid, node, step); goneErr != nil {
			return goneErr
		}
		return cpierrors.VMNotFound(vmCID)
	}
	return err
}

// storageFitsVMNode returns nil when a volume on the storage that volid names
// is reachable from node, and a retriable error otherwise. It is the storage
// half of what a re-resolve checks after a VM moved, because a change that
// names a volume must not be sent to a node that cannot see the volume. A
// shared storage fits any node it is not restricted away from. A node-local
// storage fits only the node the volume lives on, which is homeNode.
//
// The storage index is read live, because the answer decides whether a write
// goes out. A read that fails is a retriable error.
func storageFitsVMNode(ctx context.Context, deps Deps, op, volid, homeNode, node string) error {
	storage, _, err := pve.ParseDiskCID(volid)
	if err != nil {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: read the storage of volume %q", op, volid))
	}
	info, err := pve.LiveStorageInfo(ctx, deps.PVE, storage)
	if err != nil {
		return cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"%s: the VM moved to node %s, and storage %s could not be read to check that the node reaches volume %s",
			op, node, storage, volid))
	}
	switch {
	case info.Disabled:
		return cpierrors.Retriable("%s: the VM moved to node %s, and storage %s is disabled; retry once it is enabled",
			op, node, storage)
	case info.IsShared():
		if len(info.Nodes) > 0 && !containsNode(info.Nodes, node) {
			return cpierrors.Retriable(
				"%s: the VM moved to node %s, which storage %s does not reach (it serves %s); retry once the VM is "+
					"on a node the storage reaches", op, node, storage, strings.Join(info.Nodes, ", "))
		}
	case homeNode != "" && homeNode != node:
		return cpierrors.Retriable(
			"%s: the VM moved to node %s, but volume %s lives on node %s of node-local storage %s; retry once the VM "+
				"is back on that node", op, node, volid, homeNode, storage)
	}
	return nil
}

func containsNode(nodes []string, node string) bool {
	for _, n := range nodes {
		if n == node {
			return true
		}
	}
	return false
}

// settleTransferIdentities makes the two VMs of an interrupted transfer safe to
// act on before the resume runs. The resume moves the disk from its source VM
// to the parker, and PVE's move_disk needs both VMs on one node, so a source or
// a parker that migrated since the record was written would send the resume to
// a node that no longer holds one of them. Each VM is settled and located
// again. The parker's node in the intent is then replaced by where the parker
// is now, on a copy, so the journal's record is left as it is. When the source
// and the parker are found on different nodes the resume cannot run, and the
// call is a retriable error.
//
// A VM that only the config probe finds, with no row in the cluster index, ends
// the wait at once. The resume ran before this wait existed against exactly
// that shape (a source missing from the guest listings is how it meets an
// interrupted transfer), so waiting a minute for a row that may never come
// would turn every such resume into a retriable failure. A row that does exist
// is still read for its lock, its HA state, and its node.
//
// A VM that the wait does not find is left to the resume: a parker the
// interrupted transfer never created is not in the index yet, and a source VM
// that was deleted since is the case the resume already handles.
func settleTransferIdentities(ctx context.Context, deps Deps, op string, rd resolvedDisk) (resolvedDisk, error) {
	intent := rd.intent
	if intent == nil {
		return rd, nil
	}
	var sourceNode string
	if source, ok := intentSourceVMID(intent); ok {
		loc, err := settleDiskVM(ctx, deps, op, intent.SourceVMCID, source, true)
		if err != nil {
			return resolvedDisk{}, err
		}
		sourceNode = loc.Node
	}
	parker, err := settleDiskVM(ctx, deps, op, fmt.Sprintf("%d", intent.ParkerVMID), intent.ParkerVMID, true)
	if err != nil {
		return resolvedDisk{}, err
	}
	if parker.Node == "" {
		return rd, nil
	}
	if sourceNode != "" && sourceNode != parker.Node {
		return resolvedDisk{}, cpierrors.Retriable(
			"%s: the interrupted transfer of disk %s moves it from VM %s on node %s to parker %d on node %s, and PVE "+
				"moves a disk only between VMs on one node; retry once they are on the same node",
			op, rd.diskCID, intent.SourceVMCID, sourceNode, intent.ParkerVMID, parker.Node)
	}
	if parker.Node != intent.ParkerNode {
		deps.Log(ctx).Info(op+": the parker of an interrupted transfer moved; resuming on its node",
			log.Int("parker_vmid", intent.ParkerVMID),
			log.String("recorded_node", intent.ParkerNode),
			log.String("node", parker.Node))
		moved := *intent
		moved.ParkerNode = parker.Node
		rd.intent = &moved
	}
	return rd, nil
}

// resumeTransferSettled is resumeTransferIfNeeded after the transfer's VMs have
// settled and been located again. A disk with no transfer in flight pays
// nothing.
func resumeTransferSettled(ctx context.Context, deps Deps, op string, rd resolvedDisk) (resolvedDisk, error) {
	if rd.intent == nil {
		return rd, nil
	}
	settled, err := settleTransferIdentities(ctx, deps, op, rd)
	if err != nil {
		return resolvedDisk{}, err
	}
	return resumeTransferIfNeeded(ctx, deps, op, settled)
}

// transferredVolidOn reads the config of the VM on node and returns the volume
// of the drive that carries stableID as its serial. It answers whether a
// reassignment onto the VM already landed, which is what a move that is sent
// again must know first, because a move that landed leaves the parker nothing
// to give. A disk without a stable ID has no serial to look for and reports
// not found.
func transferredVolidOn(ctx context.Context, deps Deps, node string, vmid int, stableID string) (string, bool, error) {
	if stableID == "" {
		return "", false, nil
	}
	cfg, err := deps.PVE.QEMU().Config(ctx, node, vmid)
	if err != nil {
		return "", false, err
	}
	disks := qemu.ParseDisks(cfg)
	keys := make([]string, 0, len(disks))
	for key := range disks {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if strings.HasPrefix(key, "unused") {
			continue
		}
		if serial, has := pve.StableIDFromDriveOptStr(disks[key]); has && serial == stableID {
			volid, _, _ := strings.Cut(disks[key], ",")
			return volid, true, nil
		}
	}
	return "", false, nil
}

// holderUnavailable says why a VM that holds a disk cannot be acted on now,
// or returns "" when nothing is known against it. It looks for the two things
// the plan treats as a holder that is down or being recovered: an HA state of
// fence or recovery, and a node the cluster does not report online. With
// pve.ha.mode off it leaves the HA state out and reads the node alone. A lookup
// that fails returns no reason for its half, because the refusal that calls
// this keeps its meaning without the extra detail, and the failure is logged at
// debug so a refusal that reads as a plain foreign-holder refusal can be traced.
func holderUnavailable(ctx context.Context, deps Deps, holder pve.DiskHolder) string {
	var reasons []string
	node := holder.Node
	loc, err := findVMAuthoritative(ctx, deps, holder.VMID)
	switch {
	case err != nil:
		deps.Log(ctx).Debug("disk handler: could not look up the holder of a disk, so its HA state is not named",
			log.Int("holder_vmid", holder.VMID), log.Err(err))
	case loc.Found:
		if loc.Node != "" {
			node = loc.Node
		}
		if !haStateUnread(deps) {
			switch state := strings.ToLower(strings.TrimSpace(loc.HAState)); state {
			case "fence", "recovery":
				reasons = append(reasons, fmt.Sprintf("its HA state is %s", state))
			}
		}
	}
	if node != "" && deps.PVE != nil && deps.PVE.Cluster() != nil && deps.PVE.Nodes() != nil {
		report, err := pve.NodeHealth(ctx, deps.PVE.Cluster(), deps.PVE.Nodes(), diskHealthOptions(deps))
		if err != nil {
			deps.Log(ctx).Debug("disk handler: could not read node health for the holder of a disk",
				log.Int("holder_vmid", holder.VMID), log.String("node", node), log.Err(err))
		} else {
			entry, listed := report.Node(node)
			if !listed || !entry.ProxyTarget {
				reason := "the cluster does not list it"
				if listed && entry.Reason != "" {
					reason = entry.Reason
				}
				reasons = append(reasons, fmt.Sprintf("node %s is not online (%s)", node, reason))
			}
		}
	}
	return strings.Join(reasons, " and ")
}

// retryOnTransientOrLockUntilMissing is pve.RetryOnTransientOrLock that ends at
// the first config-missing answer instead of retrying it. PVE answers a call
// about a VM that left the node with a 500, and the retry loop reads any 500 as
// a transport fault, so a loop run inside pve.MutateVMConfig would spend its
// whole backoff sending to a node that no longer holds the VM before the
// mutation could settle and follow the VM. The answer comes back to the caller
// unchanged, for pve.MutateVMConfig to act on.
func retryOnTransientOrLockUntilMissing(
	ctx context.Context, logger *log.Logger, label string, maxAttempts int, op func() error,
) error {
	var missing error
	err := pve.RetryOnTransientOrLock(ctx, logger, label, maxAttempts, func() error {
		e := op()
		if e != nil && pve.IsPmxcfsConfigMissing(e) {
			missing = e
			return nil
		}
		return e
	})
	if err != nil {
		return err
	}
	return missing
}
