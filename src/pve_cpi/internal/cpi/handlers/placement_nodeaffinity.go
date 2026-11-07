package handlers

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// naRuleNamePrefix namespaces every CPI-managed HA node-affinity pin so a
// delete can find and remove it by vmid alone (delete_vm has no AZ context).
const naRuleNamePrefix = "bosh-na-"

// naRuleType is the PVE 9.x HA rule type that binds resources to a node set.
const naRuleType = "node-affinity"

// naRuleNameFor renders the deterministic per-VM node-affinity rule name, e.g.
// 100 -> "bosh-na-100". Distinct prefix from anti-affinity ("bosh-aa-").
func naRuleNameFor(vmid int) string {
	return naRuleNamePrefix + strconv.Itoa(vmid)
}

// applyAZNodeAffinityPin is the create_vm step that writes the merged
// node-affinity pin of a VM whose HA decision registers it, and it returns the
// pin it wrote or found in place. The decision already carries every node
// constraint: the AZ of the placed node (strict or preferred), a PCI device,
// the VM's bridges, its storage reach, and its CPU domain. The pin's comment
// records the storages of the VM's volumes, which a dead-node delete reads to
// bound where it may move the VM. A VM that registers with no hard constraint
// still gets a pin, the record pin, so every HA-managed VM carries its record.
//
// It warns when pve.ha.pin_az is on and no AZ resolves for the placed node,
// because BOSH never passes the cloud-config AZ name to create_vm, and when a
// strict pin targets a small AZ (see warnSingleNodeAZPin). Every failure is
// returned for the caller, which fails the call when propagateHARuleError says
// so and otherwise runs the VM without HA, so a VM never runs under HA without
// its pin.
func applyAZNodeAffinityPin(
	ctx context.Context, deps Deps, d *ha.VMDecision, storages []string, cp createVMCloudProps, node string,
	logger *log.Logger,
) (*ha.Rule, error) {
	if !d.Registers() {
		return nil, nil
	}
	if deps.Config.HAPinAZValue() != config.HARuleOff {
		warnAZPinScope(deps, d.VMID, cp, node, logger)
	}
	ruleName := naRuleNameFor(d.VMID)
	if deps.RequestDirectorUUID == "" {
		// A pin the CPI cannot mark as its own is one it could never edit or
		// clean up safely, so a request without a director_uuid writes none.
		return nil, fmt.Errorf("node-affinity: the request carries no director_uuid to mark rule %q with", ruleName)
	}
	profile, err := haRuleWriteProfile(ctx, deps)
	if err != nil {
		return nil, fmt.Errorf("node-affinity: %w", err)
	}
	return writeMergedPinRule(ctx, deps, profile, d.VMID, pinRequest{Decision: d, Storages: storages}, logger)
}

// warnAZPinScope logs the AZ warnings of a pinned VM. With no AZ resolved for
// the placed node while pve.ha.pin_az is on, the AZ part of the pin the
// operator asked for is missing, and the message names the cloud-config
// prerequisite because that is the fix in the common case.
// With a strict pin, a small AZ gets warnSingleNodeAZPin.
func warnAZPinScope(deps Deps, vmid int, cp createVMCloudProps, node string, logger *log.Logger) {
	if deps.Config.Placement == nil || len(deps.Config.Placement.AZMap) == 0 || vmInDLBAZ(cp, deps.Config) {
		return
	}
	az := pinAZForNode(cp, deps.Config, node)
	if az == "" && deps.Config.HAPinAZValue() == config.HARuleOn {
		// The chosen node is not in any requested AZ's node set. Either the VM
		// carried no availability_zone at all, or it landed outside the
		// requested AZ (operator target_node override, local-disk pin, or
		// config.node fallback). There is no AZ to make durable.
		//
		// BOSH never passes the cloud-config AZ name to create_vm (the AZ only
		// selects the CPI and the subnet), so the CPI learns it solely from
		// cloud_properties.availability_zone. An operator who sets
		// pve.ha.pin_az and az_map but omits availability_zone from the
		// AZ's cloud_properties gets a wholly inert feature.
		logger.Warn("create_vm: HA node-affinity pin is enabled but this VM was not pinned to an AZ — no "+
			"availability_zone resolved for the placed node; BOSH does not pass the cloud-config AZ name to the CPI, "+
			"so each cloud-config AZ needs cloud_properties.availability_zone naming a key of placement.az_map, and "+
			"BOSH merges an AZ's cloud_properties into every VM in that AZ (a VM pinned to a node outside its "+
			"requested AZ by target_node, a local-disk pin, or the config.node fallback reaches here too)",
			log.Int(metadataKeyVMID, vmid), log.String("node", node))
		return
	}
	if az == "" {
		return
	}
	if azNodes, ok := deps.Config.AZCandidates(az); ok {
		warnSingleNodeAZPin(az, azNodes, deps.Config.HAPinAZStrictValue(), logger)
	}
}

// pinAZForNode returns the AZ the VM was placed in by walking the same AZ order
// the scorer used (singular availability_zone, else plural availability_zones)
// and returning the first AZ whose node set contains the chosen node. The DLB
// sentinel AZ is never returned. Returns "" when the node is not in any
// requested AZ (operator pin, local-disk pin, or config.node fallback).
func pinAZForNode(cp createVMCloudProps, cfg *config.CPIConfig, node string) string {
	for _, az := range buildAZOrder(cp, cfg, nil, nil) {
		if az == cfg.DLBAZName() {
			continue
		}
		nodes, ok := cfg.AZCandidates(az)
		if !ok {
			continue
		}
		if slices.Contains(nodes, node) {
			return az
		}
	}
	return ""
}

// warnSingleNodeAZPin logs a Warn when a strict node-affinity pin binds the VM
// to an AZ node set of one or two nodes. Two hazards follow from a small
// pinned node set: (1) once every node in the set is simultaneously down
// (the only node, for a one-node set; both nodes, for a two-node set — e.g.
// one down and one drained for maintenance), PVE HA has no remaining node in
// the AZ to fail over to, so the VM cannot restart until a pinned node
// returns; (2) draining a node for maintenance (reboot, upgrade) can leave no
// legal placement target for the VM, wedging the maintenance operation. Both
// hazards are specific to strict pins — a non-strict (preferred) pin lets HA
// relocate off-AZ instead of being stuck. Deduplicates/trims azNodes the same
// way ensureNodeAffinityPin does, so a config with blank or repeated entries
// is judged on its effective node count, not its raw slice length. A no-op
// when strict is false or the effective node count is more than two.
func warnSingleNodeAZPin(az string, azNodes []string, strict bool, logger *log.Logger) {
	if !strict {
		return
	}
	nodesCSV := sortedNodesCSV(azNodes)
	if nodesCSV == "" {
		return
	}
	nodes := strings.Split(nodesCSV, ",")
	if len(nodes) > 2 {
		return
	}
	logger.Warn("create_vm: strict AZ node-affinity pin targets a small node set; "+
		"if every pinned node is simultaneously down or drained for maintenance, PVE HA has "+
		"nowhere in the AZ left to fail over to and the VM stays down until a pinned node returns "+
		"(map the AZ to >= 3 nodes to avoid this wedge risk, or set pve.ha.pin_az_strict=false "+
		"to allow HA to relocate off-AZ instead)",
		log.String("az", az), log.String("nodes", nodesCSV), log.Int("pinned_node_count", len(nodes)))
}

// pinRequest is one writer's contribution to a VM's merged pin.
type pinRequest struct {
	// Hard is a hard node constraint (a strict AZ, a PCI device), nil for none.
	Hard []string

	// Preferred holds the nodes of a non-strict AZ, which only add priority.
	Preferred []string

	// Placed is the node the CPI placed the VM on, or "" when the caller does
	// not know it.
	Placed string

	// DLB says the VM is in the DLB AZ, so PVE chooses its node.
	DLB bool

	// Storages are the storages of the VM's volumes, which the pin's comment
	// records. A pin is never written without them.
	Storages []string

	// Decision is the VM's eligibility decision, or nil when the caller made
	// none. With a decision the pin is the decision's (ha.DecisionPin), and the
	// other constraint fields are not read. The decision also lets the rule
	// writer remove a resource this call staged, or one whose CRM episode is the
	// one the decision read, when a repair of the confinement pin cannot be
	// verified.
	Decision *ha.VMDecision
}

// writeMergedPinRule writes the merged pin and returns the pin as it was
// planned, or nil when there was nothing to pin. The VM's HA resource must
// already exist, because create_vm stages it before any rule names it.
func writeMergedPinRule(
	ctx context.Context, deps Deps, profile *ha.Profile, vmid int, req pinRequest, logger *log.Logger,
) (*ha.Rule, error) {
	sid := haResourceSid(vmid)
	ruleName := naRuleNameFor(vmid)
	ident := haIdentity{Owner: deps.RequestDirectorUUID}
	marker := ident.marker()
	if len(req.Storages) == 0 {
		return nil, cpierrors.Cloud("node-affinity: pin %q for VM %d not written, because the VM's volumes name "+
			"no storage to record", ruleName, vmid)
	}

	// A hard constraint that no configured node satisfies leaves the VM nowhere
	// it may run. The check runs before any write, and the error is retriable
	// so the Director re-drives once the node set has changed.
	if hard := pinRequestHard(req); hard != nil {
		if _, err := ha.AdmissibleHard(hard, profileNodeNames(profile)); err != nil {
			return nil, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("node-affinity: pin %q not written for VM %d", ruleName, vmid))
		}
	}

	var planned *ha.Rule
	err := withHARulesLock(ctx, deps, vmid, logger, func() error {
		writer := newHARuleWriterFor(deps, profile, logger, req.Decision)
		res, err := writer.Apply(ctx, "node-affinity", func(set *ha.RuleSet) ([]ha.RuleOp, error) {
			cur := set.Find(ruleName)
			if cur != nil && haOwnershipOf(cur.Comment, ident.Owner) == haOwnedByOther {
				return nil, fmt.Errorf("node-affinity: rule %q carries another Director's ownership marker; "+
					"leaving it alone", ruleName)
			}
			if cur != nil && cur.Type != ha.RuleTypeNodeAffinity {
				return nil, fmt.Errorf("node-affinity: rule %q is a %s rule, not a node-affinity pin; leaving it alone",
					ruleName, cur.Type)
			}
			want, err := plannedPin(set, cur, profile, req, ruleName, marker, sid)
			if err != nil {
				// The VM's resource exists by now, so a pin that cannot be
				// built must fail the call in every mode rather than leave it
				// unconfined.
				return nil, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
					fmt.Sprintf("node-affinity: pin %q for VM %d", ruleName, vmid))
			}
			planned = want
			switch {
			case want == nil:
				// Nothing to pin. An existing pin stays: this path only ever
				// writes constraints, and removeNodeAffinityPin removes them.
				return nil, nil
			case cur == nil:
				return []ha.RuleOp{{Kind: ha.OpCreate, Rule: *want}}, nil
			case ha.SameRule(want, cur):
				return nil, nil
			}
			return []ha.RuleOp{{Kind: ha.OpUpdate, Rule: *want}}, nil
		})
		if err != nil {
			return err
		}
		if len(res.Wrote) == 0 {
			return nil
		}
		// A cheap re-read that the pin holds this VM, applied symmetrically with
		// anti-affinity when antiaffinity_verify is on.
		return verifyAntiAffinityMember(ctx, deps, ruleName, sid, logger)
	})
	if err != nil {
		return nil, err
	}
	return planned, nil
}

// pinRequestHard returns the hard node constraint of req: the decision's hard
// set when it carries a decision, and its own Hard otherwise.
func pinRequestHard(req pinRequest) []string {
	if req.Decision != nil {
		return req.Decision.Hard
	}
	return req.Hard
}

// plannedPin builds the pin req asks for against the rules set: the decision's
// pin when req carries a decision, and otherwise the merged pin of mergedPinSpec.
// Both carry the volume record of req.Storages.
func plannedPin(
	set *ha.RuleSet, cur *ha.Rule, profile *ha.Profile, req pinRequest, ruleName, marker, sid string,
) (*ha.Rule, error) {
	if req.Decision != nil {
		return ha.DecisionPin(req.Decision, ruleName, marker, req.Storages, profileNodeNames(profile),
			profileRebalancesOnStart(profile), inResourceAffinityRule(set, sid))
	}
	return ha.BuildPin(mergedPinSpec(set, cur, profile, req, ruleName, ha.WithVolumeRecord(marker, req.Storages), sid))
}

// profileRebalancesOnStart reports whether the cluster's CRS is known to
// rebalance a VM when the CRM starts it.
func profileRebalancesOnStart(profile *ha.Profile) bool {
	return profile != nil && profile.CRS.Known && profile.CRS.RebalanceOnStart
}

// mergedPinSpec builds the pin spec for req, keeping the hard constraint of a
// strict pin already written for the VM (see writeMergedPinRule).
func mergedPinSpec(
	set *ha.RuleSet, cur *ha.Rule, profile *ha.Profile, req pinRequest, ruleName, marker, sid string,
) ha.PinSpec {
	spec := ha.PinSpec{
		ID:               ruleName,
		Comment:          marker,
		SID:              sid,
		Hard:             req.Hard,
		Configured:       profileNodeNames(profile),
		Placed:           strings.TrimSpace(req.Placed),
		Preferred:        req.Preferred,
		RebalanceOnStart: profileRebalancesOnStart(profile),
		DLB:              req.DLB,
		SinglePriority:   inResourceAffinityRule(set, sid),
	}
	if cur == nil || !cur.Strict || cur.Disabled {
		return spec
	}
	keep := cur.NodeNames()
	if req.Hard != nil {
		hard := map[string]bool{}
		for _, n := range req.Hard {
			hard[strings.TrimSpace(n)] = true
		}
		keep = slices.DeleteFunc(keep, func(n string) bool { return !hard[n] })
	}
	if len(keep) > 0 && (spec.Placed == "" || slices.Contains(keep, spec.Placed)) {
		spec.Hard = keep
	}
	return spec
}

// vmInDLBAZ reports whether the VM asked for the DLB sentinel AZ.
func vmInDLBAZ(cp createVMCloudProps, cfg *config.CPIConfig) bool {
	sentinel := cfg.DLBAZName()
	return sentinel != "" && slices.Contains(buildAZOrder(cp, cfg, nil, nil), sentinel)
}

// removeNodeAffinityPin deletes a VM's node-affinity rule and deregisters its HA
// resource. Keyed solely on vmid (delete_vm has no AZ context). Idempotent and
// best-effort: a not-found rule/resource is a no-op, and it is safe to run
// alongside anti-affinity cleanup (both deregister the same HA resource with a
// not-found-tolerant purge). The delete runs under the rules lock, and a pin
// that carries another Director's marker stays.
func removeNodeAffinityPin(ctx context.Context, deps Deps, vmid int, logger *log.Logger) error {
	err := deleteNodeAffinityRule(ctx, deps, vmid, logger, nil)

	// Deregister the HA resource (purge removes it from any remaining rules).
	// Not-found is a no-op — anti-affinity cleanup may have purged it already.
	deregisterHaResource(ctx, deps.PVE.Cluster(), haResourceSid(vmid), deps.RequestDirectorUUID, logger)
	return err
}

// deleteNodeAffinityRule deletes a VM's node-affinity rule under the rules lock
// and leaves the VM's HA resource alone. A missing rule is a no-op, and a rule
// that carries another Director's marker stays. When keep is set, it answers
// for the rule as read under the lock, and a non-nil answer leaves the rule in
// place and is returned, so a caller removes only the rule it knows.
func deleteNodeAffinityRule(
	ctx context.Context, deps Deps, vmid int, logger *log.Logger, keep func(cur *ha.Rule) error,
) error {
	ruleName := naRuleNameFor(vmid)
	err := withHARulesLock(ctx, deps, vmid, logger, func() error {
		writer := newHARuleWriter(deps, nil, logger)
		foreign := false
		_, err := writer.Apply(ctx, "node-affinity cleanup", func(set *ha.RuleSet) ([]ha.RuleOp, error) {
			cur := set.Find(ruleName)
			if cur == nil {
				return nil, nil
			}
			if haOwnershipOf(cur.Comment, deps.RequestDirectorUUID) == haOwnedByOther {
				foreign = true
				return nil, nil
			}
			if keep != nil {
				if err := keep(cur); err != nil {
					return nil, err
				}
			}
			return []ha.RuleOp{{Kind: ha.OpDelete, Rule: ha.Rule{ID: ruleName}}}, nil
		})
		if foreign {
			logger.Debug("node-affinity: rule belongs to another Director; leaving it", log.String("rule", ruleName))
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("node-affinity: delete rule %q: %w", ruleName, err)
	}
	return nil
}

// sortedNodesCSV renders a deduplicated, sorted, comma-separated node list,
// dropping empty entries. Deterministic so a create_vm retry produces an
// identical rule.
func sortedNodesCSV(nodes []string) string {
	seen := make(map[string]struct{}, len(nodes))
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
