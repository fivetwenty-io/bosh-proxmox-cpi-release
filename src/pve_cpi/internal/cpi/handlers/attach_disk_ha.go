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
	// the new disk.
	attachHALeave

	// attachHANarrow runs the attach inside the narrowing barrier, with the pin
	// the decision gives for the VM's volumes and the new disk.
	attachHANarrow
)

// attachHA is the attach-time HA decision for one attach_disk call. It is made
// before the attach's first mutation, the resume of an interrupted transfer
// included, and it only reads.
type attachHA struct {
	deps     Deps
	logger   *log.Logger
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
		return nil, nil
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
	// resource with no pending barrier skips the decision.
	if oldPin != nil && !ha.NarrowPending(res.State, res.Comment) {
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
		deps: deps, logger: logger, vmid: vmid, sid: sid, node: loc.Node, profile: profile, decision: d,
		oldPin: oldPin, narrow: ha.NarrowDepsFor(deps.PVE, logger, haMotionWait(deps)),
	}
	switch d.Outcome {
	case ha.OutcomeFail:
		if failure := d.Failure(); failure != nil {
			return nil, failure
		}
		return nil, cpierrors.Cloud("%s: VM %d: the HA decision failed the call: %s", op, vmid, d.Reason)
	case ha.OutcomeRegister:
		newPin, err := attachNewPin(d, oldPin, profile, set, ha.VolumeStorages(in.Volumes), owner)
		if err != nil {
			return nil, err
		}
		h.action, h.newPin = attachHANarrow, newPin
	default:
		logger.Warn(op+": the VM leaves PVE HA before the attach, because the HA decision gives it no HA with "+
			"the new disk", log.Int(metadataKeyVMID, vmid), log.String("disk", rd.volid),
			log.String("reason", d.Reason))
		h.action = attachHALeave
	}
	return h, nil
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
// disk there. The VM's pin is a constraint, so the decision never widens the
// set the VM may run on, and a PCI passthrough device is one too. A VM whose
// resource rebalances automatically is a DLB VM. The existing volumes were
// judged when the VM registered, so only the new disk can be legacy.
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
	if oldPin != nil {
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

// attachNewPin returns the pin the VM needs with the new disk. An existing pin
// keeps its priorities and loses the nodes the decision no longer admits, and
// its record names every storage the VM's volumes use. A VM with no pin gets
// the decision's pin. A pin that would name no node is a retriable error, and
// so is one smaller than the configured nodes that is not strict.
func attachNewPin(
	d *ha.VMDecision, oldPin *ha.Rule, profile *ha.Profile, set *ha.RuleSet, storages []string, owner string,
) (*ha.Rule, error) {
	ruleName := naRuleNameFor(d.VMID)
	configured := profileNodeNames(profile)
	if oldPin == nil {
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
	if len(configured) == 0 || len(pin.Nodes) < len(configured) {
		pin.Strict = true
	}
	pin.Comment = ha.WithVolumeRecord(oldPin.Comment, storages)
	pin.Errors = nil
	return &pin, nil
}

// run runs attach, the rest of the attach, as the HA decision requires, and
// returns its result. attach returns the node the VM is on when it is done.
func (h *attachHA) run(ctx context.Context, attach func(context.Context) (string, error)) error {
	const op = attachHAOp
	if h == nil || h.action == attachHANone {
		_, err := attach(ctx)
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
		if err := removeNodeAffinityPin(ctx, h.deps, h.vmid, h.logger); err != nil {
			return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("%s: remove the pin of VM %d after it left HA", op, h.vmid))
		}
		label := op + ": remove the HA-managed tag"
		if err := removeHAManagedTag(ctx, h.deps, loc.Node, h.vmid, label, h.logger); err != nil {
			return err
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
		SysConsole: h.profile.SysConsole,
		Budget:     haMotionWait(h.deps),
	})
	return err
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
