package handlers

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/vmcid"
)

// attachHAOp prefixes the errors and logs of the attach-time HA decision.
const attachHAOp = "attach_disk"

// attachHAAction is what the attach-time HA decision makes the attach do.
type attachHAAction int

const (
	// attachHANone attaches as before: the VM is not HA-managed by this
	// Director, or the disk's storage is already in its pin's volume record.
	attachHANone attachHAAction = iota

	// attachHALeave removes the VM from PVE HA and waits until the CRM lets go
	// of it before the attach, because the VM may no longer run under HA with
	// the new disk. It also finishes a removal an earlier attach left part
	// way, whose HA resource is gone while the VM's pin is still there.
	attachHALeave

	// attachHAGuarded runs the attach with the pin the decision gives for the
	// VM's volumes and the new disk. A pin that narrows where the VM may run
	// is written inside the narrowing barrier, before the attach. A pin that
	// does not narrow is written after the attach.
	attachHAGuarded
)

// attachHA is the attach-time HA decision for one attach_disk call. It is made
// before the attach's first mutation, the resume of an interrupted transfer
// included, and it only reads.
type attachHA struct {
	deps     Deps
	logger   *log.Logger
	vmCID    string
	disk     resolvedDisk
	vmid     int
	sid      string
	node     string
	action   attachHAAction
	profile  *ha.Profile
	decision *ha.VMDecision
	oldPin   *ha.Rule
	newPin   *ha.Rule

	// narrow holds the reads and writes of the barrier and of the removal
	// from HA, wired to the VM's client.
	narrow ha.NarrowDeps
}

// decideAttachHA makes the attach-time HA decision for attaching the disk rd
// to the VM vmCID. Mode off reads nothing. A VM whose HA resource is missing,
// or carries no marker of this Director, gets no HA action, because the CPI
// may change only what it owns. A VM whose pin already records the disk's
// storage needs no change, unless an earlier barrier stopped part way. Every
// other attach to an HA-managed VM needs the profile to let the CPI write HA
// state, and otherwise it returns a retriable error that names what is
// missing, so the attach never adds a volume the VM's confinement does not
// account for. The decision then runs over the
// VM's volumes and the new disk:
//
//   - A failing decision returns its error.
//   - A decision that gives the VM no HA, such as a local disk under
//     pve.ha.local_policy none or a narrowed set with fewer than two
//     placeable nodes, makes the attach remove the VM from HA first.
//   - A decision that registers the VM makes the attach run inside the
//     narrowing barrier with the pin it gives.
//
// Every read failure is retriable.
func decideAttachHA(ctx context.Context, deps Deps, vmCID string, rd resolvedDisk) (*attachHA, error) {
	const op = attachHAOp
	if deps.Config == nil || deps.Config.HAModeValue() == config.HAModeOff || deps.PVE == nil {
		return nil, nil
	}
	parsed, err := vmcid.Parse(vmCID)
	if err != nil {
		// The node lookup that follows answers VMNotFound for the same CID.
		return nil, nil
	}
	vmid := parsed.VMID
	sid := haResourceSid(vmid)
	logger := deps.Log(ctx)
	owner := deps.RequestDirectorUUID
	svc := deps.PVE.Cluster()

	res, err := svc.GetHaResources(ctx, sid)
	switch {
	case err != nil && (isHaNotFound(err) || pve.IsNotFound(err)):
		return pendingAttachLeave(ctx, deps, vmCID, vmid, rd)
	case err != nil:
		return nil, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: read the HA resource of VM %d", op, vmid))
	case res == nil:
		return nil, cpierrors.Retriable("%s: the read of the HA resource of VM %d returned nothing", op, vmid)
	}
	comment := ""
	if res.Comment != nil {
		comment = *res.Comment
	}
	if owner == "" || haOwnershipOf(comment, owner) != haOwnedByUs {
		logger.Info(op+": the VM's HA resource is not this Director's, so the attach changes nothing in HA",
			log.Int(metadataKeyVMID, vmid))
		return nil, nil
	}

	storage, _, ok := strings.Cut(rd.volid, ":")
	if !ok || storage == "" {
		return nil, cpierrors.DiskNotFound(rd.volid)
	}
	set, err := readHARuleSet(ctx, svc)
	if err != nil {
		return nil, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: read the HA rules for VM %d", op, vmid))
	}
	oldPin := set.Find(naRuleNameFor(vmid))
	if oldPin != nil && haOwnershipOf(oldPin.Comment, owner) == haOwnedByOther {
		return nil, cpierrors.Cloud("%s: the pin %s of VM %d carries another Director's ownership marker", op,
			oldPin.ID, vmid)
	}
	// A barrier an earlier attach left part way must be finished, so only a
	// resource with no pending barrier skips the decision. A legacy disk in
	// managed mode never skips it, because the decision is what refuses a
	// disk with no stable ID there.
	legacyManaged := rd.stableID == "" && deps.Config.HAModeValue() == config.HAModeManaged
	if oldPin != nil && !legacyManaged && !ha.NarrowPending(res.State, res.Comment) {
		if recorded, ok := ha.VolumeRecord(oldPin.Comment); ok && slices.Contains(recorded, storage) {
			return nil, nil
		}
	}

	profile, err := resolveHAProfile(ctx, deps)
	if err != nil {
		return nil, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: resolve the HA profile for VM %d", op, vmid))
	}
	if err := attachHAWritable(profile, vmid); err != nil {
		return nil, err
	}

	loc, err := pve.FindVMAuthoritative(ctx, deps.PVE, vmid)
	if err != nil {
		return nil, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: locate VM %d for its HA decision", op, vmid))
	}
	if !loc.Found || loc.Node == "" {
		return nil, cpierrors.VMNotFound(vmCID)
	}
	cfg, err := deps.PVE.QEMU().Config(ctx, loc.Node, vmid)
	if err != nil {
		return nil, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: read the config of VM %d for its HA decision", op, vmid))
	}

	in := attachHAInput(deps, vmid, loc.Node, cfg, res, oldPin, rd)
	d, err := profile.DecideVM(in)
	if err != nil {
		return nil, err
	}
	for _, w := range d.Warnings {
		logger.Warn(op+": "+w, log.Int(metadataKeyVMID, vmid))
	}
	h := &attachHA{
		deps: deps, logger: logger, vmCID: vmCID, disk: rd, vmid: vmid, sid: sid, node: loc.Node,
		profile: profile, decision: d, oldPin: oldPin, narrow: ha.NarrowDepsFor(deps.PVE, logger, haMotionWait(deps)),
	}
	switch {
	case d.Outcome == ha.OutcomeFail:
		if failure := d.Failure(); failure != nil {
			return nil, failure
		}
		return nil, cpierrors.Cloud("%s: VM %d: the HA decision failed the call: %s", op, vmid, d.Reason)
	case d.PlaceableShortfall:
		// Enough nodes reach the VM's volumes, and the VM misses full HA only
		// because some of them are unhealthy or in maintenance now. A VM that
		// already runs under HA does not leave HA, or get pinned to one node,
		// for an outage, so the attach waits for the nodes to come back.
		return nil, cpierrors.Retriable("%s: VM %d is HA-managed, and nodes %s reach all of its volumes with the "+
			"new disk, but fewer than two of them can take the VM now; the attach waits until enough of them "+
			"are healthy again rather than take the VM out of HA for a temporary outage", op, vmid,
			strings.Join(d.Access, ","))
	case d.Outcome == ha.OutcomeRegister:
		newPin, err := attachNewPin(d, oldPin, profile, set, ha.VolumeStorages(in.Volumes), owner)
		if err != nil {
			return nil, err
		}
		h.action, h.newPin = attachHAGuarded, newPin
	default:
		logger.Warn(op+": the VM leaves PVE HA before the attach, because the HA decision gives it no HA with "+
			"the new disk", log.Int(metadataKeyVMID, vmid), log.String("disk", rd.volid),
			log.String("reason", d.Reason))
		h.action = attachHALeave
	}
	return h, nil
}

// pendingAttachLeave finishes, on the next attach, a removal from HA that an
// earlier attach left part way. That attach removed the VM's HA resource and
// then failed before it removed the VM's pin, which goes last, so a VM with no
// HA resource that still has this Director's pin is a pending leave. Mode off
// reaches nothing here. A rules read that fails, or a pin that is missing or
// not this Director's, gives no HA action, because a pin that names no
// resource confines nothing and delete_vm removes it.
func pendingAttachLeave(ctx context.Context, deps Deps, vmCID string, vmid int, rd resolvedDisk) (*attachHA, error) {
	const op = attachHAOp
	owner := deps.RequestDirectorUUID
	if owner == "" {
		return nil, nil
	}
	logger := deps.Log(ctx)
	set, err := readHARuleSet(ctx, deps.PVE.Cluster())
	if err != nil {
		logger.Debug(op+": the HA rules could not be read to look for a pin left by an earlier removal from HA",
			log.Int(metadataKeyVMID, vmid), log.Err(err))
		return nil, nil
	}
	pin := set.Find(naRuleNameFor(vmid))
	if pin == nil || haOwnershipOf(pin.Comment, owner) != haOwnedByUs {
		return nil, nil
	}
	loc, err := pve.FindVMAuthoritative(ctx, deps.PVE, vmid)
	if err != nil {
		return nil, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: locate VM %d to finish its removal from HA", op, vmid))
	}
	if !loc.Found || loc.Node == "" {
		return nil, cpierrors.VMNotFound(vmCID)
	}
	logger.Warn(op+": the VM's HA resource is gone but its pin is still there, so the attach finishes its "+
		"removal from HA first", log.Int(metadataKeyVMID, vmid), log.String("pin", pin.ID))
	return &attachHA{
		deps: deps, logger: logger, vmCID: vmCID, disk: rd, vmid: vmid, sid: haResourceSid(vmid), node: loc.Node,
		action: attachHALeave, oldPin: pin, narrow: ha.NarrowDepsFor(deps.PVE, logger, haMotionWait(deps)),
	}, nil
}

// attachHAWritable returns the retriable error of an attach that must change a
// VM's HA state when the profile does not let the CPI write it. It names the
// Sys.Console privilege when the token lacks it, and the profile's first gap
// otherwise.
func attachHAWritable(profile *ha.Profile, vmid int) error {
	const op = attachHAOp
	if profile.WritesAllowed() {
		return nil
	}
	if !profile.SysConsole {
		return cpierrors.Retriable("%s: VM %d is HA-managed and the new disk changes where it may run, but the "+
			"token lacks the Sys.Console privilege on / that the HA state change needs", op, vmid)
	}
	reason := profile.Reason
	if reason == "" {
		reason = strings.Join(profile.Gaps, "; ")
	}
	return cpierrors.Retriable("%s: VM %d is HA-managed and the new disk changes where it may run, but the CPI "+
		"may not write HA state: %s", op, vmid, reason)
}

// readHARuleSet reads and parses the cluster's HA rules.
func readHARuleSet(ctx context.Context, svc cluster.Service) (*ha.RuleSet, error) {
	resp, err := svc.ListHaRules(ctx, &cluster.ListHaRulesParams{})
	if err != nil {
		return nil, pve.WrapError(err)
	}
	return ha.ParseRuleList(resp)
}

// attachHAInput builds the decision input of an attach: the VM's volumes and
// the new disk, homed on the VM's node because the attach puts a node-local
// disk there. A strict pin is a constraint, so the decision never widens the
// set the VM may run on, and a PCI passthrough device is one too. A pin that
// is not strict, such as the record pin or the placed-node pin a cluster with
// ha-rebalance-on-start gets, states a preference and confines nothing, so it
// is no constraint, and only the volumes' access set bounds the decision. A VM
// whose resource rebalances automatically is a DLB VM. The existing volumes
// were judged when the VM registered, so only the new disk can be legacy.
//
// The input carries no CRM state, so the decision has no DecisionUID, and the
// rule writer's repair stays fail-closed at attach. A pin repair that cannot
// be verified inside the barrier then returns a retriable error and leaves the
// resource ignored with its token, which the next attach finishes, rather
// than removing a resource the CRM is acting on.
func attachHAInput(
	deps Deps, vmid int, node string, cfg map[string]any, res *cluster.GetHaResourcesResponse, oldPin *ha.Rule,
	rd resolvedDisk,
) ha.VMInput {
	volumes := haVolumesOfConfig(cfg, node, nil)
	if !slices.ContainsFunc(volumes, func(v ha.VMVolume) bool { return v.Volume == rd.volid }) {
		volumes = append(volumes, ha.VMVolume{
			Volume: rd.volid, HomeNode: node, Kind: ha.VolumeDisk, Legacy: rd.stableID == "",
		})
	}
	liveCPU, _ := pve.ConfigString(cfg, "cpu")
	in := ha.VMInput{
		Config:  deps.Config,
		VMID:    vmid,
		Node:    node,
		Volumes: volumes,
		CPUType: ha.EffectiveCPUType(deps.Config, nil, liveCPU),
		InDLBAZ: res.AutoRebalance != nil && res.AutoRebalance.Bool(),
	}
	if oldPin != nil && oldPin.Strict && !oldPin.Disabled {
		in.Constraints = append(in.Constraints, ha.NodeConstraint{Name: "the VM's pin " + oldPin.ID,
			Nodes: oldPin.NodeNames()})
	}
	if configHasPCIPassthrough(cfg) {
		in.Constraints = append(in.Constraints, ha.NodeConstraint{Name: pciConstraintName, Nodes: []string{node}})
	}
	return in
}

// configHasPCIPassthrough reports whether a VM config carries a hostpci entry.
func configHasPCIPassthrough(cfg map[string]any) bool {
	for key := range cfg {
		if strings.HasPrefix(key, "hostpci") {
			return true
		}
	}
	return false
}

// attachNewPin returns the pin the VM needs with the new disk. A strict pin
// that is in effect keeps its priorities and loses the nodes the decision no
// longer admits, and it stays strict. Any other pin, and a VM with no pin, gets
// the decision's pin, which is what create_vm writes for the same volumes, so
// a pin that only stated a preference is never made strict by itself. The
// decision's pin is strict only when the volumes' access set, a PCI device, or
// the CPU domain confines the VM. The record names every storage the VM's
// volumes use. A pin that would name no node is a retriable error.
func attachNewPin(
	d *ha.VMDecision, oldPin *ha.Rule, profile *ha.Profile, set *ha.RuleSet, storages []string, owner string,
) (*ha.Rule, error) {
	ruleName := naRuleNameFor(d.VMID)
	configured := profileNodeNames(profile)
	if oldPin == nil || !oldPin.Strict || oldPin.Disabled {
		return ha.DecisionPin(d, ruleName, haIdentity{Owner: owner}.marker(), storages, configured,
			profileRebalancesOnStart(profile), inResourceAffinityRule(set, d.SID))
	}
	if len(storages) == 0 {
		return nil, cpierrors.Cloud("pin %s: the VM holds no volume to record", ruleName)
	}
	pin := *oldPin
	pin.Nodes = maps.Clone(oldPin.Nodes)
	pin.Resources = slices.Clone(oldPin.Resources)
	for n := range pin.Nodes {
		if !slices.Contains(d.Admissible, n) {
			delete(pin.Nodes, n)
		}
	}
	if len(pin.Nodes) == 0 {
		return nil, cpierrors.Retriable("pin %s: no node of the VM's pin is admissible with the new disk", ruleName)
	}
	pin.Comment = ha.WithVolumeRecord(oldPin.Comment, storages)
	pin.Errors = nil
	return &pin, nil
}

// run runs attach, the rest of the attach, as the HA decision requires, and
// returns its result. attach returns the node the VM is on when it is done.
// The attach's read-only refusals run first, before anything changes in HA,
// so an attach they refuse leaves the VM's HA state, pin, and tag as they
// were (see preflight).
func (h *attachHA) run(ctx context.Context, attach func(context.Context) (string, error)) error {
	const op = attachHAOp
	if h == nil || h.action == attachHANone {
		_, err := attach(ctx)
		return err
	}
	if err := h.preflight(ctx); err != nil {
		return err
	}
	narrowDeps := h.narrow
	owned := func(comment string) bool {
		return haOwnershipOf(comment, h.deps.RequestDirectorUUID) == haOwnedByUs
	}
	if h.action == attachHALeave {
		loc, err := ha.LeaveHA(ctx, narrowDeps, h.vmid, h.node, haMotionWait(h.deps), owned)
		if err != nil {
			return err
		}
		// The pin goes last, so a removal that stops part way leaves the pin,
		// and the next attach finds it and finishes (see pendingAttachLeave).
		label := op + ": remove the HA-managed tag"
		if err := removeHAManagedTag(ctx, h.deps, loc.Node, h.vmid, label, h.logger); err != nil {
			return err
		}
		if err := removeNodeAffinityPin(ctx, h.deps, h.vmid, h.logger); err != nil {
			return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("%s: remove the pin of VM %d after it left HA", op, h.vmid))
		}
		_, err = attach(ctx)
		return err
	}
	// The barrier's mutation is the whole rest of the attach, which locates the
	// VM itself, so the node the drain found is only where it starts.
	var landed string
	narrowDeps.Mutate = func(ctx context.Context, m pve.VMConfigMutation) (pve.VMLocation, error) {
		if err := m.Apply(ctx, m.Node); err != nil {
			return pve.VMLocation{}, err
		}
		return pve.VMLocation{Node: landed, Found: true}, nil
	}
	_, err := ha.NarrowActive(ctx, narrowDeps, ha.NarrowInput{
		VMID:   h.vmid,
		Node:   h.node,
		OldPin: h.oldPin,
		NewPin: h.newPin,
		WritePin: func(ctx context.Context, pin *ha.Rule) error {
			return h.writePin(ctx, pin)
		},
		Mutation: pve.VMConfigMutation{
			VMID:       h.vmid,
			Node:       h.node,
			MotionWait: haMotionWait(h.deps),
			Label:      op + ": attach the disk inside the HA barrier",
			Apply: func(ctx context.Context, _ string) error {
				node, err := attach(ctx)
				landed = node
				return err
			},
		},
		CheckReach: h.checkReach,
		Unchanged:  h.attachUnchanged,
		SysConsole: h.profile.SysConsole,
		Budget:     haMotionWait(h.deps),
	})
	return err
}

// preflight runs the attach's refusals that only read, before the attach
// changes anything in HA. They are the refusal of a legacy disk named for the
// VM, the snapshot guard, and the refusal of a disk another VM holds. The
// attach runs each of them again in its own order, so a refusal here is the
// same answer the attach would give, only earlier. A disk stranded on an
// unused entry, or one whose transfer is still in flight, skips the holder
// refusal, because the attach settles it first and only then knows its holder.
func (h *attachHA) preflight(ctx context.Context) error {
	const op = attachHAOp
	rd := h.disk
	if err := refuseOwnedLegacyAttach(op, rd, h.node, h.vmid); err != nil {
		return err
	}
	cfg := h.deps.Config
	if cfg == nil {
		return nil
	}
	if err := attachDiskSnapshotGuard(ctx, h.deps, h.vmCID, h.node, h.vmid, cfg, h.logger); err != nil {
		return err
	}
	if _, stranded, ambiguous := strandedEntry(h.deps, rd); stranded || ambiguous || rd.intent != nil {
		return nil
	}
	var holder pve.DiskHolder
	if rd.stableID != "" {
		if rd.holder == nil {
			return nil
		}
		holder = *rd.holder
	} else {
		found, err := pve.ResolveDiskHolder(ctx, h.deps.PVE, h.logger, rd.volid, parkerReadConfigFor(h.deps))
		if err != nil {
			return wrapHolderScanError(err, fmt.Sprintf("%s: resolve current holder of disk %s", op, rd.diskCID))
		}
		holder = found
	}
	if holder.Found && !holder.IsParker && holder.VMID != h.vmid {
		return foreignHolderError(ctx, h.deps, op, rd.diskCID, h.vmid, holder)
	}
	return nil
}

// attachUnchanged answers the barrier's Unchanged after an attach that
// returned an error. The attach changed nothing that matters to where the VM
// may run when no volume in either view of the VM's config sits on the new
// disk's storage, because the decision ran only for a storage the VM's pin did
// not record. Every read failure is returned, and the barrier then keeps the
// narrower pin.
func (h *attachHA) attachUnchanged(ctx context.Context, node string) (bool, error) {
	storage, _, ok := strings.Cut(h.disk.volid, ":")
	if !ok || storage == "" {
		return false, fmt.Errorf("disk %s names no storage", h.disk.volid)
	}
	holding, err := pve.ReadQemuHolding(ctx, h.deps.PVE, node, h.vmid)
	if err != nil {
		return false, fmt.Errorf("read the config of VM %d: %w", h.vmid, pve.WrapError(err))
	}
	for _, view := range []map[string]any{holding.Config, holding.Replaced} {
		for _, v := range pve.ConfigAccessVolumes(view, node) {
			if s, _, _ := strings.Cut(v.Volume, ":"); s == storage {
				return false, nil
			}
		}
	}
	return true, nil
}

// writePin writes the VM's pin under the rules lock and verifies it. A nil pin
// removes it, which the barrier asks for only when the change needs no pin.
func (h *attachHA) writePin(ctx context.Context, pin *ha.Rule) error {
	if pin == nil {
		return removeNodeAffinityPin(ctx, h.deps, h.vmid, h.logger)
	}
	owner := h.deps.RequestDirectorUUID
	err := withHARulesLock(ctx, h.deps, h.vmid, h.logger, func() error {
		writer := newHARuleWriterFor(h.deps, h.profile, h.logger, h.decision)
		_, err := writer.Apply(ctx, "node-affinity", func(set *ha.RuleSet) ([]ha.RuleOp, error) {
			cur := set.Find(pin.ID)
			switch {
			case cur == nil:
				return []ha.RuleOp{{Kind: ha.OpCreate, Rule: *pin}}, nil
			case haOwnershipOf(cur.Comment, owner) == haOwnedByOther:
				return nil, fmt.Errorf("node-affinity: rule %q carries another Director's ownership marker; "+
					"leaving it alone", pin.ID)
			case cur.Type != ha.RuleTypeNodeAffinity:
				return nil, fmt.Errorf("node-affinity: rule %q is a %s rule, not a node-affinity pin; leaving it alone",
					pin.ID, cur.Type)
			case ha.SameRule(pin, cur):
				return nil, nil
			}
			return []ha.RuleOp{{Kind: ha.OpUpdate, Rule: *pin}}, nil
		})
		return err
	})
	if err != nil {
		return err
	}
	stager := &ha.Stager{
		ReadRules: func(ctx context.Context) (*ha.RuleSet, error) { return readHARuleSet(ctx, h.deps.PVE.Cluster()) },
		Managed:   h.deps.Config.HAModeValue() == config.HAModeManaged,
		Logger:    h.logger,
	}
	return stager.Verify(ctx, h.decision, pin, "")
}

// checkReach re-reads the VM's config after the attach and checks that every
// node of the new pin reaches every volume the VM now holds.
func (h *attachHA) checkReach(ctx context.Context, node string) error {
	cfg, err := h.deps.PVE.QEMU().Config(ctx, node, h.vmid)
	if err != nil {
		return fmt.Errorf("read the config of VM %d: %w", h.vmid, pve.WrapError(err))
	}
	index, err := ha.NewStorageIndex(h.profile.Storages)
	if err != nil {
		return err
	}
	want := h.newPin.NodeNames()
	reach, err := pve.VolumeAccessSet(ctx, index, want, pve.ConfigAccessVolumes(cfg, node))
	if err != nil {
		return err
	}
	var missing []string
	for _, n := range want {
		if !slices.Contains(reach, n) {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("nodes %s of the pin of VM %d do not reach all of its volumes",
			strings.Join(missing, ","), h.vmid)
	}
	return nil
}
