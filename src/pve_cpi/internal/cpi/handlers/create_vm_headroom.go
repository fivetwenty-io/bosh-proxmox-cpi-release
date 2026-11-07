package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/placement"
)

// headroomDecisionVMID stands in for the VM's ID while the eligibility decision
// is made before placement. The decision never reads it beyond the checks that
// it is positive, and create_vm allocates the real ID only after it has a node.
const headroomDecisionVMID = 1

// haResourceStateIgnored is the requested state of an HA resource the CRM no
// longer manages. Such a guest is not recovered when its node fails.
const haResourceStateIgnored = "ignored"

// headroomPlan carries the N+1 memory check of one create_vm placement. A nil
// plan means the check does not run for this call, which is what pve.ha.mode
// off, pve.ha.headroom off, a cluster where the CPI may not write HA state, and
// a VM that no node would register all give. Every method is safe on nil.
type headroomPlan struct {
	check *placement.HeadroomCheck
}

// buildHeadroomPlan reads what the estimate needs and returns the plan, or nil
// when the check does not run. With pve.ha.mode off or pve.ha.headroom off it
// returns before any read, so Request.Headroom stays nil and the filter behaves
// as it did before the key existed.
//
// The estimate counts the VM as an HA guest because the check exists for HA VMs.
// A VM that no online node would register for HA, because the cluster may not
// write HA state or the eligibility decision skips or fails it, is not recovered
// when its node fails, so it needs no room and no check runs. The decision is
// made once for each online node, from the VM's root disk storage, its strict
// availability zone, its CPU type, its PCI devices, and the plain bridges it
// needs. The VM's hard admissible set for a planned node is the decision's
// admissible set for that node. A hold VM is never recovered on another node, so
// it counts as no HA VM.
//
// A read that fails does not fail the call. It marks the guest list unknown, so
// the estimate is unavailable, which enforce refuses as a transient condition
// and warn only logs.
func buildHeadroomPlan(
	ctx context.Context, deps Deps, cp createVMCloudProps, resolver *layeredResolver, bridges bridgePlan,
	facts []placement.NodeFacts, logger *log.Logger,
) *headroomPlan {
	if deps.Config == nil || deps.PVE == nil || deps.Config.HAModeValue() == config.HAModeOff {
		return nil
	}
	mode := deps.Config.HAHeadroomValue()
	if mode == config.HAHeadroomOff {
		return nil
	}
	api := deps.PVE.Cluster()
	if api == nil {
		return nil
	}
	profile, err := resolveHAProfile(ctx, deps)
	if err != nil {
		logger.Warn("create_vm: placement: headroom check skipped, because the HA profile could not be resolved",
			log.Err(err))
		return nil
	}
	if !profile.WritesAllowed() {
		logger.Debug("create_vm: placement: headroom check skipped, because the CPI may not write HA state here",
			log.String("reason", profile.Reason))
		return nil
	}

	nodes := factNodeNames(facts)
	admissible, recoverable := headroomAdmissibleSets(deps, cp, resolver, bridges, profile, facts)
	if !recoverable {
		logger.Debug("create_vm: placement: headroom check skipped, because no node would register the VM for HA")
		return nil
	}

	_, _, memMiB := resolveVMShapeCPUMem(cp)
	check := &placement.HeadroomCheck{
		Mode: mode,
		VM: placement.HeadroomVM{
			MemBytes: int64(memMiB) * 1024 * 1024,
			AdmissibleFor: func(node string) []string {
				if set, ok := admissible[node]; ok {
					return set
				}
				return nodes
			},
		},
	}
	guests, err := readHeadroomGuests(ctx, api, nodes)
	if err != nil {
		logger.Warn("create_vm: placement: the cluster's guests could not be read for the headroom estimate",
			log.Err(err))
		check.GuestsUnknown = true
	}
	check.Guests = guests
	return &headroomPlan{check: check}
}

// apply sets the headroom check on req. In warn mode the filter keeps every
// node, so the check only decides what warn logs for the chosen node.
func (h *headroomPlan) apply(req *placement.Request) {
	if h == nil {
		return
	}
	req.Headroom = h.check
}

// enforced reports whether a short estimate removes a node from placement.
func (h *headroomPlan) enforced() bool {
	return h != nil && h.check.Mode == config.HAHeadroomEnforce
}

// warnChosen logs the estimate for the node placement chose when the mode is
// warn. The filter keeps every node in that mode, so the warning is the whole
// effect. It places the VM anyway.
func (h *headroomPlan) warnChosen(logger *log.Logger, facts []placement.NodeFacts, node string) {
	if h == nil || h.check.Mode != config.HAHeadroomWarn {
		return
	}
	if err := placement.EnforceHeadroom(logger, facts, h.check, node); err != nil {
		logger.Warn("create_vm: placement: headroom estimate", log.Err(err))
	}
}

// headroomRejected reports whether any rejection reason comes from the headroom
// check, which is a shortfall or an estimate that could not run.
func headroomRejected(rejections map[string]string) bool {
	for _, reason := range rejections {
		if isHeadroomRejectionReason(reason) {
			return true
		}
	}
	return false
}

// isHeadroomRejectionReason reports whether reason starts with a prefix the
// headroom filter records.
func isHeadroomRejectionReason(reason string) bool {
	return strings.HasPrefix(reason, placement.HeadroomShortPrefix) ||
		strings.HasPrefix(reason, placement.HeadroomUnavailablePrefix)
}

// factNodeNames returns the names of every node the facts list, in fact order.
func factNodeNames(facts []placement.NodeFacts) []string {
	out := make([]string, 0, len(facts))
	for i := range facts {
		out = append(out, facts[i].Node)
	}
	return out
}

// headroomAdmissibleSets runs the eligibility decision for the VM on each
// online node and returns the hard admissible set of every node whose decision
// registers the VM for recovery elsewhere. The bool is false when no node does.
func headroomAdmissibleSets(
	deps Deps, cp createVMCloudProps, resolver *layeredResolver, bridges bridgePlan,
	profile *ha.Profile, facts []placement.NodeFacts,
) (map[string][]string, bool) {
	cfg := deps.Config
	storage := cfg.VMStorage
	if resolver != nil {
		if pool, ok := resolver.String("storage_pool"); ok {
			storage = pool
		}
	}
	var perVMCPU *string
	if resolver != nil {
		cpu := resolveVMShapeCPUType(resolver, cfg)
		perVMCPU = &cpu
	}
	base := ha.VMInput{
		Config:  cfg,
		VMID:    headroomDecisionVMID,
		CPUType: ha.EffectiveCPUType(cfg, perVMCPU, ""),
		InDLBAZ: vmInDLBAZ(cp, cfg),
	}
	bridgeConstraints := headroomBridgeConstraints(bridges, profileNodeNames(profile))

	out := make(map[string][]string, len(facts))
	recoverable := false
	for i := range facts {
		f := facts[i]
		if !f.Online || storage == "" {
			continue
		}
		in := base
		in.Node = f.Node
		in.Volumes = []ha.VMVolume{{Volume: storage + ":vm-" + fmt.Sprint(headroomDecisionVMID) + "-disk-0"}}
		in.Constraints = append(in.Constraints, bridgeConstraints...)
		if len(cp.PCIPassthroughs) > 0 {
			in.Constraints = append(in.Constraints, ha.NodeConstraint{Name: pciConstraintName, Nodes: []string{f.Node}})
		}
		if az := pinAZForNode(cp, cfg, f.Node); az != "" {
			if azNodes, ok := cfg.AZCandidates(az); ok {
				in.AZ, in.AZNodes = az, azNodes
			}
		}
		decision, err := profile.DecideVM(in)
		if err != nil || !decision.Registers() || decision.Policy == ha.PolicyHold {
			continue
		}
		recoverable = true
		set := slices.Clone(decision.Admissible)
		for _, n := range decision.Widened {
			if !slices.Contains(set, n) {
				set = append(set, n)
			}
		}
		out[f.Node] = set
	}
	return out, recoverable
}

// headroomBridgeConstraints returns one constraint for each plain bridge the
// VM's NICs need, naming the members that hold it. A member whose bridge list
// could not be read does not count as holding it. With no required bridge the
// result is nil.
func headroomBridgeConstraints(bridges bridgePlan, members []string) []ha.NodeConstraint {
	if len(bridges.required) == 0 || bridges.checker == nil || len(members) == 0 {
		return nil
	}
	out := make([]ha.NodeConstraint, 0, len(bridges.required))
	for _, bridge := range bridges.required {
		var holders []string
		for _, n := range members {
			if present, err := bridges.checker(n, bridge); err == nil && present {
				holders = append(holders, n)
			}
		}
		out = append(out, ha.NodeConstraint{Name: "bridge " + bridge, Nodes: holders})
	}
	return out
}

// headroomResourceRow is the part of a cluster resource row the estimate reads.
type headroomResourceRow struct {
	Type     string            `json:"type"`
	VMID     sdkclient.PVEInt  `json:"vmid"`
	Node     string            `json:"node"`
	Maxmem   sdkclient.PVEInt  `json:"maxmem"`
	Template sdkclient.PVEBool `json:"template"`
}

// haSID returns the HA service ID of a guest row, or "" for a type
// that has none.
func (r headroomResourceRow) haSID() string {
	switch r.Type {
	case "qemu":
		return fmt.Sprintf("vm:%d", r.VMID.Int())
	case "lxc":
		return fmt.Sprintf("ct:%d", r.VMID.Int())
	}
	return ""
}

// readHeadroomGuests lists every guest of the cluster with its node, its
// configured memory, whether the CRM would recover it, and the nodes it may be
// recovered onto. The guests come from the cluster resources, the recovery flag
// from the HA resources, and the admissible set from the node-affinity rules.
// It returns an error when any of the three cannot be read or parsed, because
// an estimate over part of the cluster would be too optimistic. A template
// never runs, so it is not a guest.
func readHeadroomGuests(
	ctx context.Context, api sdkcluster.Service, nodes []string,
) ([]placement.HeadroomGuest, error) {
	guestType := "vm"
	resources, err := api.ListResources(ctx, &sdkcluster.ListResourcesParams{Type: &guestType})
	if err != nil {
		return nil, fmt.Errorf("list the cluster resources: %w", err)
	}
	if resources == nil {
		return nil, errors.New("list the cluster resources: no listing returned")
	}
	haResources, err := api.ListHaResources(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("list the HA resources: %w", err)
	}
	if haResources == nil {
		return nil, errors.New("list the HA resources: no listing returned")
	}
	haRules, err := api.ListHaRules(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("list the HA rules: %w", err)
	}
	rules, err := ha.ParseRuleList(haRules)
	if err != nil {
		return nil, err
	}
	managed, err := recoveredServiceIDs(haResources)
	if err != nil {
		return nil, err
	}

	var guests []placement.HeadroomGuest
	for i, raw := range *resources {
		var row headroomResourceRow
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, fmt.Errorf("cluster resource row %d does not parse: %w", i, err)
		}
		if row.Type != "qemu" && row.Type != "lxc" {
			continue
		}
		if row.Template.Bool() {
			continue
		}
		if row.Node == "" || row.VMID.Int() <= 0 {
			return nil, fmt.Errorf("cluster resource row %d names no node or no VM ID", i)
		}
		guest := placement.HeadroomGuest{VMID: int(row.VMID.Int()), Node: row.Node, MemBytes: row.Maxmem.Int()}
		sid := row.haSID()
		if managed[sid] {
			guest.Admissible = recoveryNodes(rules, sid, nodes)
			// A strict pin to the guest's own node is a hold: the guest waits for
			// its node and is never recovered elsewhere.
			guest.HA = len(guest.Admissible) != 1 || guest.Admissible[0] != row.Node
		}
		guests = append(guests, guest)
	}
	return guests, nil
}

// recoveredServiceIDs returns the service IDs of the HA resources the CRM
// recovers. A resource in the ignored state is outside the CRM's management.
func recoveredServiceIDs(resp *sdkcluster.ListHaResourcesResponse) (map[string]bool, error) {
	out := make(map[string]bool, len(*resp))
	for i, raw := range *resp {
		var row struct {
			SID   string `json:"sid"`
			State string `json:"state"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, fmt.Errorf("HA resource row %d does not parse: %w", i, err)
		}
		if strings.TrimSpace(row.SID) == "" {
			return nil, fmt.Errorf("HA resource row %d names no service ID", i)
		}
		if strings.EqualFold(strings.TrimSpace(row.State), haResourceStateIgnored) {
			continue
		}
		out[row.SID] = true
	}
	return out, nil
}

// recoveryNodes returns the nodes the CRM may recover sid onto: every node,
// narrowed by each enabled strict node-affinity rule that names the resource. A
// positive strict rule keeps its own nodes and a negative one removes them. A
// non-strict rule only sets priorities, so it narrows nothing. The result can be
// empty, which means the resource may run nowhere.
func recoveryNodes(rules *ha.RuleSet, sid string, all []string) []string {
	set := slices.Clone(all)
	for i := range rules.Rules {
		r := &rules.Rules[i]
		if r.Type != ha.RuleTypeNodeAffinity || r.Disabled || !r.Strict || !r.HasResource(sid) {
			continue
		}
		named := r.NodeNames()
		set = slices.DeleteFunc(set, func(n string) bool {
			if r.Affinity == ha.AffinityNegative {
				return slices.Contains(named, n)
			}
			return !slices.Contains(named, n)
		})
	}
	return set
}

// noViableCandidatesError is the error of a placement in which every node was
// rejected. A rejection set whose reasons may all clear is retriable.
func noViableCandidatesError(rejections map[string]string, retriable bool) error {
	if retriable {
		return cpierrors.Retriable("create_vm: no viable placement candidates (transient); all nodes rejected: %s",
			formatRejections(rejections))
	}
	return cpierrors.Cloud("create_vm: no viable placement candidates; all nodes rejected: %s",
		formatRejections(rejections))
}
