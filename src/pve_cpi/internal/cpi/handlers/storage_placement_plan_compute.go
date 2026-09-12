package handlers

import (
	"context"
	"encoding/json"
	"math"
	"math/rand"
	"slices"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/placement"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// ObserveStoragePlanCompute preserves existing AZ and compute scoring while
// excluding the scalar storage axis, which the frozen inventory now owns.
// Explicit/static pins still pass live memory, PCI, maintenance and network gates.
func ObserveStoragePlanCompute(ctx context.Context, deps Deps, parsed *createVMParsedArgs, rng *rand.Rand) ([]StoragePlanNodeGroup, error) {
	if deps.Config == nil || deps.PVE == nil || parsed == nil {
		return nil, planError(StoragePlanConfiguration, "compute observation dependencies are required")
	}
	cfg := deps.Config
	cp := parsed.cloudProps
	resolver, err := newLayeredResolver(parsed.cloudPropsMap, cfg)
	if err != nil {
		return nil, err
	}
	constraints, err := deriveDiskFaultConstraints(ctx, deps, parsed.diskCIDs)
	if err != nil {
		return nil, err
	}
	azs := buildAZOrder(cp, cfg, rng, resolver)
	if len(constraints.requiredAZs) > 0 {
		azs, err = applyDiskAZConstraint(azs, constraints.requiredAZs)
		if err != nil {
			return nil, err
		}
	}
	if len(azs) == 0 {
		azs = []string{""}
	}
	pin := cp.TargetNode
	if pin == "" && !cfg.PlacementEnabled() {
		pin = cfg.Node
	}
	if constraints.requiredLocalNode != "" {
		if pin != "" && pin != constraints.requiredLocalNode {
			return nil, planError(StoragePlanConfiguration, "node pin conflicts with persistent disk locality")
		}
		pin = constraints.requiredLocalNode
	}
	groupTag := antiAffinityGroupTag(cfg, parsed.env)
	facts, err := placement.GatherNodeFacts(ctx, deps.PVE.Cluster(), deps.PVE.Nodes(), deps.Log(ctx), placement.GatherOptions{StorageName: "", GroupTag: groupTag, ExcludeMaintenanceNodes: cfg.ExcludeMaintenanceNodesEnabled(), MaintenanceNodeTags: cfg.MaintenanceNodeTagsValue()})
	if err != nil {
		return nil, planError(StoragePlanObservation, "compute facts: %v", err)
	}
	w := cfg.EffectiveWeights()
	weights := placement.Weights{Mem: w.Mem, CPU: w.CPU, GuestCount: w.GuestCount, MemorySignal: cfg.MemorySignalValue()}
	for _, override := range []struct {
		key   string
		value *float64
	}{{"placement_weight_mem", &weights.Mem}, {"placement_weight_cpu", &weights.CPU}, {"placement_weight_guest_count", &weights.GuestCount}} {
		if v, ok := resolver.Float(override.key); ok {
			*override.value = v
		}
	}
	if groupTag != "" {
		weights.AntiAffinity = placement.DefaultWeights().AntiAffinity
	}
	if err := validatePCIPassthroughs(cp.PCIPassthroughs); err != nil {
		return nil, err
	}
	var addresses []string
	for _, pt := range cp.PCIPassthroughs {
		addresses = append(addresses, pt.Address)
	}
	var checker func(string) (bool, error)
	if len(addresses) > 0 {
		checker = buildPCIChecker(ctx, deps.PVE.Nodes(), addresses)
	}
	cores, sockets, mem := resolveVMShapeCPUMem(cp)
	if cores <= 0 || sockets <= 0 || mem <= 0 || int64(cores) > math.MaxInt64/int64(sockets) || int64(mem) > math.MaxInt64/(1<<20) {
		return nil, planError(StoragePlanConfiguration, "invalid or overflowing compute shape")
	}
	bridge, _, err := resolveVMNICDefaultsWithError(cfg, cp, parsed.cloudPropsMap)
	if err != nil {
		return nil, err
	}
	defaults := cp.NetworkDefaults
	var bridges []string
	for name := range parsed.networks {
		if parsed.networks[name].Type == "vip" {
			continue
		}
		resolved, _, err := resolveNICBridgeAndVLAN(defaults, parsed.networks[name].CloudProperties, bridge, name)
		if err != nil {
			return nil, err
		}
		bridges = append(bridges, resolved)
	}
	slices.Sort(bridges)
	bridges = slices.Compact(bridges)
	networkEligible := map[string]bool{}
	for _, fact := range facts {
		if !fact.Online {
			continue
		}
		ok, err := storagePlanNodeBridges(ctx, deps, fact.Node, bridges)
		if err != nil {
			continue
		}
		networkEligible[fact.Node] = ok
	}
	groups, err := rankStorageComputeGroups(ctx, deps, parsed, azs, pin, facts, weights, addresses, checker, cores, sockets, mem, networkEligible)
	if err != nil {
		return nil, err
	}

	if len(groups) == 0 {
		return nil, planError(StoragePlanCapacity, "no node satisfies compute, AZ, network, PCI and disk constraints")
	}
	return groups, nil
}
func storagePlanNodeBridges(ctx context.Context, deps Deps, node string, required []string) (bool, error) {
	if len(required) == 0 {
		return true, nil
	}
	typ := "any_bridge"
	response, err := deps.PVE.Nodes().ListNetwork(ctx, node, &sdknodes.ListNetworkParams{Type: &typ})
	if err != nil || response == nil {
		response, err = deps.PVE.Nodes().ListNetwork(ctx, node, nil)
	}
	if err != nil || response == nil {
		return false, planError(StoragePlanObservation, "network observation failed on %s", node)
	}
	names := map[string]bool{}
	for _, raw := range *response {
		var row struct {
			Iface string `json:"iface"`
		}
		if err = json.Unmarshal(raw, &row); err != nil || row.Iface == "" {
			return false, planError(StoragePlanObservation, "malformed network observation on %s", node)
		}
		names[row.Iface] = true
	}
	for _, bridge := range required {
		if !names[bridge] {
			return false, nil
		}
	}
	return true, nil
}

func rankStorageComputeGroups(ctx context.Context, deps Deps, parsed *createVMParsedArgs, azs []string, pin string, facts []placement.NodeFacts, weights placement.Weights, addresses []string, checker func(string) (bool, error), cores, sockets, mem int, networkEligible map[string]bool) ([]StoragePlanNodeGroup, error) {
	cfg := deps.Config
	var err error
	var groups []StoragePlanNodeGroup
	for _, az := range azs {
		if err = ctx.Err(); err != nil {
			return nil, planError(StoragePlanObservation, "%v", err)
		}
		var candidates []string
		if az != "" {
			var known bool
			_, known = cfg.AZCandidates(az)
			if !known && az != cfg.DLBAZName() {
				return nil, planError(StoragePlanConfiguration, "unknown AZ %q", az)
			}
			var skip bool
			candidates, skip = resolveAZCandidatesValidated(az, cfg, deps.Log(ctx))
			if skip {
				continue
			}
			if len(candidates) == 0 {
				continue
			}
		}
		if pin != "" {
			if len(candidates) > 0 && !slices.Contains(candidates, pin) {
				continue
			}
			candidates = []string{pin}
		}
		pass, _ := placement.Filter(facts, placement.Request{CandidateNodes: candidates, RequiredCPU: int64(cores) * int64(sockets), RequiredMemBytes: int64(mem) * (1 << 20), ExcludeMaintenanceNodes: cfg.ExcludeMaintenanceNodesEnabled(), RequiredPCIAddresses: addresses, PCIChecker: checker})
		eligible := pass[:0]
		for _, fact := range pass {
			if networkEligible[fact.Node] {
				eligible = append(eligible, fact)
			}
		}
		_, nodes := scoreAndPickWithRanked(eligible, weights, deps.Log(ctx), az)
		if len(nodes) > 0 {
			haNodes := storageComputeHANodes(deps, parsed, az, nodes, facts)
			groups = append(groups, StoragePlanNodeGroup{AZ: az, Nodes: nodes, HANodes: haNodes})
		}
	}
	return groups, nil
}

func storageComputeHANodes(deps Deps, parsed *createVMParsedArgs, az string, nodes []string, facts []placement.NodeFacts) []string {
	cfg, cp := deps.Config, parsed.cloudProps
	haNodes := []string{}
	haCloudProps := cp
	haCloudProps.AvailabilityZone = az
	for _, node := range nodes {
		if len(haRegistrationFeatures(deps, haCloudProps, node, parsed.env)) > 0 {
			for _, fact := range facts {
				haNodes = append(haNodes, fact.Node)
			}
			break
		}
	}
	if len(haNodes) > 0 && cfg.HANodeAffinityPinEnabled() && cfg.PinAZStrict() {
		if permitted, ok := cfg.AZCandidates(az); ok && len(permitted) > 0 {
			haNodes = permitted
		}
	}
	slices.Sort(haNodes)
	haNodes = slices.Compact(haNodes)
	return haNodes
}
