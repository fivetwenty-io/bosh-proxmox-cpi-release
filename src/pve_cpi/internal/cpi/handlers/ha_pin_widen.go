package handlers

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// pinWidenReachConstraint names the node constraint that holds the widened
// pin to the nodes the widening read and found able to take the VM now.
const pinWidenReachConstraint = "nodes that reach the VM's storage and network now"

// errPinWidenSkip ends a widening check that found nothing to do. It is never
// logged as a failure.
var errPinWidenSkip = errors.New("nothing to widen")

// nicKeyPattern matches the NIC keys of a QEMU config, net0 through netN.
var nicKeyPattern = regexp.MustCompile(`^net\d+$`)

// widenNarrowedPin is the step of a touch call that gives back the nodes a
// strict pin left out only because they could not be read when the pin was
// written. create_vm keeps a node out of the pin when it cannot read that
// node's bridges, because it cannot prove the VM's network is there, and a
// node that is down at that moment stays out of the pin after it returns. This
// step reads every online member that the pin leaves out, and the pin grows
// to the members that now hold every plain bridge the VM's NICs use and see
// every storage its volumes use as active. The HA decision then runs again
// with the VM's volumes, CPU type, PCI devices, and availability zone, so the
// pin never grows past what create_vm would have written with every node
// answering.
//
// It changes nothing unless the VM carries the bosh-ha-managed tag, the pin
// bosh-na-<vmid> is a strict, enabled node-affinity rule with this
// Director's marker, the VM's HA resource carries the same marker and isn't
// held ignored or mid-narrowing, and the pin's volume record matches the
// storages the VM's config uses now. A replicate VM is left to the replica
// policy, and the managed disk lifecycle is left alone. The step only ever
// widens. A decision that does not admit every node of the old pin, a
// decision that would not register the VM as shared, and a pin that changed
// under the rules lock leave the pin as it is. A failure is logged and never
// fails the call, because the VM runs as it did before.
func widenNarrowedPin(ctx context.Context, deps Deps, op string, vmid int, loc pve.VMLocation, logger *log.Logger) {
	if deps.Config == nil || deps.PVE == nil || deps.Config.HAModeValue() == config.HAModeOff ||
		loc.Node == "" || deps.RequestDirectorUUID == "" || hasReplicaTag(loc.Tags) ||
		!slices.Contains(parseTagsField(loc.Tags), haManagedTag) {
		return
	}
	if _, wrapped := deps.PVE.(guardWrappedClient); wrapped {
		return
	}
	logger = logger.With(log.Int(metadataKeyVMID, vmid), log.String("node", loc.Node))
	oldPin, newPin, err := widenPinOnce(ctx, deps, vmid, loc.Node, logger)
	switch {
	case errors.Is(err, errPinWidenSkip):
		return
	case err != nil:
		logger.Warn(op+": the VM's strict pin could not be checked for nodes it may now include; a later call "+
			"checks again", log.Err(err))
		return
	}
	logger.Info(op+": widened the VM's strict pin to the nodes that now reach its storage and network",
		log.String("old_nodes", strings.Join(oldPin.NodeNames(), ",")),
		log.String("new_nodes", strings.Join(newPin.NodeNames(), ",")),
		log.Bool("strict", newPin.Strict))
}

// widenPinOnce runs the widening check of widenNarrowedPin and writes the
// wider pin. It returns the old and new pins when it wrote one, and
// errPinWidenSkip when there was nothing to widen.
func widenPinOnce(ctx context.Context, deps Deps, vmid int, node string, logger *log.Logger) (*ha.Rule, *ha.Rule, error) {
	owner := deps.RequestDirectorUUID
	svc := deps.PVE.Cluster()
	if svc == nil {
		return nil, nil, errPinWidenSkip
	}
	set, err := readHARuleSet(ctx, svc)
	if err != nil {
		return nil, nil, fmt.Errorf("read the HA rules: %w", err)
	}
	ruleName := naRuleNameFor(vmid)
	oldPin := set.Find(ruleName)
	if oldPin == nil || oldPin.Type != ha.RuleTypeNodeAffinity || !oldPin.Strict || oldPin.Disabled ||
		haOwnershipOf(oldPin.Comment, owner) != haOwnedByUs || replicaPinComment(oldPin.Comment) {
		return nil, nil, errPinWidenSkip
	}
	recorded, ok := ha.VolumeRecord(oldPin.Comment)
	if !ok {
		return nil, nil, errPinWidenSkip
	}
	profile, err := resolveHAProfile(ctx, deps)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve the HA profile: %w", err)
	}
	if !profile.WritesAllowed() {
		return nil, nil, errPinWidenSkip
	}
	pinNodes := oldPin.NodeNames()
	candidates := pinWidenCandidates(profile, pinNodes)
	if len(candidates) == 0 {
		return nil, nil, errPinWidenSkip
	}
	az, azNodes, ok := pinWidenAZ(deps.Config, pinNodes)
	if !ok {
		return nil, nil, errPinWidenSkip
	}

	sid := haResourceSid(vmid)
	res, err := svc.GetHaResources(ctx, sid)
	if err != nil {
		return nil, nil, fmt.Errorf("read HA resource %s: %w", sid, pve.WrapError(err))
	}
	if res == nil || res.Comment == nil || haOwnershipOf(*res.Comment, owner) != haOwnedByUs ||
		pve.HARequestedState(res.State) == "ignored" || ha.NarrowPending(res.State, res.Comment) {
		return nil, nil, errPinWidenSkip
	}
	cfg, err := deps.PVE.QEMU().Config(ctx, node, vmid)
	if err != nil {
		return nil, nil, fmt.Errorf("read the config of VM %d on node %s: %w", vmid, node, pve.WrapError(err))
	}
	in := pinWidenInput(deps, profile.ResourceKeys(), vmid, node, cfg, res)
	storages := ha.VolumeStorages(in.Volumes)
	if !slices.Equal(storages, recorded) {
		// An attach or a detach changed the VM's volumes since the pin was
		// written, and its own decision owns the pin until the record agrees.
		return nil, nil, errPinWidenSkip
	}

	reachable, err := pinWidenReachable(ctx, deps, cfg, storages, candidates, logger)
	if err != nil {
		return nil, nil, err
	}
	if len(reachable) == 0 {
		return nil, nil, errPinWidenSkip
	}
	in.AZ, in.AZNodes = az, azNodes
	in.Constraints = append(in.Constraints, ha.NodeConstraint{
		Name: pinWidenReachConstraint, Nodes: append(slices.Clone(pinNodes), reachable...),
	})
	d, err := profile.DecideVM(in)
	if err != nil {
		return nil, nil, err
	}
	if !d.Registers() || d.Policy != ha.PolicyShared || d.Transient || d.PlaceableShortfall ||
		!pinWidens(pinNodes, d.Admissible) {
		return nil, nil, errPinWidenSkip
	}
	newPin, err := ha.DecisionPin(d, ruleName, haIdentity{Owner: owner}.marker(), storages,
		profileNodeNames(profile), profileRebalancesOnStart(profile), inResourceAffinityRule(set, d.SID))
	if err != nil {
		return nil, nil, err
	}
	if !pinWidens(pinNodes, newPin.NodeNames()) {
		return nil, nil, errPinWidenSkip
	}
	if err := writeWidenedPin(ctx, deps, profile, d, vmid, oldPin, newPin, logger); err != nil {
		return nil, nil, err
	}
	return oldPin, newPin, nil
}

// pinWidenCandidates lists the online members of the profile that the pin
// does not name, sorted. A member the profile reports offline cannot be read,
// so it is never a candidate.
func pinWidenCandidates(profile *ha.Profile, pinNodes []string) []string {
	var out []string
	for _, n := range profile.OnlineNodes() {
		if n != "" && !slices.Contains(pinNodes, n) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// pinWidenAZ returns the availability zone the widened pin must stay inside.
// With pin_az off, or no az_map, the decision gets no zone, as create_vm gave
// it none. Otherwise a touch call doesn't know which zone the VM named, so
// the zone is the one zone of az_map, the DLB sentinel aside, that holds every
// node of the pin. When no zone or more than one holds them, ok is false and
// the pin isn't widened, because the widening could leave the VM's zone.
func pinWidenAZ(cfg *config.CPIConfig, pinNodes []string) (az string, nodes []string, ok bool) {
	if cfg.HAPinAZValue() == config.HARuleOff || cfg.Placement == nil || len(cfg.Placement.AZMap) == 0 {
		return "", nil, true
	}
	var matches []string
	for _, name := range slices.Sorted(maps.Keys(cfg.Placement.AZMap)) {
		if name == "" || name == cfg.DLBAZName() {
			continue
		}
		members, _ := cfg.AZCandidates(name)
		if len(members) > 0 && !slices.ContainsFunc(pinNodes, func(n string) bool { return !slices.Contains(members, n) }) {
			matches = append(matches, name)
		}
	}
	if len(matches) != 1 {
		return "", nil, false
	}
	members, _ := cfg.AZCandidates(matches[0])
	return matches[0], slices.Clone(members), true
}

// pinWidenInput builds the decision input of the widening from the VM's
// config as it is now, the way attach_disk builds one, with no constraint for
// the old pin, because the old pin is what the widening may grow. The VM's
// PCI devices hold it to its node, and the caller adds the zone and the nodes
// the widening found reachable.
func pinWidenInput(
	deps Deps, keys ha.ResourceKeys, vmid int, node string, cfg map[string]any, res *cluster.GetHaResourcesResponse,
) ha.VMInput {
	liveCPU, _ := pve.ConfigString(cfg, "cpu")
	in := ha.VMInput{
		Config:      deps.Config,
		VMID:        vmid,
		Node:        node,
		Volumes:     haVolumesOfConfig(cfg, node, nil),
		CPUType:     ha.EffectiveCPUType(deps.Config, nil, liveCPU),
		InDLBAZ:     keys.InDLB(res.Comment, res.AutoRebalance),
		LinkedClone: configHasLinkedClone(cfg),
		HAManaged:   true,
	}
	if configHasPCIPassthrough(cfg) {
		in.Constraints = append(in.Constraints, ha.NodeConstraint{Name: pciConstraintName, Nodes: []string{node}})
	}
	return in
}

// pinWidenReachable returns the candidates that read back able to take the VM
// now. Each one must report every storage in storages as enabled and active,
// and, unless bridge_check is off, must list every plain bridge the VM's NICs
// use. A candidate whose storage or bridge list can't be read is left out, as
// create_vm leaves it out. A bridge whose SDN membership can't be read ends
// the check with an error, because the widening can't tell whether the node
// needs it.
func pinWidenReachable(
	ctx context.Context, deps Deps, cfg map[string]any, storages, candidates []string, logger *log.Logger,
) ([]string, error) {
	var bridges []string
	if deps.Config.BridgeCheckMode() != config.BridgeCheckOff {
		var err error
		if bridges, err = configPlainBridges(ctx, deps, cfg); err != nil {
			return nil, err
		}
	}
	checker := newNodeBridgeChecker(ctx, deps.PVE)
	var out []string
	for _, n := range candidates {
		if missing, err := nodeLacksStorage(ctx, deps, n, storages); err != nil || missing != "" {
			logger.Debug("pin widening: node left out, because it does not show the VM's storage as active",
				log.String("candidate", n), log.String("storage", missing), log.Err(err))
			continue
		}
		reach := true
		for _, bridge := range bridges {
			if present, err := checker.checkFresh(n, bridge); err != nil || !present {
				logger.Debug("pin widening: node left out, because it does not list the VM's bridge",
					log.String("candidate", n), log.String("bridge", bridge), log.Err(err))
				reach = false
				break
			}
		}
		if reach {
			out = append(out, n)
		}
	}
	return out, nil
}

// configPlainBridges lists the distinct bridges the VM's NICs use that are
// not SDN vnets, sorted. A vnet exists on every node of its zone by
// definition, which is why create_vm makes no bridge constraint for one.
func configPlainBridges(ctx context.Context, deps Deps, cfg map[string]any) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for key, raw := range cfg {
		if !nicKeyPattern.MatchString(key) {
			continue
		}
		text, ok := pve.ConfigStringValue(raw)
		if !ok {
			continue
		}
		for part := range strings.SplitSeq(text, ",") {
			name, value, found := strings.Cut(strings.TrimSpace(part), "=")
			if !found || name != "bridge" || value == "" || seen[value] {
				continue
			}
			seen[value] = true
			isVnet, err := pve.IsSDNVnet(ctx, deps.PVE, value)
			if err != nil {
				return nil, fmt.Errorf("read whether bridge %s is an SDN vnet: %w", value, err)
			}
			if !isVnet {
				out = append(out, value)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// nodeLacksStorage returns the first storage in storages that node does not
// list as active with the images content type, or "" when it lists all of
// them. It applies the check create_vm's placement applies.
func nodeLacksStorage(ctx context.Context, deps Deps, node string, storages []string) (string, error) {
	resp, err := deps.PVE.Nodes().ListStorage(ctx, node, nil)
	if err != nil {
		return "", pve.WrapError(err)
	}
	if resp == nil {
		return "", fmt.Errorf("node %s answered its storage list with no body", node)
	}
	for _, s := range storages {
		if !nodeHasStorage(resp, s) {
			return s, nil
		}
	}
	return "", nil
}

// pinWidens reports whether next names every node of pinNodes and at least
// one more.
func pinWidens(pinNodes, next []string) bool {
	if len(next) <= len(pinNodes) {
		return false
	}
	for _, n := range pinNodes {
		if !slices.Contains(next, n) {
			return false
		}
	}
	return true
}

// writeWidenedPin replaces oldPin with newPin under the HA rules lock (see
// writeReplacedPin). A pin that changed since the read, and one that already
// reads as newPin, end the widening as a skip.
func writeWidenedPin(
	ctx context.Context, deps Deps, profile *ha.Profile, d *ha.VMDecision, vmid int, oldPin, newPin *ha.Rule,
	logger *log.Logger,
) error {
	wrote, err := writeReplacedPin(ctx, deps, profile, d, vmid, oldPin, newPin, logger)
	switch {
	case errors.Is(err, errPinChanged):
		return errPinWidenSkip
	case err != nil:
		return err
	case !wrote:
		return errPinWidenSkip
	}
	return nil
}

// errPinChanged ends a pin replacement that found the pin in PVE neither the
// pin it was decided from nor the one it would write. The caller leaves the
// pin to whichever call changed it.
var errPinChanged = errors.New("the pin changed since it was read")

// writeReplacedPin replaces oldPin, the pin bosh-na-<vmid> as the caller read
// it, with newPin under the HA rules lock, and then checks that the VM is
// still a member of its anti-affinity rule (see verifyAntiAffinityMember). It
// reports whether it wrote. The write happens only when the pin is still
// exactly oldPin, so a pin another call changed since the read is left to that
// call, and errPinChanged is returned. A pin that already reads as newPin,
// which another call wrote, needs no write, and it reports false with no
// error. Under a context from withHARulesLockTryOnce the rules lock is tried
// once.
func writeReplacedPin(
	ctx context.Context, deps Deps, profile *ha.Profile, d *ha.VMDecision, vmid int, oldPin, newPin *ha.Rule,
	logger *log.Logger,
) (bool, error) {
	ruleName := naRuleNameFor(vmid)
	wrote := false
	err := withHARulesLock(ctx, deps, vmid, logger, func(ctx context.Context) error {
		writer := newHARuleWriterFor(deps, profile, logger, d)
		res, err := writer.Apply(ctx, "node-affinity", func(set *ha.RuleSet) ([]ha.RuleOp, error) {
			cur := set.Find(ruleName)
			switch {
			case cur != nil && ha.SameRule(newPin, cur):
				// The writer plans again after each write, and the new pin
				// it reads back is the end of the plan.
				return nil, nil
			case cur == nil || !ha.SameRule(oldPin, cur):
				return nil, errPinChanged
			}
			return []ha.RuleOp{{Kind: ha.OpUpdate, Rule: *newPin}}, nil
		})
		if err != nil {
			return err
		}
		if len(res.Wrote) == 0 {
			return nil
		}
		wrote = true
		return verifyAntiAffinityMember(ctx, deps, ruleName, haResourceSid(vmid), logger)
	})
	return wrote, err
}
