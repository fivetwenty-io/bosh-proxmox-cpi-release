// create_vm_placement.go resolves node and availability-zone placement
// for new VMs: AZ ordering, node scoring/filtering, fallback/alternate node
// selection, and the storage-headroom gate that feeds placement decisions.
// Split out of create_vm.go (mechanical move, no behavior change).
package handlers

import (
	"context"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/placement"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// resolveVMShape derives the createVMShape from deps.Config + parsed args.
// Returns cpierrors.CloudError if the target node cannot be determined.
func resolveVMShape(ctx context.Context, deps Deps, parsed *createVMParsedArgs) (*createVMShape, error) {
	cp := parsed.cloudProps

	// Anti-affinity group tag (Tier 2, scheduler-soft spreading). Only computed
	// when anti-affinity is enabled; otherwise the scorer ignores group membership
	// and behavior is identical to Tier 1.
	groupTag := antiAffinityGroupTag(deps.Config, parsed.env)

	node, err := resolveTargetNode(ctx, deps, cp, groupTag, parsed.diskCIDs, parsed.cloudPropsMap)
	if err != nil {
		return nil, err
	}
	return buildVMShapeForNode(ctx, deps, parsed, node)
}

// resolveVMShapeWithAlternates is like resolveVMShape but additionally returns
// the ordered list of alternate node names (from the same scored candidate pass)
// capped at fallbackMax. Used by the post-selection fallback path when
// PlacementFallbackMaxValue() > 0.
//
// Returns (shape, nil, err) when placement scoring produces no alternates
// (operator target_node, static config.node, or single-node cluster), so the
// caller must treat a nil alternates slice as "no fallback available".
func resolveVMShapeWithAlternates(
	ctx context.Context,
	deps Deps,
	parsed *createVMParsedArgs,
	fallbackMax int,
) (shape *createVMShape, alternates []string, err error) {
	cp := parsed.cloudProps
	groupTag := antiAffinityGroupTag(deps.Config, parsed.env)

	winner, alts, nodeErr := resolveTargetNodeWithFallbacks(
		ctx, deps, cp, groupTag, parsed.diskCIDs, nil, parsed.cloudPropsMap, fallbackMax)
	if nodeErr != nil {
		return nil, nil, nodeErr
	}

	s, err := buildVMShapeForNode(ctx, deps, parsed, winner)
	if err != nil {
		return nil, nil, err
	}
	return s, alts, nil
}

// resolveTargetNode determines which PVE node the new VM will land on. See
// resolveTargetNodeWithFallbacks — the single implementation of the placement
// decision tree — for the full algorithm; this wrapper is the
// production-default entry (non-deterministic shuffle, no alternates).
func resolveTargetNode(ctx context.Context, deps Deps, cp createVMCloudProps, groupTag string, diskCIDs []string, cloudPropsMap map[string]any) (string, error) {
	return resolveTargetNodeWithRNG(ctx, deps, cp, groupTag, diskCIDs, nil, cloudPropsMap)
}

// resolveTargetNodeWithRNG is the testable implementation of resolveTargetNode.
// rng controls AZ shuffle order; pass nil for production (non-deterministic).
// cloudPropsMap is the raw cloud_properties map used to build the layered resolver
// for per-call placement weight overrides and AZ resolution via vm_type profiles.
// Pass nil to skip resolver-based overrides.
//
// One-line delegation: resolveTargetNodeWithFallbacks with fallbackMax == 0 is
// this exact algorithm — buildAlternates returns nil alternates at zero and
// scoreAndPickWithRanked picks identically to the ranked-less variant this
// function used to duplicate (~260 lines, byte-equivalent branch structure,
// verified by diff before the copy was deleted). Keeping ONE implementation
// means every future placement change lands on both the fallback and
// non-fallback paths by construction instead of by remembering to edit twice.
func resolveTargetNodeWithRNG(
	ctx context.Context,
	deps Deps,
	cp createVMCloudProps,
	groupTag string,
	diskCIDs []string,
	rng *rand.Rand,
	cloudPropsMap map[string]any,
) (string, error) {
	node, _, err := resolveTargetNodeWithFallbacks(ctx, deps, cp, groupTag, diskCIDs, rng, cloudPropsMap, 0)
	return node, err
}

// resolveTargetNodeWithFallbacks is the single implementation of the node
// placement decision tree; every entry point (resolveTargetNode,
// resolveTargetNodeWithRNG, resolveVMShapeWithAlternates) funnels here.
//
// Decision tree (evaluated in order):
//  1. cp.TargetNode != "" → operator override; skip scoring entirely (backward compat).
//  2. deps.Config.PlacementEnabled() == true → live placement scoring:
//     a. Build AZ order: singular availability_zone → single-element list (backward
//     compat). Plural availability_zones → iterate in operator order (shuffle if
//     placement.az_shuffle is true). Append config.placement.az_fallback_order.
//     b. GatherNodeFacts once (cluster-wide). ExcludeMaintenanceNodes wired from
//     config default (true).
//     c. For each AZ: resolve candidate set, Filter+Score+Pick. Advance to next
//     AZ on empty-after-filter. Return chosen node on first viable AZ.
//     d. No viable AZ: classify rejection causes. Transient causes →
//     cpierrors.Retriable. Permanent (bad AZ name) → cpierrors.Cloud.
//     e. After all AZs exhausted, fall back to config.node with a warning.
//  3. deps.Config.PlacementEnabled() == false → deps.Config.Node (legacy behavior).
//  4. All paths: if the resolved node is still "" → CloudError.
//
// diskCIDs carries the persistent disk CIDs passed to create_vm. When non-empty,
// disk fault-domain constraints are derived before placement runs:
//   - local-storage disks pin the VM to the disk's home node (hard constraint).
//   - shared-storage disks with an AZ label constrain the AZ order.
//   - bare legacy CIDs (no metadata) impose no constraint.
//
// groupTag, when non-empty, is the anti-affinity tag (e.g. "job--diego-cell")
// that activates scheduler-soft same-group spreading. rng is injected for
// deterministic shuffle in tests; pass nil for production.
//
// fallbackMax caps the returned alternates — the tail of the ranked list
// starting at rank 2, all of which passed the same Filter constraints (same
// AZ, same maintenance/CPU/mem filter) as the winner. fallbackMax == 0 is the
// plain single-winner path (alternates nil). When the winner comes from
// Branch 1 (operator target_node), Branch 3 (static config.node), or the
// config.node fallback inside Branch 2, no ranked alternates are available
// and alternates is nil — callers must treat nil as "no fallback candidates".
//
//nolint:gocognit,gocyclo // Single home of the multi-AZ placement loop; inherent complexity.
func resolveTargetNodeWithFallbacks(
	ctx context.Context,
	deps Deps,
	cp createVMCloudProps,
	groupTag string,
	diskCIDs []string,
	rng *rand.Rand,
	cloudPropsMap map[string]any,
	fallbackMax int,
) (winner string, alternates []string, err error) {
	// deps.Log(ctx) is nil-Logger-safe and prefers ctx's per-request
	// span-correlated logger over deps.Logger when present (see
	// resolveTargetNodeWithRNG above for the full rationale).
	logger := deps.Log(ctx)

	diskConstraints, dcErr := deriveDiskFaultConstraints(ctx, deps, diskCIDs)
	if dcErr != nil {
		return "", nil, dcErr
	}

	// Branch 1: operator pin — no alternates.
	if cp.TargetNode != "" {
		if diskConstraints.requiredLocalNode != "" && diskConstraints.requiredLocalNode != cp.TargetNode {
			return "", nil, cpierrors.Cloud(
				"create_vm: cloud_properties.target_node=%q conflicts with local disk placement constraint (disk node=%q); "+
					"set target_node=%q or move the disk to shared storage",
				cp.TargetNode, diskConstraints.requiredLocalNode, diskConstraints.requiredLocalNode,
			)
		}
		logger.Debug("create_vm: node selection: operator override via target_node",
			log.String("node", cp.TargetNode),
		)
		return cp.TargetNode, nil, nil
	}

	var cpResolver *layeredResolver
	if deps.Config != nil {
		var resolverErr error
		cpResolver, resolverErr = newLayeredResolver(cloudPropsMap, deps.Config)
		if resolverErr != nil {
			return "", nil, resolverErr
		}
	}

	// Branch 2: live placement scoring — alternates available.
	if deps.Config.PlacementEnabled() && deps.PVE != nil {
		azOrder := buildAZOrder(cp, deps.Config, rng, cpResolver)

		if len(diskConstraints.requiredAZs) > 0 {
			azOrder, dcErr = applyDiskAZConstraint(azOrder, diskConstraints.requiredAZs)
			if dcErr != nil {
				return "", nil, dcErr
			}
		}

		for _, az := range azOrder {
			_, ok := deps.Config.AZCandidates(az)
			if !ok && (az != deps.Config.DLBAZName() || deps.Config.DLBAZName() == "") {
				return "", nil, cpierrors.Cloud(
					"create_vm: availability_zone %q is not defined in placement.az_map; "+
						"add the AZ to config.placement.az_map or remove availability_zone from cloud_properties",
					az,
				)
			}
		}

		storageName := deps.Config.VMStorage
		excludeMaintenance := deps.Config.ExcludeMaintenanceNodesEnabled()
		facts, gatherErr := placement.GatherNodeFacts(ctx,
			deps.PVE.Cluster(),
			deps.PVE.Nodes(),
			logger,
			placement.GatherOptions{
				StorageName:             storageName,
				GroupTag:                groupTag,
				ExcludeMaintenanceNodes: excludeMaintenance,
				MaintenanceNodeTags:     deps.Config.MaintenanceNodeTagsValue(),
			},
		)
		if gatherErr != nil {
			return "", nil, cpierrors.Wrap(pve.WrapError(gatherErr),
				"create_vm: placement: gather node facts")
		}

		w := deps.Config.EffectiveWeights()
		weights := placement.Weights{
			Mem:          w.Mem,
			Storage:      w.Storage,
			CPU:          w.CPU,
			GuestCount:   w.GuestCount,
			MemorySignal: deps.Config.MemorySignalValue(),
		}
		if cpResolver != nil {
			if f, found := cpResolver.Float("placement_weight_mem"); found {
				weights.Mem = f
			}
			if f, found := cpResolver.Float("placement_weight_storage"); found {
				weights.Storage = f
			}
			if f, found := cpResolver.Float("placement_weight_cpu"); found {
				weights.CPU = f
			}
			if f, found := cpResolver.Float("placement_weight_guest_count"); found {
				weights.GuestCount = f
			}
		}
		if groupTag != "" {
			weights.AntiAffinity = placement.DefaultWeights().AntiAffinity
		}

		localPin := diskConstraints.requiredLocalNode
		if localPin != "" {
			var pinnedFact *placement.NodeFacts
			for i := range facts {
				if facts[i].Node == localPin {
					pinnedFact = &facts[i]
					break
				}
			}
			if pinnedFact == nil {
				return "", nil, cpierrors.Cloud(
					"create_vm: local disk is pinned to node %q but that node "+
						"is not reachable in the cluster (offline, removed, or unknown); "+
						"ensure the disk's home node is online before creating the VM",
					localPin,
				)
			}
			if pinnedFact.InMaintenance {
				return "", nil, cpierrors.Cloud(
					"create_vm: local disk is pinned to node %q but that node "+
						"is currently in maintenance; wait for maintenance to complete or "+
						"migrate the disk to a different node",
					localPin,
				)
			}
			if !pinnedFact.Online {
				return "", nil, cpierrors.Cloud(
					"create_vm: local disk is pinned to node %q but that node "+
						"is offline; bring the node online before creating the VM",
					localPin,
				)
			}
			logger.Debug("create_vm: node selection: local disk pins node",
				log.String("node", localPin),
			)
			return localPin, nil, nil
		}

		// Build PCI checker for fallback path (same logic as resolveTargetNodeWithRNG).
		var pciAddrsFB []string
		var pciCheckerFnFB func(string) (bool, error)
		if len(cp.PCIPassthroughs) > 0 {
			pciAddrsFB = make([]string, len(cp.PCIPassthroughs))
			for i, pt := range cp.PCIPassthroughs {
				pciAddrsFB[i] = pt.Address
			}
			pciCheckerFnFB = buildPCIChecker(ctx, deps.PVE.Nodes(), pciAddrsFB)
		}

		// Storage-capacity hard filter (same computation as non-fallback path; 0 when gate off).
		var requiredStorageBytesFB int64
		if deps.Config.ReserveStorageHeadroomEnabled() {
			requiredStorageBytesFB = computeRequiredStorageBytes(deps.Config, cp, storageName)
		}

		// Storage-utilization ceiling gate (same computation as non-fallback path).
		utilCeilingPctFB, utilAddBytesFB, warnUtilChosenFB := utilizationGateForRequest(deps.Config, cp, storageName, facts, logger)

		allRejections := make(map[string]string)

		if len(azOrder) == 0 {
			req := placement.Request{
				ExcludeMaintenanceNodes: excludeMaintenance,
				RequiredPCIAddresses:    pciAddrsFB,
				PCIChecker:              pciCheckerFnFB,
				RequiredStorageBytes:    requiredStorageBytesFB,
				MaxUtilizationPct:       utilCeilingPctFB,
				PlannedAddBytes:         utilAddBytesFB,
			}
			pass, rejections := placement.Filter(facts, req)
			mergeRejections(allRejections, rejections)
			logFilterRejections(logger, rejections, "")
			if chosen, ranked := scoreAndPickWithRanked(pass, weights, logger, ""); chosen != "" {
				warnUtilChosenFB(chosen)
				alts := buildAlternates(chosen, ranked, fallbackMax)
				return chosen, alts, nil
			}
		} else {
			for _, az := range azOrder {
				candidateSet, skipSilently := resolveAZCandidatesValidated(az, deps.Config, logger)
				if skipSilently {
					continue
				}
				req := placement.Request{
					CandidateNodes:          candidateSet,
					ExcludeMaintenanceNodes: excludeMaintenance,
					RequiredPCIAddresses:    pciAddrsFB,
					PCIChecker:              pciCheckerFnFB,
					RequiredStorageBytes:    requiredStorageBytesFB,
					MaxUtilizationPct:       utilCeilingPctFB,
					PlannedAddBytes:         utilAddBytesFB,
				}
				pass, rejections := placement.Filter(facts, req)
				mergeRejections(allRejections, rejections)
				logFilterRejections(logger, rejections, az)
				if chosen, ranked := scoreAndPickWithRanked(pass, weights, logger, az); chosen != "" {
					warnUtilChosenFB(chosen)
					alts := buildAlternates(chosen, ranked, fallbackMax)
					return chosen, alts, nil
				}
				logger.Debug("create_vm: placement: AZ exhausted, trying next",
					log.String("az", az),
				)
			}
		}

		// All AZs exhausted — fall back to config.node (no alternates).
		fallback := deps.Config.Node
		logger.Warn("create_vm: placement: no viable candidates; falling back to config.node",
			log.String("fallback", fallback),
		)
		if fallback == "" {
			if classifyFilterResult(allRejections) {
				return "", nil, cpierrors.Retriable(
					"create_vm: no viable placement candidates (transient); "+
						"all nodes rejected: %s",
					formatRejections(allRejections),
				)
			}
			return "", nil, cpierrors.Cloud(
				"create_vm: no viable placement candidates; "+
					"all nodes rejected: %s",
				formatRejections(allRejections),
			)
		}
		return fallback, nil, nil
	}

	// Branch 3: placement disabled or PVE nil — no alternates.
	if diskConstraints.requiredLocalNode != "" {
		logger.Debug("create_vm: node selection: local disk pins node (placement disabled)",
			log.String("node", diskConstraints.requiredLocalNode),
		)
		return diskConstraints.requiredLocalNode, nil, nil
	}

	node := deps.Config.Node
	if node == "" {
		return "", nil, cpierrors.Cloud(
			"create_vm: target node not set in cloud_properties.target_node or config.node",
		)
	}
	logger.Debug("create_vm: node selection: placement disabled, using config.node",
		log.String("node", node),
	)
	return node, nil, nil
}

// buildAlternates extracts up to fallbackMax alternate node names from the ranked
// list, skipping the winner (first entry). Returns nil when fallbackMax == 0 or
// the ranked list has only one entry.
func buildAlternates(winner string, ranked []string, fallbackMax int) []string {
	if fallbackMax <= 0 || len(ranked) <= 1 {
		return nil
	}
	alts := make([]string, 0, fallbackMax)
	for _, n := range ranked {
		if n == winner {
			continue
		}
		alts = append(alts, n)
		if len(alts) >= fallbackMax {
			break
		}
	}
	if len(alts) == 0 {
		return nil
	}
	return alts
}

// applyDiskAZConstraint reconciles the VM's AZ order with the AZs required by
// shared-storage persistent disk CIDs.
//
// Rules (all non-retriable — disk AZ conflicts are operator configuration errors):
//   - VM AZ order empty: return the sorted required AZ list (constrain to disk AZs).
//   - VM AZ order non-empty: return only the AZs present in both the VM order and
//     requiredAZs, in the VM's original order (intersection). If the intersection
//     is empty, return a CloudError: the VM's AZ configuration is incompatible with
//     the disk's AZ requirement.
func applyDiskAZConstraint(azOrder []string, requiredAZs map[string]struct{}) ([]string, error) {
	if len(requiredAZs) == 0 {
		return azOrder, nil
	}

	if len(azOrder) == 0 {
		// No VM AZ preference: constrain to disk AZs in sorted order for determinism.
		result := make([]string, 0, len(requiredAZs))
		for az := range requiredAZs {
			result = append(result, az)
		}
		sort.Strings(result)
		return result, nil
	}

	// Intersect: keep VM AZ order but drop AZs not in requiredAZs.
	result := make([]string, 0, len(azOrder))
	for _, az := range azOrder {
		if _, required := requiredAZs[az]; required {
			result = append(result, az)
		}
	}
	if len(result) == 0 {
		reqList := make([]string, 0, len(requiredAZs))
		for az := range requiredAZs {
			reqList = append(reqList, az)
		}
		sort.Strings(reqList)
		return nil, cpierrors.Cloud(
			"create_vm: VM availability_zone(s) %v do not include the AZ(s) required "+
				"by persistent disk(s): %v; update cloud_properties.availability_zone(s) "+
				"to include a matching AZ, or move the disk(s) to shared storage without an AZ label",
			azOrder, reqList,
		)
	}
	return result, nil
}

// buildAZOrder constructs the ordered AZ list for a placement attempt.
//
// Priority (highest to lowest):
//  1. cp.AvailabilityZone (singular, per-call) — backward compat; returns 1-elem slice, no fallback.
//  2. cp.AvailabilityZones (plural, per-call) — iterate in operator order.
//  3. resolver.String("availability_zone") — singular from profile; same semantics as #1.
//  4. resolver.StringSlice("availability_zones") — plural from profile; feeds multi-AZ path.
//  5. cfg.AZFallbackOrderValue() — config-level fallback appended after any plural list.
//
// Steps 3–4 are only consulted when both per-call fields are absent (empty).
// Pass a nil resolver to skip profile-based AZ resolution entirely (byte-identical to the
// pre-resolver behavior).
func buildAZOrder(cp createVMCloudProps, cfg *config.CPIConfig, rng *rand.Rand, resolver *layeredResolver) []string {
	// Singular per-call takes precedence — backward compat, no multi-AZ behavior.
	if cp.AvailabilityZone != "" {
		return []string{cp.AvailabilityZone}
	}

	// Plural per-call list is the starting point for multi-AZ behavior.
	startList := cp.AvailabilityZones

	// When the per-call fields are both absent, consult the resolver for profile-supplied AZ.
	if len(startList) == 0 && resolver != nil {
		if az, found := resolver.String("availability_zone"); found {
			// Singular profile AZ: same backward-compat semantics as cp.AvailabilityZone.
			return []string{az}
		}
		if azs, found := resolver.StringSlice("availability_zones"); found {
			// Plural profile AZ list: use as starting point for multi-AZ + fallback logic.
			startList = azs
		}
	}

	if len(startList) == 0 && len(cfg.AZFallbackOrderValue()) == 0 {
		return nil // no AZ constraint
	}

	// Start from the resolved list (copy to avoid mutating caller's slice or resolver output).
	order := make([]string, len(startList))
	copy(order, startList)

	if cfg.AZShuffleEnabled() && len(order) > 1 {
		if rng == nil {
			rng = rand.New(rand.NewSource(time.Now().UnixNano())) // #nosec G404 -- shuffle; non-cryptographic
		}
		rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
	}

	// Append fallback AZs not already in the list.
	inOrder := make(map[string]struct{}, len(order))
	for _, az := range order {
		inOrder[az] = struct{}{}
	}
	for _, az := range cfg.AZFallbackOrderValue() {
		if _, already := inOrder[az]; !already {
			order = append(order, az)
			inOrder[az] = struct{}{}
		}
	}
	return order
}

// diskFaultConstraints carries the hard placement constraints derived from
// persistent disk CIDs before VM placement runs.
//
// requiredLocalNode, when non-empty, is the single PVE node that all
// local-storage disks share. The VM must land on this node.
//
// requiredAZs, when non-empty, is the set of AZ labels from shared-storage
// disks whose CID metadata carries an AZ. The VM's AZ order must intersect
// this set when an AZ is configured; if not, placement is constrained to only
// those AZs.
type diskFaultConstraints struct {
	// requiredLocalNode is set when one or more local-storage disks have a node
	// recorded in their CID metadata. Empty means no local-node pin.
	requiredLocalNode string
	// requiredAZs collects AZ labels from shared-storage disks. Empty means
	// no AZ constraint from persistent disks.
	requiredAZs map[string]struct{}
}

// deriveDiskFaultConstraints inspects each disk CID and builds the set of
// placement constraints it implies. Bare legacy CIDs (no metadata) are silently
// skipped to preserve backward compatibility.
//
// Errors returned:
//   - Two or more local-storage disks on different nodes → cpierrors.Cloud.
//   - Backend resolution failure → cpierrors.Wrap (unexpected; safe to retry).
//
// The ctx is used only for backend Resolve calls (cached in production).
func deriveDiskFaultConstraints(ctx context.Context, deps Deps, diskCIDs []string) (diskFaultConstraints, error) {
	var c diskFaultConstraints
	if len(diskCIDs) == 0 {
		return c, nil
	}

	resolver := backendResolverOrDefault(deps)
	localNodes := make(map[string]struct{}) // unique local nodes seen

	for _, cid := range diskCIDs {
		if cid == "" {
			continue
		}
		_, meta, err := pve.ParseEncodedDiskCID(cid)
		if err != nil || meta == nil {
			// Meta-less CID (envelope without metadata, bare legacy) or parse
			// failure (malformed envelope or legacy suffix): impose no
			// constraint. Failing open is safe — the Director replays CIDs the
			// CPI emitted, so a malformed CID here is effectively impossible,
			// and the caller already validated CIDs at parse time.
			continue
		}

		// Determine backend kind for this disk's pool.
		pool := meta.Pool
		if pool == "" {
			// Pool absent from meta but node/AZ may still be set (e.g. CID
			// written by an older CPI version that set Node/AZ without Pool).
			// Derive the pool from the bare CID so the node/AZ constraint is
			// not silently dropped. Fail closed: if ParseDiskCID cannot extract
			// a storage prefix, skip with no constraint (cannot classify).
			if meta.Node == "" && meta.AZ == "" {
				// Truly empty meta — legacy upgrade path, no constraint.
				continue
			}
			bareCID, _, parseErr := pve.ParseEncodedDiskCID(cid)
			if parseErr != nil {
				continue
			}
			derivedPool, _, parseErr := pve.ParseDiskCID(bareCID)
			if parseErr != nil {
				// Bare CID malformed; cannot classify. Skip — fail closed.
				continue
			}
			pool = derivedPool
		}

		backend, resolveErr := resolver.Resolve(ctx, pool)
		if resolveErr != nil {
			return diskFaultConstraints{}, cpierrors.Wrap(resolveErr,
				"create_vm: fault-domain: cannot resolve backend for disk pool "+pool)
		}

		if backend.Kind() == pve.BackendLocal {
			if meta.Node != "" {
				localNodes[meta.Node] = struct{}{}
			}
		} else {
			// Shared backend: AZ constraint only.
			if meta.AZ != "" {
				if c.requiredAZs == nil {
					c.requiredAZs = make(map[string]struct{})
				}
				c.requiredAZs[meta.AZ] = struct{}{}
			}
		}
	}

	// Validate local-node set: all local disks must share one node.
	if len(localNodes) > 1 {
		nodes := make([]string, 0, len(localNodes))
		for n := range localNodes {
			nodes = append(nodes, n)
		}
		sort.Strings(nodes)
		return diskFaultConstraints{}, cpierrors.Cloud(
			"create_vm: persistent disks are pinned to different local nodes %v — "+
				"local-storage disks cannot span nodes; ensure all persistent disks "+
				"reside on the same PVE node or use shared storage",
			nodes,
		)
	}
	if len(localNodes) == 1 {
		for n := range localNodes {
			c.requiredLocalNode = n
		}
	}

	return c, nil
}

// resolveAZCandidatesValidated looks up the node list for az in the AZ map.
// Called only after pre-validation confirmed all AZ names are known; unknown
// names are not expected here. Returns (nil, true) for the DLB sentinel AZ.
// Returns (nodes, false) for a valid AZ.
func resolveAZCandidatesValidated(az string, cfg *config.CPIConfig, logger *log.Logger) (candidates []string, skipSilently bool) {
	nodes, ok := cfg.AZCandidates(az)
	if ok {
		logger.Debug("create_vm: node selection: AZ candidate set",
			log.String("az", az),
			log.String("candidates", strings.Join(nodes, ",")),
		)
		return nodes, false
	}
	// DLB sentinel: skip scoring for this AZ.
	if az == cfg.DLBAZName() && cfg.DLBAZName() != "" {
		logger.Debug("create_vm: node selection: DLB sentinel AZ — candidates = all online nodes",
			log.String("az", az),
		)
		return nil, true
	}
	// Should not reach here after pre-validation; treat as skip.
	return nil, true
}

// scoreAndPickWithRanked scores the passed nodes, picks the best, and returns
// the full ranked list alongside the winner. The ranked list is in descending
// score order (winner first) and contains the node names in that order.
// Returns ("", nil) when pass is empty.
func scoreAndPickWithRanked(pass []placement.NodeFacts, weights placement.Weights, logger *log.Logger, az string) (winner string, rankedNodes []string) {
	if len(pass) == 0 {
		return "", nil
	}
	scored := placement.Score(pass, weights, nil)
	chosen := placement.Pick(scored, nil)
	if chosen == "" {
		return "", nil
	}
	logger.Info("create_vm: node selection: placement scoring chose node",
		log.String("node", chosen),
		log.String("az", az),
	)
	nodes := make([]string, len(scored))
	for i, sn := range scored {
		nodes[i] = sn.Node
	}
	return chosen, nodes
}

// logFilterRejections emits a Debug entry for each rejection.
func logFilterRejections(logger *log.Logger, rejections map[string]string, az string) {
	for n, reason := range rejections {
		logger.Debug("create_vm: placement: node filtered",
			log.String("node", n),
			log.String("reason", reason),
			log.String("az", az),
		)
	}
}

// mergeRejections merges src into dst, keeping existing entries (first rejection wins).
func mergeRejections(dst, src map[string]string) {
	for k, v := range src {
		if _, exists := dst[k]; !exists {
			dst[k] = v
		}
	}
}

// classifyFilterResult returns true when all rejection reasons are transient
// (node may come back without operator intervention). Returns true on empty
// rejections (cluster temporarily unreachable → retriable).
// Returns false when any rejection is a permanent misconfiguration.
func classifyFilterResult(rejections map[string]string) (retriable bool) {
	if len(rejections) == 0 {
		return true // no facts = cluster may be temporarily unreachable
	}
	for _, reason := range rejections {
		if !isTransientRejectionReason(reason) {
			return false
		}
	}
	return true
}

// isTransientRejectionReason reports whether a single rejection reason string
// corresponds to a transient condition that may clear without operator action.
//
// "not in candidate node set" is a scope constraint, not a node-health signal.
// It is neutral — it does not indicate a permanent misconfiguration. Returning
// true here ensures it never prevents retriability when other nodes are offline
// (a transient cause). A pure "all nodes outside candidate set" result is still
// retriable because the cluster topology may change (node added to AZ, config
// reload).
func isTransientRejectionReason(reason string) bool {
	switch reason {
	case "node offline", "node in maintenance", "insufficient CPU", "insufficient free memory",
		"not in candidate node set", "storage utilization ceiling exceeded":
		// "insufficient free storage" is intentionally absent: a storage-capacity
		// shortfall is a hard permanent constraint (the node does not have enough
		// free space for the VM's disks plus headroom) and will not clear without
		// operator action (freeing space or moving data). Map it to non-retriable
		// so the Director surfaces the error immediately instead of retrying
		// indefinitely against the same over-committed node.
		//
		// "storage utilization ceiling exceeded" is deliberately the OPPOSITE
		// classification: the ceiling gate (storage.max_utilization_pct) is a
		// proportional early-warning band, not a hard-out-of-space condition —
		// capacity can plausibly free up (a neighboring delete, a completed
		// migration) well before the pool is actually full. Enforce-mode
		// violations must be RETRIABLE so the director re-drives rather than
		// treating a recoverable capacity pressure signal as a permanent
		// failure.
		return true
	}
	// A failed ListHardwarePci call (pvedaemon restart, momentary node
	// unreachability) is transient: the device may well be present, the check
	// just could not run. "missing required PCI device" stays permanent — the
	// node answered and the device is absent.
	if strings.HasPrefix(reason, "PCI device check error: ") {
		return true
	}
	return false
}

// formatRejections returns a compact human-readable summary of a rejection map.
func formatRejections(rejections map[string]string) string {
	if len(rejections) == 0 {
		return "(no candidates available)"
	}
	parts := make([]string, 0, len(rejections))
	for node, reason := range rejections {
		parts = append(parts, node+": "+reason)
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

// computeRequiredStorageBytes returns the minimum free-storage floor in bytes
// for placement.Request.RequiredStorageBytes when the storage-headroom gate is
// enabled. Returns 0 if disabled (gate-off callers must not call this).
//
// Formula (all quantities in bytes):
//
//	floor = rootDiskBytes + ephemeralDiskBytes + headroomBytes
//
// where:
//   - rootDiskBytes     = rootDiskGiB (GiB→bytes); rootDiskGiB is the effective
//     root disk size derived from cloud_properties exactly as resolveVMShapeStorage
//     does it — max(defaultStemcellDiskGiB, ceil(requestedMiB/1024)).
//   - ephemeralDiskBytes = ephemeral GiB×GiB→bytes, included ONLY when the
//     ephemeral disk resolves to the same storage pool as vmStorage (i.e.
//     cp.EphemeralStoragePool is "" or equals storageName). If the ephemeral disk
//     is on a different pool, those bytes are excluded so the filter is not applied
//     to a pool whose facts were not gathered.
//   - headroomBytes = cfg.StorageHeadroomMBValue() (MiB→bytes) +
//     memSwapBytes, where memSwapBytes = memMiB×MiB→bytes when a dedicated
//     ephemeral disk is present (mirroring vSphere's max-swapfile term: ESXi
//     reserves VM-RAM bytes for the host swap file; PVE/QEMU has no host
//     swapfile, so we include it only when the ephemeral disk is present as
//     a worst-case in-guest swap reservation that competes for the same pool).
//     memSwapBytes is 0 when no dedicated ephemeral disk is requested.
//
// storageName is the pool the placement facts measured (always deps.Config.VMStorage).
func computeRequiredStorageBytes(cfg *config.CPIConfig, cp createVMCloudProps, storageName string) int64 {
	const mibBytes = int64(1024 * 1024)

	footprintBytes := computeDiskFootprintBytes(cp, storageName)
	ephemeralOnVMStorage := cp.EphemeralStoragePool == "" || cp.EphemeralStoragePool == storageName

	// Headroom: configured margin + VM RAM as in-guest swap reservation.
	// The swap term mirrors vSphere's DISK_HEADROOM logic (max swapfile ≈ VM RAM).
	// Include only when a dedicated ephemeral disk is present (the swap file
	// resides on the ephemeral disk; without a dedicated ephemeral disk the
	// agent carves swap from the root disk, which is already counted above).
	headroomMiB := int64(cfg.StorageHeadroomMBValue())
	headroomBytes := headroomMiB * mibBytes

	if cp.EphemeralDiskSizeMB > 0 && ephemeralOnVMStorage {
		// Include VM RAM as worst-case swap reservation (vSphere max-swapfile term).
		memMiB := int64(cp.Memory)
		if cp.RAM > 0 {
			memMiB = int64(cp.RAM)
		}
		if memMiB < 0 {
			memMiB = 0
		}
		headroomBytes += memMiB * mibBytes
	}

	return footprintBytes + headroomBytes
}

// computeDiskFootprintBytes returns the raw disk footprint in bytes — root
// disk plus ephemeral disk (when it lands on storageName) — with NO headroom
// margin. This is the quantity storage.max_utilization_pct evaluates the pool
// against; computeRequiredStorageBytes adds a headroom margin on top of this
// same footprint for the separate placement.reserve_storage_headroom gate.
//
//   - rootDiskBytes = rootDiskGiB (GiB→bytes); rootDiskGiB is the effective
//     root disk size derived from cloud_properties exactly as
//     resolveVMShapeStorage does it — max(defaultStemcellDiskGiB,
//     ceil(requestedMiB/1024)).
//   - ephemeralDiskBytes = ephemeral GiB×GiB→bytes, included ONLY when the
//     ephemeral disk resolves to the same storage pool as storageName (i.e.
//     cp.EphemeralStoragePool is "" or equals storageName). If the ephemeral
//     disk is on a different pool, those bytes are excluded so the gate is
//     not applied to a pool whose facts were not gathered.
func computeDiskFootprintBytes(cp createVMCloudProps, storageName string) int64 {
	const gibBytes = int64(1024 * 1024 * 1024)

	// Effective root disk size: mirrors resolveVMShapeStorage logic.
	rootDiskGiB := int64(defaultStemcellDiskGiB)
	requestedMiB := 0
	if cp.RootDiskSize > 0 {
		requestedMiB = cp.RootDiskSize
	} else if cp.Disk > 0 {
		requestedMiB = cp.Disk
	}
	if requestedMiB > 0 {
		gib := int64((requestedMiB + 1023) / 1024)
		if gib > rootDiskGiB {
			rootDiskGiB = gib
		}
	}
	rootDiskBytes := rootDiskGiB * gibBytes

	// Ephemeral disk size: only counted when it lands on the same storage pool.
	// cp.EphemeralStoragePool=="" means the ephemeral disk defaults to vmStorage.
	// When it is explicitly set to a different pool, the facts do not cover that
	// pool and we exclude those bytes to avoid incorrect rejections.
	ephemeralGiB := int64(0)
	ephemeralOnVMStorage := cp.EphemeralStoragePool == "" || cp.EphemeralStoragePool == storageName
	if cp.EphemeralDiskSizeMB > 0 && ephemeralOnVMStorage {
		ephemeralGiB = int64((cp.EphemeralDiskSizeMB + 1023) / 1024)
	}
	ephemeralDiskBytes := ephemeralGiB * gibBytes

	return rootDiskBytes + ephemeralDiskBytes
}

// utilizationGateForRequest returns the placement.Request fields that enforce
// storage.max_utilization_pct, plus a closure to run after the winning node
// is chosen.
//
// In enforce mode (the default) the returned ceilingPct/addBytes populate
// placement.Request.MaxUtilizationPct/PlannedAddBytes, so Filter hard-rejects
// any candidate node that would breach the ceiling; the returned closure is
// then a no-op, since a violation on the chosen node is impossible to observe
// (Filter already excluded it).
//
// In warn mode the hard-filter fields are left zero — scoring and candidate
// selection are byte-identical to the gate being disabled — and the returned
// closure instead checks the ultimately chosen node against the same ceiling
// and logs a Warn if it would be breached, without blocking placement.
//
// Disabled (storage.max_utilization_pct unset or 0) returns zero fields and a
// no-op closure — zero added cost, zero behavior change.
func utilizationGateForRequest(
	cfg *config.CPIConfig, cp createVMCloudProps, storageName string, facts []placement.NodeFacts, logger *log.Logger,
) (ceilingPct int, addBytes int64, warnChosen func(node string)) {
	noop := func(string) {}
	ceiling := cfg.MaxUtilizationPctValue()
	if ceiling <= 0 {
		return 0, 0, noop
	}
	footprint := computeDiskFootprintBytes(cp, storageName)
	if cfg.MaxUtilizationEnforce() {
		return ceiling, footprint, noop
	}
	return 0, 0, func(node string) {
		warnIfNodeUtilizationExceeds(facts, node, ceiling, footprint, logger)
	}
}

// warnIfNodeUtilizationExceeds implements the warn-mode half of the
// create_vm storage.max_utilization_pct gate: it looks up node's already-
// gathered storage facts and logs a Warn if placing addBytes there would push
// projected utilization past ceilingPct. A no-op when node's facts are
// missing or report no storage capacity (fail-open, consistent with the
// enforce-mode Filter path's fail-open on TotalStorageBytes == 0).
func warnIfNodeUtilizationExceeds(facts []placement.NodeFacts, node string, ceilingPct int, addBytes int64, logger *log.Logger) {
	for _, f := range facts {
		if f.Node != node {
			continue
		}
		if f.TotalStorageBytes <= 0 {
			return
		}
		used := f.TotalStorageBytes - f.FreeStorageBytes
		if used < 0 {
			used = 0
		}
		pct := float64(used+addBytes) / float64(f.TotalStorageBytes) * 100
		if pct > float64(ceilingPct) {
			logger.Warn("create_vm: chosen node's storage pool projected utilization exceeds ceiling (warn mode; proceeding)",
				log.String("node", node),
				log.Float64("projected_pct", pct),
				log.Int("ceiling_pct", ceilingPct),
			)
		}
		return
	}
}
