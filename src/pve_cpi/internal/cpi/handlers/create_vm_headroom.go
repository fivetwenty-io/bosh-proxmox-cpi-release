package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

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

// headroomNewVMLabel names the VM being placed in the lines the estimate
// writes. The VM has no ID yet, and the estimate prints a zero ID as "VM 0".
const headroomNewVMLabel = "the new VM"

// headroomPlan carries the N+1 memory check of one create_vm placement. A nil
// plan means the check does not run for this call, which is what pve.ha.mode
// off, pve.ha.headroom off, a request that carries no director_uuid, a cluster
// where the CPI may not write HA state, a VM whose pools are not known before
// placement, and a VM that no node would register all give. Every method is
// safe on nil.
//
// The plan holds the rule that enforce never refuses a VM the real HA decision
// would not register on the node being checked. exempt lists the nodes where
// the prediction says the VM would not register, and neither the filter nor the
// warning counts the VM as an HA guest there.
type headroomPlan struct {
	check  *placement.HeadroomCheck
	exempt map[string]bool

	// read lists the cluster's guests. It runs at most once, when the first
	// estimate needs the guests, so warn mode reads only after placement has
	// chosen a node and a node the VM would not be HA on reads nothing.
	read func() ([]placement.HeadroomGuest, error)
	once sync.Once
	log  *log.Logger
}

// buildHeadroomPlan decides whether the check runs and returns the plan, or nil
// when it does not. With pve.ha.mode off or pve.ha.headroom off it returns
// before any read, so Request.Headroom stays nil and the filter behaves as it
// did before the key existed. The cluster's guests are not read here. The plan
// reads them when the first estimate needs them.
//
// The estimate counts the VM as an HA guest because the check exists for HA VMs.
// A VM that the real HA decision would not register is not recovered when its
// node fails, so it needs no room. The prediction therefore follows that
// decision. A request with no director_uuid gets no HA, as it does in
// decideCreatedVMHA. The decision is made for each online node over every
// volume the VM's config will reference, which are its root disk, its ephemeral
// disk, its config-drive ISO, and its persistent disks, with the same
// availability zone, CPU type, PCI, and bridge inputs the real decision reads.
// A linked clone is decided only when the VM clones its root disk, which is
// known after placement, so the VM counts as HA on a node only if it registers
// both ways. A node where it does not register is exempt from the check, and a
// hold VM is never recovered on another node, so it is exempt too.
//
// Where an input is not known before placement, because a storage tier, a
// storage selector, or a storage plan picks a pool, or because a persistent
// disk's CID names no pool, the check does not run and the reason is logged at
// Debug. That is the reading that never refuses.
//
// A read of the guests that fails does not fail the call. It marks the guest
// list unknown, so the estimate is unavailable, which enforce refuses as a
// transient condition and warn only logs.
func buildHeadroomPlan(
	ctx context.Context, deps Deps, cp createVMCloudProps, cloudPropsMap map[string]any, resolver *layeredResolver,
	diskCIDs []string, bridges bridgePlan, facts []placement.NodeFacts, logger *log.Logger,
) *headroomPlan {
	if deps.Config == nil || deps.PVE == nil || deps.Config.HAModeValue() == config.HAModeOff {
		return nil
	}
	mode := deps.Config.HAHeadroomValue()
	if mode == config.HAHeadroomOff {
		return nil
	}
	// decideCreatedVMHA gives a request with no director_uuid no HA, because the
	// CPI could not mark the HA resource as its own, so the VM needs no room.
	if deps.RequestDirectorUUID == "" {
		logger.Debug("create_vm: placement: headroom check skipped, because the request carries no director_uuid " +
			"and the VM gets no HA")
		return nil
	}
	api := deps.PVE.Cluster()
	if api == nil {
		return nil
	}
	volumes, unknown := headroomVolumes(ctx, deps, cp, cloudPropsMap, resolver, diskCIDs)
	if unknown != "" {
		logger.Debug("create_vm: placement: headroom check skipped, because " + unknown)
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
	admissible, recoverable := headroomAdmissibleSets(deps, cp, resolver, bridges, profile, facts, volumes)
	if !recoverable {
		logger.Debug("create_vm: placement: headroom check skipped, because no node would register the VM for HA")
		return nil
	}
	exempt := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		if _, registers := admissible[n]; !registers {
			exempt[n] = true
		}
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
	return &headroomPlan{
		check:  check,
		exempt: exempt,
		read:   func() ([]placement.HeadroomGuest, error) { return readHeadroomGuests(ctx, api, nodes) },
		log:    logger,
	}
}

// ensureGuests reads the cluster's guests into the check the first time an
// estimate needs them. A read that fails marks the guest list unknown.
func (h *headroomPlan) ensureGuests() {
	h.once.Do(func() {
		guests, err := h.read()
		if err != nil {
			h.log.Warn("create_vm: placement: the cluster's guests could not be read for the headroom estimate",
				log.Err(err))
			h.check.GuestsUnknown = true
		}
		h.check.Guests = guests
	})
}

// enforced reports whether a short estimate removes a node from placement.
func (h *headroomPlan) enforced() bool {
	return h != nil && h.check.Mode == config.HAHeadroomEnforce
}

// filter runs placement.Filter with the headroom check on req. A node the
// estimate rejects but where the VM would not be an HA guest stays in the
// result, because enforce must not refuse a VM the real decision would leave
// unregistered. Headroom is the last test Filter makes, so every node rejected
// for headroom has passed the others and can be restored as it is. The reasons
// of the nodes that stay rejected name the VM as "the new VM", because it has no
// ID yet. In warn mode the filter keeps every node, and the guests are not read.
func (h *headroomPlan) filter(
	facts []placement.NodeFacts, req placement.Request, logger *log.Logger,
) (pass []placement.NodeFacts, rejections map[string]string) {
	if h != nil {
		if h.enforced() {
			h.ensureGuests()
		}
		req.Headroom = h.check
	}
	pass, rejections = placement.Filter(facts, req)
	if !h.enforced() {
		return pass, rejections
	}
	restored := make(map[string]bool)
	for node, reason := range rejections {
		if !isHeadroomRejectionReason(reason) {
			continue
		}
		if h.exempt[node] {
			logger.Debug("create_vm: placement: headroom check does not apply, because the VM would not be an "+
				"HA guest on this node", log.String("node", node))
			delete(rejections, node)
			restored[node] = true
			continue
		}
		rejections[node] = relabelNewVM(reason)
	}
	if len(restored) > 0 {
		inPass := make(map[string]bool, len(pass))
		for i := range pass {
			inPass[pass[i].Node] = true
		}
		pass = pass[:0:0]
		for i := range facts {
			if inPass[facts[i].Node] || restored[facts[i].Node] {
				pass = append(pass, facts[i])
			}
		}
	}
	if len(rejections) == 0 {
		rejections = nil
	}
	return pass, rejections
}

// warnChosen logs the estimate for the node placement chose when the mode is
// warn. The filter keeps every node in that mode, so the warning is the whole
// effect, and it places the VM anyway. A node where the VM would not be an HA
// guest gets no estimate and no read.
func (h *headroomPlan) warnChosen(logger *log.Logger, facts []placement.NodeFacts, node string) {
	if h == nil || h.check.Mode != config.HAHeadroomWarn {
		return
	}
	if h.exempt[node] {
		logger.Debug("create_vm: placement: headroom check does not apply, because the VM would not be an HA "+
			"guest on this node", log.String("node", node))
		return
	}
	h.ensureGuests()
	result := placement.EstimateHeadroom(facts, *h.check, node)
	if !result.Short() {
		return
	}
	for _, line := range result.Lines() {
		logger.Warn("create_vm: placement: headroom estimate: "+relabelNewVM(line), log.String("node", node))
	}
}

// relabelNewVM replaces the zero VM ID the estimate prints for the VM being
// placed with a name an operator can read. A real VM ID is positive, so "VM 0 "
// never names another guest.
func relabelNewVM(text string) string {
	return strings.ReplaceAll(text, "VM 0 ", headroomNewVMLabel+" ")
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

// headroomVolumes lists the volumes the VM's config will reference, as the
// decision input names them, for the prediction made before placement. The
// first is the root disk. The second return value is empty when every volume is
// known, and otherwise says why one is not, so the caller runs no check. It
// reads nothing from the cluster, except the config-drive pool that
// deps.EffectiveISOStorage resolves once for the whole call, as the agent does
// when it writes the ISO.
//
// A pool a storage tier, a storage selector, or a storage plan picks is not
// known, nor is the pool of an encrypted ephemeral disk or of a persistent disk
// whose CID names none.
func headroomVolumes(
	ctx context.Context, deps Deps, cp createVMCloudProps, cloudPropsMap map[string]any, resolver *layeredResolver,
	diskCIDs []string,
) (volumes []ha.VMVolume, unknown string) {
	cfg := deps.Config
	if resolver == nil {
		return nil, "the VM's cloud properties are not resolved"
	}
	selection, err := ResolveStoragePlacementSelectors(cfg, "create_vm", cloudPropsMap, cp.EphemeralDiskSizeMB > 0)
	if err != nil {
		return nil, "the storage selectors do not resolve: " + err.Error()
	}
	if selection.SetManaged || selection.UsesAtomicSelectors || selection.FuturePersistentSet != "" {
		return nil, "a storage selector or plan picks the VM's pools"
	}

	root := cfg.VMStorage
	if pool, ok := resolver.String("storage_pool"); ok {
		root = pool
	} else if _, ok := resolver.String("storage_tier"); ok {
		return nil, "a storage tier picks the root disk's pool"
	}
	if root == "" {
		return nil, "the root disk's pool is not known before placement"
	}
	volumes = append(volumes, ha.VMVolume{Volume: root + ":vm-" + strconv.Itoa(headroomDecisionVMID) + "-disk-0"})

	if cp.EphemeralDiskSizeMB > 0 {
		var encrypted *bool
		if v, ok := resolver.Bool("encrypted"); ok {
			encrypted = &v
		}
		if ResolveEncrypted(cfg.Encrypted, encrypted) {
			return nil, "an encrypted ephemeral disk's pool is picked by a storage tier"
		}
		if _, ok := resolver.String("ephemeral_storage_tier"); ok {
			return nil, "a storage tier picks the ephemeral disk's pool"
		}
		pool := ephemeralPoolSetting(cfg, resolver, cp)
		if pool == "" {
			return nil, "the ephemeral disk's pool is not known before placement"
		}
		volumes = append(volumes, ha.VMVolume{
			Volume: pool + ":vm-" + strconv.Itoa(headroomDecisionVMID) + "-ephemeral-0",
		})
	}

	if cfg.BootAgentMode() != config.AgentModeNoAgent {
		iso := deps.EffectiveISOStorage(ctx)
		if iso == "" {
			return nil, "the config-drive ISO's pool is not known"
		}
		volumes = append(volumes, ha.VMVolume{
			Volume: iso + ":iso/vm-" + strconv.Itoa(headroomDecisionVMID) + "-config.iso", Kind: ha.VolumeISO,
		})
	}

	for _, cid := range diskCIDs {
		if cid == "" {
			continue
		}
		vol, ok := placementDiskVolume(cid)
		if !ok {
			return nil, "a persistent disk's CID names no pool"
		}
		volumes = append(volumes, vol)
	}
	return volumes, ""
}

// headroomAdmissibleSets runs the eligibility decision for the VM on each
// online node, over the volumes the VM's config will reference, and returns the
// hard admissible set of every node where the decision registers the VM for
// recovery elsewhere. The bool is false when no node does.
//
// Whether the root disk is a linked clone is decided after placement, so the
// decision runs both ways. A node counts only if the VM registers either way,
// which keeps a prediction from marking a VM as HA that the real decision
// leaves unregistered. The set is the union of the two, so a node the VM could
// be recovered on is never left out.
func headroomAdmissibleSets(
	deps Deps, cp createVMCloudProps, resolver *layeredResolver, bridges bridgePlan,
	profile *ha.Profile, facts []placement.NodeFacts, volumes []ha.VMVolume,
) (map[string][]string, bool) {
	cfg := deps.Config
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
		if !f.Online {
			continue
		}
		var set []string
		registers := true
		for _, linked := range []bool{false, true} {
			in := base
			in.Node = f.Node
			in.LinkedClone = linked
			in.Volumes = slices.Clone(volumes)
			in.Volumes[0].LinkedClone = linked
			scopeHAInputToNode(&in, cfg, cp, f.Node)
			in.Constraints = append(in.Constraints, bridgeConstraints...)
			decision, err := profile.DecideVM(in)
			if err != nil || !decision.Registers() || decision.Policy == ha.PolicyHold {
				registers = false
				break
			}
			for _, n := range append(slices.Clone(decision.Admissible), decision.Widened...) {
				if !slices.Contains(set, n) {
					set = append(set, n)
				}
			}
		}
		if !registers {
			continue
		}
		recoverable = true
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
