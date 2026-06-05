package handlers

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
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
// node), skips the DLB sentinel AZ (DLB intentionally un-pins), and logs any pin
// failure without failing create_vm. A no-op when pinning is disabled.
//
// Selective propagation (C1): a TypeRetriableCloud error (lock-timeout, verify
// failure) is returned so the director re-drives rather than silently losing the
// AZ pin guarantee. Generic HA-API failures (HA unconfigured, rule-write hiccup)
// are logged as warnings and do not fail create_vm, preserving the §7.21 intent.
func applyAZNodeAffinityPin(ctx context.Context, deps Deps, vmid int, cp createVMCloudProps, node string, logger *log.Logger) error {
	if !deps.Config.HANodeAffinityPinEnabled() {
		return nil
	}
	az := pinAZForNode(cp, deps.Config, node)
	if az == "" {
		// The chosen node is not in any requested AZ's node set (operator
		// target_node override, local-disk pin, or config.node fallback). There
		// is no AZ to make durable, so skip — but log so a silently-unpinned VM
		// is visible rather than mysterious.
		logger.Debug("create_vm: node-affinity pin skipped; placed node has no requested-AZ membership",
			log.Int(metadataKeyVMID, vmid), log.String("node", node))
		return nil
	}
	azNodes, ok := deps.Config.AZCandidates(az)
	if !ok {
		return nil
	}
	if pinErr := ensureNodeAffinityPin(ctx, deps, vmid, azNodes, deps.Config.PinAZStrict(), logger); pinErr != nil {
		if cpierrors.IsType(pinErr, cpierrors.TypeRetriableCloud) {
			// Lock-timeout or verify-failure: propagate so the director re-drives.
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

// ensureNodeAffinityPin writes a PVE HA node-affinity rule binding the VM to the
// AZ node set, so the AZ placement chosen at scoring survives HA failover and
// DLB rebalance. It is per-VM (one rule named bosh-na-<vmid>), so no membership
// scan is needed. strict selects a hard pin (HA will not relocate off the node
// set) versus a preferred pin. It is best-effort: every PVE failure (including
// "HA not configured") is returned for the caller to log, never to fail
// create_vm. azNodes empty is a no-op.
func ensureNodeAffinityPin(
	ctx context.Context, deps Deps, vmid int, azNodes []string, strict bool, logger *log.Logger,
) error {
	nodesCSV := sortedNodesCSV(azNodes)
	if nodesCSV == "" {
		return nil
	}
	svc := deps.PVE.Cluster()
	sid := haResourceSid(vmid)
	ruleName := naRuleNameFor(vmid)

	// Register the VM as an HA resource (idempotent; "already defined" is fine).
	if err := svc.CreateHaResources(ctx, &cluster.CreateHaResourcesParams{Sid: sid}); err != nil {
		if !isHaAlreadyExists(err) {
			logger.Warn("node-affinity: register HA resource failed (continuing best-effort)",
				log.String("sid", sid), log.Err(err))
		}
	}

	// A prior rule (e.g. a create_vm retry) is refreshed: PVE has no partial
	// edit for rules, so delete-then-create to reflect the current node set.
	existing, listErr := findHaRule(ctx, svc, ruleName)
	if listErr != nil {
		return fmt.Errorf("node-affinity: list HA rules: %w", listErr)
	}
	if existing != nil {
		if err := svc.DeleteHaRules(ctx, ruleName); err != nil && !isHaNotFound(err) {
			return fmt.Errorf("node-affinity: delete rule %q for refresh: %w", ruleName, err)
		}
	}
	if err := createNodeAffinityRule(ctx, svc, ruleName, sid, nodesCSV, strict); err != nil {
		return err
	}

	// The node-affinity rule is per-VM (bosh-na-<vmid>), so the only contention
	// is a create_vm retry of the SAME vmid — there is no cross-group/cross-VM
	// RMW hazard, and the dedicated per-VMID guest-VMID conflict model already
	// serializes same-vmid retries. The coarse cluster pool lock is therefore
	// intentionally NOT taken here. The read-after-write verify is cheap (one
	// re-list) and applied symmetrically with anti-affinity when enabled, to
	// catch a rule that a concurrent same-vmid retry left without this resource.
	return verifyAntiAffinityMember(ctx, deps, ruleName, sid, logger)
}

// removeNodeAffinityPin deletes a VM's node-affinity rule and deregisters its HA
// resource. Keyed solely on vmid (delete_vm has no AZ context). Idempotent and
// best-effort: a not-found rule/resource is a no-op, and it is safe to run
// alongside anti-affinity cleanup (both deregister the same HA resource with a
// not-found-tolerant purge).
func removeNodeAffinityPin(ctx context.Context, deps Deps, vmid int, logger *log.Logger) error {
	svc := deps.PVE.Cluster()
	ruleName := naRuleNameFor(vmid)
	sid := haResourceSid(vmid)

	var firstErr error
	if err := svc.DeleteHaRules(ctx, ruleName); err != nil && !isHaNotFound(err) {
		firstErr = fmt.Errorf("node-affinity: delete rule %q: %w", ruleName, err)
	}

	// Deregister the HA resource (purge removes it from any remaining rules).
	// Not-found is a no-op — anti-affinity cleanup may have purged it already.
	purge := true
	if err := svc.DeleteHaResources(ctx, sid, &cluster.DeleteHaResourcesParams{Purge: &purge}); err != nil {
		if !isHaNotFound(err) {
			logger.Debug("node-affinity: deregister HA resource (non-fatal)",
				log.String("sid", sid), log.Err(err))
		}
	}
	return firstErr
}

// createNodeAffinityRule creates a node-affinity rule pinning sid to nodesCSV.
// When strict is true PVE will not relocate the resource off the node set even
// on total node-set failure (hard AZ guarantee); when false the pin is
// preferred (HA may relocate off-set on failure).
func createNodeAffinityRule(ctx context.Context, svc cluster.Service, ruleName, sid, nodesCSV string, strict bool) error {
	nodes := nodesCSV
	comment := "BOSH AZ node-affinity pin for " + sid
	if err := svc.CreateHaRules(ctx, &cluster.CreateHaRulesParams{
		Rule:      ruleName,
		Type:      naRuleType,
		Nodes:     &nodes,
		Strict:    &strict,
		Resources: sid,
		Comment:   &comment,
	}); err != nil {
		return fmt.Errorf("node-affinity: create rule %q: %w", ruleName, err)
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
