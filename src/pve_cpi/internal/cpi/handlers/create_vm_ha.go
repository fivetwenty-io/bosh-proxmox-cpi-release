package handlers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// haManagedTag is the PVE tag on a VM that the CPI registered with PVE HA. It
// lets an operator filter for HA-managed VMs in the PVE UI, and it lets the
// doctor find an owned VM that carries it but has no HA resource.
const haManagedTag = "bosh-ha-managed"

// pciConstraintName names the node constraint of a VM with PCI passthrough
// devices, which may run only on the node that holds them.
const pciConstraintName = "PCI passthrough devices"

// createdVMHA carries one create_vm call's PVE HA registration through the
// staging order of ha.Stager. A nil value means the VM gets no HA, which is
// what mode off, a decision that skips, and an auto call that cannot mark a
// resource as its own all give, and every method is safe to call on it.
type createdVMHA struct {
	deps     Deps
	logger   *log.Logger
	vmid     int
	ident    haIdentity
	profile  *ha.Profile
	decision *ha.VMDecision
	storages []string
	stager   *ha.Stager

	// pin is the merged pin the call wrote and aaRule the anti-affinity rule
	// the VM was placed in, or "" when it is in none. Verify checks both.
	pin    *ha.Rule
	aaRule string

	// unconfined says why a write of the VM's confinement failed in auto. A
	// non-empty value makes the activation remove the staged resource, so the
	// VM runs without HA rather than under HA without its confinement.
	unconfined string

	// activated is true once PVE HA runs the VM, and landing is the node it
	// runs on.
	activated bool
	landing   string
}

// staged reports whether the call staged the VM's resource and has not
// activated or removed it, which is when the VM must not be started directly.
func (h *createdVMHA) staged() bool {
	return h != nil && h.decision != nil && h.decision.StagedIgnored
}

// haActivated reports whether PVE HA runs the VM.
func (h *createdVMHA) haActivated() bool { return h != nil && h.activated }

// decideCreatedVMHA makes the VM's HA decision after its configuration has
// defined every volume, and stages its HA resource as ignored when the
// decision registers it. Mode off reads and writes nothing. A request with no
// director_uuid, or a profile that cannot be resolved, fails a managed call and
// gives an auto call no HA, because the CPI could not mark the resource as its
// own or could not know what it may write. A decision that fails the call
// returns its error.
func decideCreatedVMHA(
	ctx context.Context, deps Deps, logger *log.Logger, parsed *createVMParsedArgs, shape *createVMShape, vmid int,
) (*createdVMHA, error) {
	mode := deps.Config.HAModeValue()
	if mode == config.HAModeOff {
		return nil, nil
	}
	managed := mode == config.HAModeManaged
	ident := haIdentityFor(deps, parsed.env)
	if ident.Owner == "" {
		if managed {
			return nil, cpierrors.Cloud("create_vm: VM %d: pve.ha.mode is managed and the request carries no "+
				"director_uuid, so the CPI cannot mark the VM's HA resource as its own", vmid)
		}
		logger.Warn("create_vm: the VM runs without HA, because the request carries no director_uuid to mark its "+
			"HA resource with", log.Int(metadataKeyVMID, vmid))
		return nil, nil
	}
	profile, err := resolveHAProfile(ctx, deps)
	if err != nil {
		if managed {
			return nil, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("create_vm: VM %d: resolve the HA profile", vmid))
		}
		logger.Warn("create_vm: the VM runs without HA, because the HA profile could not be resolved",
			log.Int(metadataKeyVMID, vmid), log.Err(err))
		return nil, nil
	}
	cfg, err := deps.PVE.QEMU().Config(ctx, shape.node, vmid)
	if err != nil {
		return nil, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("create_vm: read the config of VM %d for its HA decision", vmid))
	}
	in := createdVMHAInput(ctx, deps, parsed, shape, vmid, cfg, profile)
	decision, err := profile.DecideVM(in)
	if err != nil {
		return nil, err
	}
	for _, w := range decision.Warnings {
		logger.Warn("create_vm: "+w, log.Int(metadataKeyVMID, vmid))
	}
	switch decision.Outcome {
	case ha.OutcomeFail:
		if failure := decision.Failure(); failure != nil {
			return nil, failure
		}
		return nil, cpierrors.Cloud("create_vm: VM %d: the HA decision failed the call: %s", vmid, decision.Reason)
	case ha.OutcomeRegister:
	default:
		logger.Info("create_vm: the VM runs without HA", log.Int(metadataKeyVMID, vmid),
			log.String("reason", decision.Reason))
		warnRequestedHAWithoutRegistration(deps, parsed, decision, logger)
		return nil, nil
	}
	h := &createdVMHA{
		deps: deps, logger: logger, vmid: vmid, ident: ident, profile: profile, decision: decision,
		storages: ha.VolumeStorages(in.Volumes),
	}
	h.stager = h.newStager(managed)
	if err := h.stager.Stage(ctx, decision, ident.marker()); err != nil {
		return nil, err
	}
	return h, nil
}

// warnRequestedHAWithoutRegistration logs a warning when the operator set
// anti_affinity or pin_az to on and the decision still gives the VM no HA,
// because a rule needs a registered resource and none is written.
func warnRequestedHAWithoutRegistration(
	deps Deps, parsed *createVMParsedArgs, d *ha.VMDecision, logger *log.Logger,
) {
	var asked []string
	if deps.Config.HAAntiAffinityValue() == config.HARuleOn && haIdentityFor(deps, parsed.env).Group != "" {
		asked = append(asked, "pve.ha.anti_affinity")
	}
	if deps.Config.HAPinAZValue() == config.HARuleOn {
		asked = append(asked, "pve.ha.pin_az")
	}
	if len(asked) == 0 {
		return
	}
	logger.Warn("create_vm: no HA rule is written for this VM, because the HA decision does not register it",
		log.Int(metadataKeyVMID, d.VMID), log.String("settings", strings.Join(asked, ",")),
		log.String("reason", d.Reason))
}

// createdVMHAInput builds the decision input from the VM's config, its shape,
// and its cloud properties. It leaves State nil, because the VM was created by
// this call and has no HA state for the decision to read yet, so the decision
// carries no CRM uid.
func createdVMHAInput(
	ctx context.Context, deps Deps, parsed *createVMParsedArgs, shape *createVMShape, vmid int,
	cfg map[string]any, profile *ha.Profile,
) ha.VMInput {
	node := shape.node
	liveCPU, _ := pve.ConfigString(cfg, "cpu")
	in := ha.VMInput{
		Config:      deps.Config,
		VMID:        vmid,
		Node:        node,
		Volumes:     haVolumesOfConfig(cfg, node, parsed.legacyDiskVolids),
		CPUType:     ha.EffectiveCPUType(deps.Config, &shape.cpuType, liveCPU),
		InDLBAZ:     vmInDLBAZ(parsed.cloudProps, deps.Config),
		LinkedClone: configHasLinkedClone(cfg),
	}
	if az := pinAZForNode(parsed.cloudProps, deps.Config, node); az != "" {
		if nodes, ok := deps.Config.AZCandidates(az); ok {
			in.AZ, in.AZNodes = az, nodes
		}
	}
	if len(parsed.cloudProps.PCIPassthroughs) > 0 {
		in.Constraints = append(in.Constraints, ha.NodeConstraint{Name: pciConstraintName, Nodes: []string{node}})
	}
	in.Constraints = append(in.Constraints, createdVMBridgeConstraints(ctx, deps, parsed, profileNodeNames(profile))...)
	return in
}

// createdVMBridgeConstraints returns one constraint for each plain bridge the
// VM's NICs use, naming the cluster nodes that hold it. A node whose bridge
// list cannot be read does not count as holding it, so the VM is never pinned
// where its network may be missing. With the bridge check off, the operator
// has said the bridges exist everywhere, and no constraint is made.
func createdVMBridgeConstraints(
	ctx context.Context, deps Deps, parsed *createVMParsedArgs, members []string,
) []ha.NodeConstraint {
	if deps.Config == nil || deps.PVE == nil || len(members) == 0 ||
		deps.Config.BridgeCheckMode() == config.BridgeCheckOff {
		return nil
	}
	bridges := distinctBridges(plainCheckRefs(ctx, deps, parsed))
	if len(bridges) == 0 {
		return nil
	}
	checker := parsed.sharedBridgeChecker(ctx, deps.PVE)
	out := make([]ha.NodeConstraint, 0, len(bridges))
	for _, bridge := range bridges {
		var nodes []string
		for _, n := range members {
			if present, err := checker.check(n, bridge); err == nil && present {
				nodes = append(nodes, n)
			}
		}
		out = append(out, ha.NodeConstraint{Name: "bridge " + bridge, Nodes: nodes})
	}
	return out
}

// haVolumesOfConfig lists every volume a VM config references as decision
// input. A drive with media=cdrom is an ISO, a drive with replicate=0 is one
// PVE replication skips, and a volume in legacy is a persistent disk whose CID
// carries no stable ID.
func haVolumesOfConfig(cfg map[string]any, node string, legacy map[string]bool) []ha.VMVolume {
	access := pve.ConfigAccessVolumes(cfg, node)
	opts := driveOptionsByVolume(cfg)
	out := make([]ha.VMVolume, 0, len(access))
	for _, v := range access {
		vol := ha.VMVolume{Volume: v.Volume, HomeNode: v.HomeNode, Kind: ha.VolumeDisk, Legacy: legacy[v.Volume]}
		o := opts[v.Volume]
		if o["media"] == "cdrom" {
			vol.Kind = ha.VolumeISO
		}
		if o["replicate"] == "0" {
			vol.NoReplicate = true
		}
		out = append(out, vol)
	}
	return out
}

// driveOptionsByVolume maps each volume a config's drives name to the options
// of its drive entry.
func driveOptionsByVolume(cfg map[string]any) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, raw := range cfg {
		text, ok := pve.ConfigStringValue(raw)
		if !ok || !strings.Contains(text, ":") {
			continue
		}
		parts := strings.Split(text, ",")
		volid := strings.TrimSpace(strings.TrimPrefix(parts[0], "file="))
		o := map[string]string{}
		for _, p := range parts[1:] {
			if k, val, ok := strings.Cut(strings.TrimSpace(p), "="); ok {
				if k == "file" {
					volid = strings.TrimSpace(val)
					continue
				}
				o[k] = strings.TrimSpace(val)
			}
		}
		if volid != "" {
			out[volid] = o
		}
	}
	return out
}

// recordLegacyDisk records the volume of a persistent disk the call attached
// when its CID carries no stable ID.
func (p *createVMParsedArgs) recordLegacyDisk(rd resolvedDisk) {
	if rd.stableID != "" || rd.volid == "" {
		return
	}
	if p.legacyDiskVolids == nil {
		p.legacyDiskVolids = map[string]bool{}
	}
	p.legacyDiskVolids[rd.volid] = true
}

// configHasLinkedClone reports whether a disk of the config is a linked clone
// whose volume name shows its base, which is the form file storages give it.
// LVM-thin and ZFS name a linked clone like any other volume, so the check
// cannot see those, and a VM cloned on them reads as a full clone here.
func configHasLinkedClone(cfg map[string]any) bool {
	for _, v := range pve.ConfigAccessVolumes(cfg, "") {
		_, name, _ := strings.Cut(v.Volume, ":")
		if base, _, ok := strings.Cut(name, "/"); ok && strings.HasPrefix(base, "base-") {
			return true
		}
	}
	return false
}

// newStager wires the staging order to the cluster.
func (h *createdVMHA) newStager(managed bool) *ha.Stager {
	deps := h.deps
	svc := deps.PVE.Cluster()
	owner := h.ident.Owner
	sysConsole := h.profile.SysConsole
	// The watchdog check reads every admissible node, and one health report
	// answers them all, so the first successful read is kept for the call.
	var health *pve.NodeHealthReport
	return &ha.Stager{
		Resources: svc,
		Owned:     func(comment string) bool { return haOwnershipOf(comment, owner) == haOwnedByUs },
		ReadRules: func(ctx context.Context) (*ha.RuleSet, error) {
			resp, err := svc.ListHaRules(ctx, &cluster.ListHaRulesParams{})
			if err != nil {
				return nil, err
			}
			return ha.ParseRuleList(resp)
		},
		Watchdog: func(ctx context.Context, node string) (pve.WatchdogState, error) {
			if health == nil {
				report, err := pve.NodeHealth(ctx, svc, deps.PVE.Nodes(), haHealthOptions(deps))
				if err != nil {
					return pve.WatchdogUnknown, err
				}
				health = report
			}
			return health.WatchdogOf(node), nil
		},
		SetStarted: func(ctx context.Context, vmid int, node string) (*pve.SetHAStateResult, error) {
			return pve.SetHAState(ctx, pve.HAStateDepsFor(deps.PVE), pve.SetHAStateInput{
				VMID: vmid, Node: node, Target: pve.HATargetStarted, Caller: pve.HACallerKeep,
				SysConsole: sysConsole, Budget: haMotionWait(deps), HealthOptions: haHealthOptions(deps),
			})
		},
		Managed:       managed,
		FragileQuorum: h.profile.Quorum.OneFailureFromLoss(),
		Logger:        h.logger,
	}
}

// confine writes the staged VM's confinement, which is step 3 of the staging
// order: its anti-affinity membership first, so the pin's priorities see the
// membership, and then its merged pin with the volume record. It then checks
// that a PCI VM's pin holds its node alone and, for a DLB VM, the cluster's CRS
// mode. An error that must fail the call is returned: a retriable one, and in
// managed every one. Any other write failure in auto marks the VM unconfined,
// so the activation removes the staged resource and the VM runs without HA.
func (h *createdVMHA) confine(ctx context.Context, parsed *createVMParsedArgs, node string) error {
	if !h.staged() {
		return nil
	}
	aaRule, err := applyAntiAffinityMembership(ctx, h.deps, h.decision, parsed.env, h.logger)
	if err != nil {
		return h.confinementFailure("anti-affinity membership", err)
	}
	h.aaRule = aaRule
	pin, err := applyAZNodeAffinityPin(ctx, h.deps, h.decision, h.storages, parsed.cloudProps, node, h.logger)
	if err != nil {
		return h.confinementFailure("node-affinity pin", err)
	}
	h.pin = pin
	if err := applyPCINodeAffinityPin(h.decision, h.pin, node, parsed.cloudProps.PCIPassthroughs); err != nil {
		return err
	}
	if err := ensureDLBMembership(ctx, h.deps, h.decision, h.logger); err != nil {
		h.logger.Warn("create_vm: the DLB cluster check did not complete (non-fatal)",
			log.Int(metadataKeyVMID, h.vmid), log.Err(err))
	}
	return nil
}

// confinementFailure returns err when it must fail the call, and otherwise
// records that the VM is unconfined and logs it.
func (h *createdVMHA) confinementFailure(what string, err error) error {
	if propagateHARuleError(h.deps, err) {
		return err
	}
	h.unconfined = fmt.Sprintf("the %s could not be written: %s", what, err.Error())
	h.logger.Warn("create_vm: the VM's HA confinement could not be written; the staged HA resource is removed "+
		"and the VM runs without HA", log.Int(metadataKeyVMID, h.vmid), log.String("step", what), log.Err(err))
	return nil
}

// activate runs steps 4 to 6 of the staging order for a staged VM and returns
// the node the VM runs on. It verifies the confinement, applies the watchdog
// check, requests started, and checks the landing node. In auto, a
// confinement that is not in effect, a watchdog that is not armed, or a
// confinement write that failed removes the staged resource and starts the VM
// directly with start, so it runs without HA. A VM with no staged resource was
// started before, and node is returned as it is.
func (h *createdVMHA) activate(ctx context.Context, node string, start func(context.Context) error) (string, error) {
	if !h.staged() {
		return node, nil
	}
	reason := h.unconfined
	if reason == "" {
		switch err := h.stager.Verify(ctx, h.decision, h.pin, h.aaRule); {
		case err == nil:
		case errors.Is(err, ha.ErrConfinementUnverified) && !h.stager.Managed:
			reason = err.Error()
		default:
			return node, err
		}
	}
	if reason == "" {
		switch err := h.stager.CheckWatchdog(ctx, h.decision, node); {
		case err == nil:
		case errors.Is(err, ha.ErrWatchdogNotArmed):
			reason = err.Error()
		default:
			return node, err
		}
	}
	if reason != "" {
		return node, h.runWithoutHA(ctx, reason, start)
	}
	landing, err := h.stager.Activate(ctx, h.decision, node)
	if err != nil {
		// A landing outside the admissible set still names where PVE HA
		// started the VM, and the rollback must act there.
		if landing != "" {
			return landing, err
		}
		return node, err
	}
	h.activated, h.landing = true, landing
	return landing, nil
}

// runWithoutHA removes the staged resource, removes the VM's pin and the
// anti-affinity rules its group no longer needs, and starts the VM directly.
// The removal purges the resource from every rule, and the pin and the rules
// are then removed by name whether or not this call recorded writing them,
// because a write that failed part way can leave a rule behind. A rule that
// cannot be deleted then names no resource of this VM and confines nothing, so
// those failures are only logged.
func (h *createdVMHA) runWithoutHA(ctx context.Context, reason string, start func(context.Context) error) error {
	if err := h.stager.Unstage(ctx, h.decision, reason); err != nil {
		return err
	}
	if err := removeNodeAffinityPin(ctx, h.deps, h.vmid, h.logger); err != nil {
		h.logger.Warn("create_vm: the pin of the VM that runs without HA could not be removed; it names no "+
			"resource now, and delete_vm removes it", log.Int(metadataKeyVMID, h.vmid), log.Err(err))
	}
	if err := pruneGroupAntiAffinityRules(ctx, h.deps, h.ident, h.vmid, h.logger); err != nil {
		h.logger.Warn("create_vm: the anti-affinity rules the VM that runs without HA left could not be pruned; "+
			"they name no resource of this VM now", log.Int(metadataKeyVMID, h.vmid), log.Err(err))
	}
	h.pin = nil
	h.aaRule = ""
	return start(ctx)
}

// completeCreatedVM runs the steps of a create after the VM's networks are
// known, on the main path and on the fallback path alike. The VM is running
// unless h staged it. It applies the firewall, the VIP ipfilter, the IP
// forwarding, and the advertised routes, then writes the HA confinement, runs
// the config-drive ISO check, and activates the staged VM. A VM PVE HA started
// elsewhere has shape.node moved to its landing node, and an HA-managed VM
// gets the bosh-ha-managed tag. The health gate runs last, on the VM's node.
func completeCreatedVM(
	ctx context.Context, deps Deps, logger *log.Logger, parsed *createVMParsedArgs, shape *createVMShape, vmid int,
	h *createdVMHA, start func(context.Context) error,
) error {
	if err := applyCreatedVMNetworkPolicy(ctx, deps, logger, parsed, shape, vmid); err != nil {
		return err
	}
	if err := h.confine(ctx, parsed, shape.node); err != nil {
		return err
	}
	if err := checkISOStorageForHA(ctx, deps, vmid, parsed.cloudProps, shape.node, parsed.env, logger); err != nil {
		return err
	}
	// shape.node follows the VM to the node PVE HA started it on before the
	// error is returned, because the rollback reads shape.node and must tear
	// the VM down where it runs.
	landing, err := h.activate(ctx, shape.node, start)
	if landing != "" && landing != shape.node {
		logger.Info("create_vm: PVE HA started the VM on another node",
			log.Int(metadataKeyVMID, vmid), log.String("placed", shape.node), log.String("node", landing))
		shape.node = landing
	}
	if err != nil {
		return err
	}
	// The tag is a marker for operators. delete_vm finds an HA-managed VM by
	// its HA resource, so a failed tag write is logged and the create goes on
	// rather than tearing down a VM that runs under HA.
	if h.haActivated() {
		if err := addHAManagedTag(ctx, deps, shape.node, vmid, logger); err != nil {
			logger.Warn("create_vm: the bosh-ha-managed tag could not be written; the VM runs under PVE HA without it",
				log.Int(metadataKeyVMID, vmid), log.String("node", shape.node), log.Err(err))
		}
	}
	if deps.Config.HealthCheckEnabled() {
		if err := runHealthGate(ctx, deps, shape.node, vmid, logger); err != nil {
			return err
		}
	}
	return nil
}

// applyCreatedVMNetworkPolicy applies the firewall, the VIP ipfilter, the IP
// forwarding, and the advertised routes of a created VM.
//
// The firewall's security groups resolve per call, then from the vm_type or
// disk_type profile, then from the global default, and the firewall flag the
// same way. Non-empty groups attach their rules and enable the firewall, and a
// set flag with no groups enables it alone. A failure fails the call, so a
// half-firewalled VM is never left behind. The datacenter master switch is
// probed once per process whenever a firewall feature is in play.
//
// The VIP ipfilter is best-effort: a failure leaves ipfilter off and is
// logged. NICs with ip_forwarding get their firewall turned off, and the
// advertised routes inject their SDN subnets; both fail the call on error.
func applyCreatedVMNetworkPolicy(
	ctx context.Context, deps Deps, logger *log.Logger, parsed *createVMParsedArgs, shape *createVMShape, vmid int,
) error {
	effectiveGroups, err := resolveEffectiveSecurityGroups(parsed.cloudPropsMap, deps.Config,
		parsed.cloudProps.SecurityGroups)
	if err != nil {
		return err
	}
	firewallEnabled, err := resolveEffectiveFirewall(parsed.cloudPropsMap, deps.Config)
	if err != nil {
		return err
	}
	if firewallFeatureInPlay(effectiveGroups, firewallEnabled, parsed.networks) {
		probeFirewallMasterSwitch(ctx, deps, logger)
	}
	switch {
	case len(effectiveGroups) > 0:
		if err := applySecurityGroups(ctx, deps, shape.node, vmid, effectiveGroups, logger); err != nil {
			return err
		}
	case firewallEnabled:
		if err := enableVMFirewall(ctx, deps, shape.node, vmid, logger); err != nil {
			return err
		}
	}
	if err := applyVIPAllowedAddressPairs(ctx, deps, shape.node, vmid, parsed.networks, logger); err != nil {
		logger.Warn("create_vm: VIP ipfilter not fully applied (non-fatal)",
			log.Int(metadataKeyVMID, vmid), log.Err(err))
	}
	if err := applyIPForwarding(ctx, deps, shape.node, vmid, parsed.networks, logger); err != nil {
		return err
	}
	return applyCreatedVMRoutes(ctx, deps, parsed, shape, vmid, logger)
}

// addHAManagedTag adds the bosh-ha-managed tag to the VM's tags.
func addHAManagedTag(ctx context.Context, deps Deps, node string, vmid int, logger *log.Logger) error {
	return mutateVMTags(ctx, deps, node, vmid, "create_vm: tag the VM as HA-managed", logger,
		func(tags []string) []string {
			if slices.Contains(tags, haManagedTag) {
				return tags
			}
			return append(slices.Clone(tags), haManagedTag)
		})
}

// removeHAManagedTag removes the bosh-ha-managed tag from the VM's tags.
func removeHAManagedTag(ctx context.Context, deps Deps, node string, vmid int, label string, logger *log.Logger) error {
	return mutateVMTags(ctx, deps, node, vmid, label, logger, func(tags []string) []string {
		return slices.DeleteFunc(slices.Clone(tags), func(t string) bool { return t == haManagedTag })
	})
}
