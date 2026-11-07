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
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
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

// applyAZNodeAffinityPin is the create_vm step that writes the AZ node-affinity
// HA pin when enabled. It derives the AZ the VM was actually placed in from the
// chosen node (so it works for both the singular availability_zone and the
// plural availability_zones forms — the scorer picks one AZ but returns only the
// node), skips the DLB sentinel AZ (DLB intentionally un-pins), warns when a
// strict pin targets a single-node AZ (see warnSingleNodeAZPin), and logs any
// pin failure without failing create_vm. A no-op when pinning is disabled.
//
// Selective propagation: an error propagateHARuleError names (lock-timeout,
// verify failure, a hard constraint no configured node satisfies, and in
// managed mode every pin failure) is returned so the director re-drives rather
// than silently losing the AZ pin guarantee. In auto, generic HA-API failures
// (HA unconfigured, rule-write hiccup) are logged as warnings and do not fail
// create_vm, preserving the best-effort intent.
//
// The AZ pin is written into the VM's one merged pin, bosh-na-<vmid>, with the
// placed node at the highest priority. When no AZ resolves, the VM still gets
// the placed-node pin that ha-rebalance-on-start needs (ensureRebalancePin).
func applyAZNodeAffinityPin(ctx context.Context, deps Deps, vmid int, cp createVMCloudProps, node string, logger *log.Logger) error {
	if !deps.Config.HANodeAffinityPinEnabled() {
		return nil
	}
	az := pinAZForNode(cp, deps.Config, node)
	if az == "" {
		// The chosen node is not in any requested AZ's node set. Either the VM
		// carried no availability_zone at all, or it landed outside the
		// requested AZ (operator target_node override, local-disk pin, or
		// config.node fallback). There is no AZ to make durable, so skip.
		//
		// Warn, not Debug: reaching here means pve.ha.pin_az is "on", so
		// the operator asked for every VM to be pinned and this one is not.
		// BOSH never passes the cloud-config AZ name to create_vm (the AZ only
		// selects the CPI and the subnet), so the CPI learns it solely from
		// cloud_properties.availability_zone. An operator who sets
		// pve.ha.pin_az and az_map but omits availability_zone from the
		// AZ's cloud_properties gets a wholly inert feature, and at Debug the
		// only evidence was a line nobody sees at the default log level. The
		// message names the cloud-config prerequisite because that is the fix
		// in the common case.
		logger.Warn("create_vm: HA node-affinity pin is enabled but this VM was not pinned — no availability_zone "+
			"resolved for the placed node; BOSH does not pass the cloud-config AZ name to the CPI, so each cloud-config AZ needs "+
			"cloud_properties.availability_zone naming a key of placement.az_map, and BOSH merges an AZ's cloud_properties "+
			"into every VM in that AZ (a VM pinned to a node "+
			"outside its requested AZ by target_node, a local-disk pin, or the config.node fallback reaches here too)",
			log.Int(metadataKeyVMID, vmid), log.String("node", node))
		// A VM with no AZ still keeps the node the CPI chose when the CRM
		// would otherwise rebalance it at start.
		if pinErr := ensureRebalancePin(ctx, deps, vmid, cp, node, logger); pinErr != nil {
			if propagateHARuleError(deps, pinErr) {
				return pinErr
			}
			logger.Warn("create_vm: HA placed-node pin not applied (non-fatal)",
				log.Int(metadataKeyVMID, vmid), log.String("node", node), log.Err(pinErr))
		}
		return nil
	}
	azNodes, ok := deps.Config.AZCandidates(az)
	if !ok {
		return nil
	}
	strict := deps.Config.HAPinAZStrictValue()
	warnSingleNodeAZPin(az, azNodes, strict, logger)
	req := pinRequest{Placed: node, Register: true}
	if strict {
		req.Hard = azNodes
	} else {
		req.Preferred = azNodes
	}
	if pinErr := ensureMergedPin(ctx, deps, vmid, req, logger); pinErr != nil {
		if propagateHARuleError(deps, pinErr) {
			// Lock-timeout, verify failure, or a managed-mode pin that is not in
			// effect: propagate so the director re-drives.
			return pinErr
		}
		logger.Warn("create_vm: HA node-affinity pin not fully applied (non-fatal)",
			log.Int(metadataKeyVMID, vmid), log.String("az", az), log.Err(pinErr))
	}
	return nil
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

// ensureNodeAffinityPin writes the node constraint azNodes into the VM's merged
// PVE HA node-affinity pin, bosh-na-<vmid>, so the placement chosen at scoring
// survives HA failover and DLB rebalance. strict makes azNodes a hard
// constraint, which confines the VM to them, and otherwise they are preferred
// nodes, which only add priority. A single node is also taken as the placed
// node. It is best-effort: every PVE failure (including "HA not configured") is
// returned for the caller to log, never to fail create_vm. azNodes empty is a
// no-op. See ensureMergedPin for how the pin merges with one already written.
func ensureNodeAffinityPin(
	ctx context.Context, deps Deps, vmid int, azNodes []string, strict bool, logger *log.Logger,
) error {
	nodesCSV := sortedNodesCSV(azNodes)
	if nodesCSV == "" {
		return nil
	}
	nodes := strings.Split(nodesCSV, ",")
	req := pinRequest{Register: true}
	if strict {
		req.Hard = nodes
	} else {
		req.Preferred = nodes
	}
	if len(nodes) == 1 {
		req.Placed = nodes[0]
	}
	return ensureMergedPin(ctx, deps, vmid, req, logger)
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

	// Register registers the VM as an HA resource before the pin is written.
	Register bool

	// Decision is the VM's eligibility decision, or nil when the caller made
	// none. It lets the rule writer remove a resource this call staged, or one
	// whose CRM episode is the one the decision read, when a repair of the
	// confinement pin cannot be verified.
	Decision *ha.VMDecision
}

// ensureMergedPin writes the VM's one merged node-affinity pin. The pin names
// every node of the hard admissible set and is strict when that set is smaller
// than the cluster's configured nodes. The placed node, the preferred nodes,
// and the rest get priorities 2, 1, and 0, flattened to one value while the VM
// is in a resource-affinity rule, because PVE refuses several priorities there.
//
// The writers of a VM's constraints run one after another (the AZ pin, then the
// PCI pin), so a strict pin already written for the VM is a hard constraint the
// new one must keep. Its nodes are intersected with the new hard set, and the
// result replaces the new hard set when it is non-empty and holds the placed
// node. An AZ pin of {a,b,c} and a PCI pin of {a} so give strict {a} in either
// order. A strict pin that shares no node with the new constraint, or that does
// not hold the placed node, is left from an earlier attempt and is replaced.
//
// The pin is written under the rules lock with a PUT that carries the read's
// digest, so it changes in place and never disappears between a delete and a
// create. A pin carrying another Director's marker is never touched. The PVE 9
// gate runs before the lock.
func ensureMergedPin(ctx context.Context, deps Deps, vmid int, req pinRequest, logger *log.Logger) error {
	ruleName := naRuleNameFor(vmid)
	ident := haIdentity{Owner: deps.RequestDirectorUUID}
	if ident.Owner == "" {
		// A pin the CPI cannot mark as its own is one it could never edit or
		// clean up safely, so a request without a director_uuid writes none.
		return fmt.Errorf("node-affinity: the request carries no director_uuid to mark rule %q with", ruleName)
	}
	profile, err := haRuleWriteProfile(ctx, deps)
	if err != nil {
		return fmt.Errorf("node-affinity: %w", err)
	}
	return writeMergedPin(ctx, deps, profile, vmid, req, logger)
}

// writeMergedPin is ensureMergedPin after the gate, for a caller that already
// holds the gated profile.
func writeMergedPin(
	ctx context.Context, deps Deps, profile *ha.Profile, vmid int, req pinRequest, logger *log.Logger,
) error {
	svc := deps.PVE.Cluster()
	sid := haResourceSid(vmid)
	ruleName := naRuleNameFor(vmid)
	ident := haIdentity{Owner: deps.RequestDirectorUUID}
	marker := ident.marker()

	// A hard constraint that no configured node satisfies leaves the VM nowhere
	// it may run. The check runs before the registration, so the CPI never
	// leaves an HA resource without the confinement it needs, and the error is
	// retriable so the Director re-drives once the node set has changed.
	if req.Hard != nil {
		if _, err := ha.AdmissibleHard(req.Hard, profileNodeNames(profile)); err != nil {
			return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
				fmt.Sprintf("node-affinity: pin %q not written and VM %d not registered with HA", ruleName, vmid))
		}
	}

	if req.Register {
		// Register the VM as an HA resource (idempotent; "already defined" is fine).
		if err := svc.CreateHaResources(ctx, &cluster.CreateHaResourcesParams{Sid: sid, Comment: &marker}); err != nil {
			if !isHaAlreadyExists(err) {
				logger.Warn("node-affinity: register HA resource failed (continuing best-effort)",
					log.String("sid", sid), log.Err(err))
			}
		}
	}

	return withHARulesLock(ctx, deps, vmid, logger, func() error {
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
			want, err := ha.BuildPin(mergedPinSpec(set, cur, profile, req, ruleName, marker, sid))
			if err != nil {
				// The VM is registered by now, so a pin that cannot be built
				// must fail the call in every mode rather than leave it
				// unconfined.
				return nil, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
					fmt.Sprintf("node-affinity: pin %q for VM %d", ruleName, vmid))
			}
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
}

// mergedPinSpec builds the pin spec for req, keeping the hard constraint of a
// strict pin already written for the VM (see ensureMergedPin).
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
		RebalanceOnStart: profile != nil && profile.CRS.Known && profile.CRS.RebalanceOnStart,
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

// ensureRebalancePin writes the placed-node pin a VM with no node constraint
// needs while the cluster has ha-rebalance-on-start on, which would otherwise
// let the CRM start the VM on a node the CPI did not choose. The pin is
// non-strict and names only the placed node. It is written only for a VM that
// is already an HA resource and is not in the DLB AZ, because DLB exists to let
// PVE choose. Every other case is a no-op.
func ensureRebalancePin(
	ctx context.Context, deps Deps, vmid int, cp createVMCloudProps, node string, logger *log.Logger,
) error {
	if deps.RequestDirectorUUID == "" || strings.TrimSpace(node) == "" || vmInDLBAZ(cp, deps.Config) {
		return nil
	}
	got, err := deps.PVE.Cluster().GetHaResources(ctx, haResourceSid(vmid))
	switch {
	case err != nil && (isHaNotFound(err) || pve.IsNotFound(err)):
		return nil
	case err != nil:
		return fmt.Errorf("node-affinity: read HA resource for the placed-node pin: %w", err)
	case got == nil:
		return nil
	}
	profile, err := haRuleWriteProfile(ctx, deps)
	if err != nil {
		return fmt.Errorf("node-affinity: %w", err)
	}
	if !profile.CRS.Known || !profile.CRS.RebalanceOnStart {
		return nil
	}
	return writeMergedPin(ctx, deps, profile, vmid, pinRequest{Placed: node}, logger)
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
	svc := deps.PVE.Cluster()
	ruleName := naRuleNameFor(vmid)
	sid := haResourceSid(vmid)

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
			return []ha.RuleOp{{Kind: ha.OpDelete, Rule: ha.Rule{ID: ruleName}}}, nil
		})
		if foreign {
			logger.Debug("node-affinity: rule belongs to another Director; leaving it", log.String("rule", ruleName))
		}
		return err
	})
	if err != nil {
		err = fmt.Errorf("node-affinity: delete rule %q: %w", ruleName, err)
	}

	// Deregister the HA resource (purge removes it from any remaining rules).
	// Not-found is a no-op — anti-affinity cleanup may have purged it already.
	deregisterHaResource(ctx, svc, sid, deps.RequestDirectorUUID, logger)
	return err
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
