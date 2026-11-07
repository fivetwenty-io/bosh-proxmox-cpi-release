package handlers

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/vmcid"
)

// This file wires the re-registration of an errored HA resource (see
// ha.ReregisterErrored) to the cluster: the VM config that holds the
// re-registration tag and record, the eligibility decision over the VM's
// volumes, and the rule writers that restore the VM's confinement.

// readdOptions are what a caller of reregisterErroredVM adds to the order.
type readdOptions struct {
	// decided is the state read the caller's decision was made on, nil when
	// the caller made none.
	decided *pve.HAServiceState
	// stopAfterRemoval ends the order once the resource is removed, for a
	// caller whose decision gives the VM no HA anyway.
	stopAfterRemoval bool
	// between is the change the caller makes while the VM has no HA resource,
	// nil for none.
	between func(ctx context.Context, node string) (pve.VMLocation, error)
	// decide replaces the decision over the VM's volumes, for a caller that
	// made its own over the volumes the change gives the VM.
	decide func(ctx context.Context, node string, st *pve.HAServiceState, rec ha.ReaddRecord) (*ha.VMDecision, error)
	// writePin replaces the merged pin write of the confinement, for a caller
	// that knows the pin its change needs.
	writePin func(ctx context.Context, d *ha.VMDecision) (*ha.Rule, error)
	// profile is the resolved HA profile, nil to resolve it here.
	profile *ha.Profile
}

// readdError wraps msg as a blocked re-registration, a retriable error that
// a retry alone does not clear.
func readdError(op, msg string) error {
	return cpierrors.WrapAs(fmt.Errorf("%w: %s", ha.ErrReaddBlocked, msg), cpierrors.TypeRetriableCloud,
		op+": ha readd")
}

// reregisterErroredVM runs the re-registration order for the VM vmid on node,
// or finishes one an earlier call left part way. It refuses a request with no
// director_uuid, because the CPI could not prove the resource its own.
func reregisterErroredVM(
	ctx context.Context, deps Deps, op string, vmid int, node string, logger *log.Logger, opts readdOptions,
) (ha.ReregisterResult, error) {
	if deps.PVE == nil || deps.PVE.Cluster() == nil || deps.PVE.Nodes() == nil || deps.PVE.QEMU() == nil {
		return ha.ReregisterResult{}, cpierrors.Cloud("%s: the PVE client cannot re-register the HA resource of VM %d",
			op, vmid)
	}
	owner := deps.RequestDirectorUUID
	if owner == "" {
		return ha.ReregisterResult{}, readdError(op, fmt.Sprintf("the request carries no director_uuid, so the CPI "+
			"cannot prove the HA resource of VM %d its own; clear its error without stopping the guest by running "+
			"'ha-manager remove %s' and then 'ha-manager add %s --state started'", vmid, haResourceSid(vmid),
			haResourceSid(vmid)))
	}
	profile := opts.profile
	if profile == nil {
		resolved, err := resolveHAProfile(ctx, deps)
		if err != nil {
			return ha.ReregisterResult{}, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("%s: resolve the HA profile to re-register VM %d", op, vmid))
		}
		profile = resolved
	}
	managed := deps.Config.HAModeValue() == config.HAModeManaged
	w := &readdWiring{deps: deps, op: op, vmid: vmid, owner: owner, profile: profile, logger: logger, opts: opts}
	decide := w.decide
	if opts.decide != nil {
		decide = opts.decide
	}
	in := ha.ReregisterInput{
		VMID:             vmid,
		Node:             node,
		Profile:          profile,
		Decided:          opts.decided,
		StopAfterRemoval: opts.stopAfterRemoval,
		PinID:            naRuleNameFor(vmid),
		Budget:           haMotionWait(deps),
		Stager:           newHAStager(deps, owner, profile, managed, logger),
		Power:            w.power,
		ReadIntent:       w.readIntent,
		WriteIntent:      w.writeIntent,
		ClearIntent:      w.clearIntent,
		Decide:           decide,
		LeaveRules:       w.leaveRules,
		Between:          opts.between,
		Confine:          w.confine,
		WithoutHA:        w.withoutHA,
	}
	return ha.ReregisterErrored(ctx, ha.NarrowDepsFor(deps.PVE, logger, haMotionWait(deps)), in)
}

// readdWiring holds one re-registration's callbacks.
type readdWiring struct {
	deps    Deps
	op      string
	vmid    int
	owner   string
	profile *ha.Profile
	logger  *log.Logger
	opts    readdOptions
	// volumes are the VM's volumes as the last decision read them, which the
	// pin's volume record names.
	volumes []ha.VMVolume
}

func (w *readdWiring) power(ctx context.Context, node string) (string, error) {
	st, err := w.deps.PVE.QEMU().Status(ctx, node, w.vmid)
	if err != nil {
		return "", pve.WrapError(err)
	}
	state, _ := pve.ConfigString(st, "status")
	return state, nil
}

func (w *readdWiring) readIntent(ctx context.Context, node string) (ha.ReaddIntent, error) {
	cfg, err := w.deps.PVE.QEMU().Config(ctx, node, w.vmid)
	if err != nil {
		return ha.ReaddIntent{}, pve.WrapError(err)
	}
	raw, _ := pve.ConfigString(cfg, jsonKeyTags)
	tag, _, _ := ha.ReaddTagOf(parseTagsField(raw))
	rec, recErr := ha.ReaddRecordOf(pve.DescriptionFromConfig(cfg))
	return ha.ReaddIntent{Tag: tag, Record: rec, RecordErr: recErr}, nil
}

// writeIntent writes the tag and the record in one digest-guarded write.
func (w *readdWiring) writeIntent(ctx context.Context, node, tag string, rec ha.ReaddRecord) error {
	return mutateVMTagsAndDescription(ctx, w.deps, node, w.vmid, w.op+": write the HA re-registration tag", w.logger,
		func(tags []string, desc string) ([]string, string, error) {
			next := withReaddTag(tags, tag)
			cur, err := ha.ReaddRecordOf(desc)
			if err == nil && cur != nil && reflect.DeepEqual(*cur, rec) {
				return next, desc, nil
			}
			nextDesc, err := ha.WithReaddRecord(desc, &rec)
			return next, nextDesc, err
		})
}

// clearIntent removes the tag and the record in one digest-guarded write. A
// VM that runs under HA again gets the bosh-ha-managed tag, and one that runs
// without HA loses it. A resource another order holds leaves the tag as it is.
func (w *readdWiring) clearIntent(ctx context.Context, node string, outcome ha.ReaddOutcome) error {
	return mutateVMTagsAndDescription(ctx, w.deps, node, w.vmid, w.op+": remove the HA re-registration tag",
		w.logger, func(tags []string, desc string) ([]string, string, error) {
			next := tags
			if _, _, ok := ha.ReaddTagOf(tags); ok {
				next = ha.WithoutReaddTags(tags)
			}
			switch outcome {
			case ha.ReaddRegistered:
				if !slices.Contains(next, haManagedTag) {
					next = append(slices.Clone(next), haManagedTag)
				}
			case ha.ReaddWithoutHA, ha.ReaddRemoved:
				if slices.Contains(next, haManagedTag) {
					next = slices.DeleteFunc(slices.Clone(next), func(t string) bool { return t == haManagedTag })
				}
			}
			nextDesc, err := ha.WithReaddRecord(desc, nil)
			return next, nextDesc, err
		})
}

// withReaddTag returns tags with tag as their only re-registration tag, and
// tags unchanged when that already holds.
func withReaddTag(tags []string, tag string) []string {
	var found []string
	for _, t := range tags {
		if _, ok := ha.ReaddTagTime(t); ok {
			found = append(found, strings.TrimSpace(t))
		}
	}
	if len(found) == 1 && found[0] == tag {
		return tags
	}
	return append(ha.WithoutReaddTags(tags), tag)
}

// decide makes the VM's eligibility decision over the volumes its config holds
// on node now. The record keeps what the removal took away: a strict pin is a
// constraint, so the decision never widens where the VM may run, and an
// auto-rebalance flag says the VM is a DLB VM. A VM that is not a DLB VM and
// whose node lies in exactly one AZ of the AZ map gets that AZ, which pin_az
// then confines it to as create_vm does.
func (w *readdWiring) decide(
	ctx context.Context, node string, st *pve.HAServiceState, rec ha.ReaddRecord,
) (*ha.VMDecision, error) {
	cfg, err := w.deps.PVE.QEMU().Config(ctx, node, w.vmid)
	if err != nil {
		return nil, cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: read the config of VM %d for its HA decision", w.op, w.vmid))
	}
	in := readdDecisionInput(w.deps.Config, w.vmid, node, cfg, st, rec)
	d, err := w.profile.DecideVM(in)
	if err != nil {
		return nil, err
	}
	for _, msg := range d.Warnings {
		w.logger.Warn(w.op+": "+msg, log.Int(metadataKeyVMID, w.vmid))
	}
	w.volumes = in.Volumes
	return d, nil
}

// readdDecisionInput builds the decision input of a re-registration.
func readdDecisionInput(
	cfg *config.CPIConfig, vmid int, node string, vmCfg map[string]any, st *pve.HAServiceState, rec ha.ReaddRecord,
) ha.VMInput {
	liveCPU, _ := pve.ConfigString(vmCfg, "cpu")
	in := ha.VMInput{
		Config:      cfg,
		VMID:        vmid,
		Node:        node,
		Volumes:     haVolumesOfConfig(vmCfg, node, nil),
		CPUType:     ha.EffectiveCPUType(cfg, nil, liveCPU),
		InDLBAZ:     rec.AutoRebalance,
		LinkedClone: configHasLinkedClone(vmCfg),
		State:       st,
	}
	if rec.PinStrict && len(rec.PinNodes) > 0 {
		in.Constraints = append(in.Constraints, ha.NodeConstraint{Name: "the VM's pin " + naRuleNameFor(vmid),
			Nodes: slices.Clone(rec.PinNodes)})
	}
	if configHasPCIPassthrough(vmCfg) {
		in.Constraints = append(in.Constraints, ha.NodeConstraint{Name: pciConstraintName, Nodes: []string{node}})
	}
	if !rec.AutoRebalance {
		if az := soleAZOfNode(cfg, node); az != "" {
			nodes, _ := cfg.AZCandidates(az)
			in.AZ, in.AZNodes = az, nodes
		}
	}
	return in
}

// soleAZOfNode returns the one AZ of the AZ map, the DLB AZ aside, that lists
// node, and "" when none or several do, because the call then cannot tell
// which AZ the VM was placed in.
func soleAZOfNode(cfg *config.CPIConfig, node string) string {
	if cfg == nil || cfg.Placement == nil {
		return ""
	}
	names := make([]string, 0, len(cfg.Placement.AZMap))
	for az := range cfg.Placement.AZMap {
		names = append(names, az)
	}
	sort.Strings(names)
	found := ""
	for _, az := range names {
		if az == cfg.DLBAZName() || !slices.Contains(cfg.Placement.AZMap[az], node) {
			continue
		}
		if found != "" {
			return ""
		}
		found = az
	}
	return found
}

// leaveRules takes the VM out of every rule this Director owns and leaves its
// resource alone, for a removal PVE refuses while a rule names the resource.
// The VM's pin goes, and an anti-affinity rule left with fewer than two
// members goes too. A rule another Director owns, or one with no marker, is
// left alone.
func (w *readdWiring) leaveRules(ctx context.Context) error {
	return leaveOwnedHARules(ctx, w.deps, w.vmid, w.owner, w.logger)
}

func leaveOwnedHARules(ctx context.Context, deps Deps, vmid int, owner string, logger *log.Logger) error {
	sid := haResourceSid(vmid)
	pinID := naRuleNameFor(vmid)
	return withHARulesLock(ctx, deps, vmid, logger, func() error {
		writer := newHARuleWriter(deps, nil, logger)
		_, err := writer.Apply(ctx, "ha readd rule leave", func(set *ha.RuleSet) ([]ha.RuleOp, error) {
			var ops []ha.RuleOp
			for i := range set.Rules {
				r := &set.Rules[i]
				if !r.HasResource(sid) || haOwnershipOf(r.Comment, owner) != haOwnedByUs {
					continue
				}
				rest := slices.DeleteFunc(slices.Clone(r.Resources), func(s string) bool { return s == sid })
				switch {
				case r.ID == pinID, len(rest) == 0,
					r.Type == ha.RuleTypeResourceAffinity && len(rest) < 2:
					ops = append(ops, ha.RuleOp{Kind: ha.OpDelete, Rule: ha.Rule{ID: r.ID}})
				default:
					next := r.Clone()
					next.Resources, next.Errors = rest, nil
					ops = append(ops, ha.RuleOp{Kind: ha.OpUpdate, Rule: next})
				}
			}
			return ops, nil
		})
		return err
	})
}

// confine writes the staged VM's anti-affinity membership, when the record's
// marker names an instance group, and then its pin with the volume record. A
// CPI-owned rule PVE disabled while the resource was gone is enabled again. An
// error that must fail the call is returned, and any other failure in auto
// leaves the VM unconfined, so it runs without HA.
func (w *readdWiring) confine(
	ctx context.Context, d *ha.VMDecision, rec ha.ReaddRecord, node string,
) (ha.Confinement, error) {
	ident := w.identity(rec)
	var conf ha.Confinement
	if w.deps.Config.HAAntiAffinityValue() != config.HARuleOff && w.deps.Config.AntiAffinityEnabled() &&
		ident.valid() {
		placed, err := ensureAntiAffinityMembershipFor(ctx, w.deps, ident, w.vmid, d, w.logger)
		if err == nil {
			err = enableOwnedGroupRules(ctx, w.deps, ident, w.vmid, w.logger)
		}
		if err != nil {
			return w.unconfined("anti-affinity membership", err)
		}
		conf.AntiAffinity = placed
	}
	var pin *ha.Rule
	var err error
	if w.opts.writePin != nil {
		pin, err = w.opts.writePin(ctx, d)
	} else {
		pin, err = writeMergedPinRule(ctx, w.deps, w.profile, w.vmid,
			pinRequest{Decision: d, Storages: ha.VolumeStorages(w.volumes)}, w.logger)
	}
	if err == nil && pin == nil {
		err = fmt.Errorf("node-affinity: no pin was planned for VM %d", w.vmid)
	}
	if err != nil {
		return w.unconfined("node-affinity pin", err)
	}
	conf.Pin = pin
	if err := ensureDLBMembership(ctx, w.deps, d, w.logger); err != nil {
		w.logger.Warn(w.op+": the DLB cluster check did not complete (non-fatal)", log.Int(metadataKeyVMID, w.vmid),
			log.String("node", node), log.Err(err))
	}
	return conf, nil
}

func (w *readdWiring) unconfined(what string, err error) (ha.Confinement, error) {
	if propagateHARuleError(w.deps, err) {
		return ha.Confinement{}, err
	}
	w.logger.Warn(w.op+": the re-registered VM's HA confinement could not be written; it runs without HA",
		log.Int(metadataKeyVMID, w.vmid), log.String("step", what), log.Err(err))
	return ha.Confinement{Unconfined: fmt.Sprintf("the %s could not be written: %s", what, err.Error())}, nil
}

// identity is the instance group the record's marker names, owned by this
// Director.
func (w *readdWiring) identity(rec ha.ReaddRecord) haIdentity {
	m, ok := parseHaMarker(rec.Comment)
	if !ok {
		return haIdentity{Owner: w.owner}
	}
	return haIdentity{Owner: w.owner, Deployment: m.Deployment, Group: m.Group}
}

// withoutHA takes the VM out of its rules and removes the anti-affinity rules
// its group no longer needs, for a VM that goes on without HA.
func (w *readdWiring) withoutHA(ctx context.Context, _ *ha.VMDecision, rec ha.ReaddRecord, _ string) error {
	err := leaveOwnedHARules(ctx, w.deps, w.vmid, w.owner, w.logger)
	if pruneErr := pruneGroupAntiAffinityRules(ctx, w.deps, w.identity(rec), w.vmid, w.logger); pruneErr != nil {
		err = errors.Join(err, pruneErr)
	}
	return err
}

// enableOwnedGroupRules enables every anti-affinity rule of ident's group that
// this Director owns, holds the VM, and still spreads two members or more,
// when PVE disabled it. PVE disables a rule that names a resource it cannot
// find, which a removal and re-registration can leave behind.
func enableOwnedGroupRules(ctx context.Context, deps Deps, ident haIdentity, vmid int, logger *log.Logger) error {
	sid := haResourceSid(vmid)
	base := ident.ruleName()
	return withHARulesLock(ctx, deps, vmid, logger, func() error {
		writer := newHARuleWriter(deps, nil, logger)
		_, err := writer.Apply(ctx, "anti-affinity enable", func(set *ha.RuleSet) ([]ha.RuleOp, error) {
			var ops []ha.RuleOp
			for i := range set.Rules {
				r := &set.Rules[i]
				if _, ok := aaChunkIndex(base, r.ID); !ok || !r.Disabled || !r.HasResource(sid) ||
					len(r.Resources) < 2 || haOwnershipOf(r.Comment, ident.Owner) != haOwnedByUs {
					continue
				}
				next := r.Clone()
				next.Disabled, next.Errors = false, nil
				ops = append(ops, ha.RuleOp{Kind: ha.OpUpdate, Rule: next})
			}
			return ops, nil
		})
		return err
	})
}

// finishPendingHAReadd finishes the HA re-registration of a VM that carries
// the re-registration tag, before a handler changes the VM. loc is the VM's
// location with the tags the caller read. Mode off, a VM with no tag, and a
// client the managed lifecycle guard wraps, whose HA writes the guard refuses,
// do nothing here. A re-registration that cannot go on for a reason a retry
// does not clear is logged and the handler goes on, because the VM runs as it
// did before the call. Any other failure fails the handler, so the Director
// retries it.
func finishPendingHAReadd(ctx context.Context, deps Deps, op string, vmid int, loc pve.VMLocation, logger *log.Logger) error {
	if deps.Config == nil || deps.PVE == nil {
		return nil
	}
	tag, _, tagged := ha.ReaddTagOf(parseTagsField(loc.Tags))
	if !tagged {
		return nil
	}
	if deps.Config.HAModeValue() == config.HAModeOff {
		logger.Warn(op+": the VM carries an unfinished HA re-registration, and pve.ha.mode is off, so the CPI "+
			"leaves it; register the VM by hand with 'ha-manager add "+haResourceSid(vmid)+" --state started' or "+
			"remove the tag", log.Int(metadataKeyVMID, vmid), log.String("tag", tag))
		return nil
	}
	if _, wrapped := deps.PVE.(guardWrappedClient); wrapped {
		logger.Info(op+": the VM carries an unfinished HA re-registration; a later call outside the managed "+
			"lifecycle finishes it", log.Int(metadataKeyVMID, vmid), log.String("tag", tag))
		return nil
	}
	res, err := reregisterErroredVM(ctx, deps, op, vmid, loc.Node, logger, readdOptions{})
	switch {
	case err == nil:
		logger.Info(op+": finished the VM's HA re-registration", log.Int(metadataKeyVMID, vmid),
			log.String("outcome", string(res.Outcome)), log.String("node", res.Node))
		return nil
	case errors.Is(err, ha.ErrReaddBlocked), errors.Is(err, ha.ErrReaddVMStopped):
		logger.Warn(op+": the VM's HA re-registration cannot go on; the call goes on and the VM keeps its tag",
			log.Int(metadataKeyVMID, vmid), log.Err(err))
		return nil
	}
	return err
}

// finishPendingHAReaddForCID locates the VM vmCID names and finishes its HA
// re-registration when it carries the tag, after the ownership guard. It is
// the hook of the disk handlers, which locate the VM themselves later, so a
// CID that does not parse and a VM that is not found are left to the
// handler's own answers. Mode off reads nothing.
func finishPendingHAReaddForCID(ctx context.Context, deps Deps, op, vmCID string) error {
	if deps.Config == nil || deps.PVE == nil || deps.Config.HAModeValue() == config.HAModeOff {
		return nil
	}
	cid, err := vmcid.Parse(vmCID)
	if err != nil {
		return nil //nolint:nilerr // the handler's own parse answers for a CID that does not parse
	}
	loc, err := findVMAuthoritative(ctx, deps, cid.VMID)
	if err != nil {
		return cpierrors.WrapAs(pve.WrapError(err), cpierrors.TypeRetriableCloud,
			fmt.Sprintf("%s: locate VM %s to check its HA re-registration", op, vmCID))
	}
	if !loc.Found || loc.Node == "" {
		return nil
	}
	if _, _, tagged := ha.ReaddTagOf(parseTagsField(loc.Tags)); !tagged {
		return nil
	}
	logger := deps.Log(ctx).With(log.String("vm_cid", vmCID))
	if err := refuseUnownedVM(ctx, deps, op, vmCID, cid, loc, nil, logger); err != nil {
		return err
	}
	return finishPendingHAReadd(ctx, deps, op, cid.VMID, loc, logger)
}
