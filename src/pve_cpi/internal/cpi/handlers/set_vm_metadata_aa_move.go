package handlers

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/aapattern"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/placement"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/spreadclaim"
)

// A joining member that shares a node with an anti-affinity partner costs two
// migrations once the rule that pairs them is written, because PVE HA's CRM
// keeps every negative partner's current and target node out of each member's
// choice, so both members leave in the same round when a third node is free.
// The join moves its own VM off its partners' nodes first, which leaves the
// partner where it is, so the rule costs one migration instead of two. See
// section 6.7 of the topology spec for the reasoning behind each limit here.
const (
	// aaColoMoveWaitCap bounds the wait for the move below
	// pve.ha.motion_wait_sec, so a slow migration delays set_vm_metadata by a
	// few minutes at most before the join writes the rule anyway.
	aaColoMoveWaitCap = 3 * time.Minute
	// aaColoMoveMaxAge is how old a member's birth on its HA resource marker
	// may be for the join to move it. A member past it is a running workload
	// that a later set_vm_metadata touches, and PVE moves it instead.
	aaColoMoveMaxAge = time.Hour
	// aaColoMoveClaimSlack is how long the move's spread claim outlives the
	// wait, which covers the locked join that follows. The join releases the
	// claim once it returns, and a claim whose process is gone stops counting.
	aaColoMoveClaimSlack = 10 * time.Minute
	// aaColoMoveReleaseWait bounds the release of the claim after the join.
	aaColoMoveReleaseWait = 30 * time.Second
)

// aaColoMoveHooks are the reads and the migration the move makes. Each nil
// field uses the cluster through deps, and tests set the ones they fake.
type aaColoMoveHooks struct {
	now func() time.Time
	// config reads the VM's config on node.
	config func(ctx context.Context, node string, vmid int) (map[string]any, error)
	// reachable returns the candidates that show every storage in storages
	// as active and list every plain bridge of cfg.
	reachable func(ctx context.Context, cfg map[string]any, storages, candidates []string) ([]string, error)
	// facts gathers the scorer's node facts, counting the guests that carry
	// groupTag.
	facts func(ctx context.Context, groupTag string) ([]placement.NodeFacts, error)
	// stamp reads the cluster's stamp, or returns "" when it cannot.
	stamp func(ctx context.Context, node string) string
	// migrate asks PVE HA to move the VM from one node to another and waits
	// for the move within ctx.
	migrate func(ctx context.Context, vmid int, from, to string, budget time.Duration) error
}

// coloMoveHooks returns the move's hooks, with each one that deps leaves nil
// set to the cluster read or the PVE HA migration it stands for.
func (d Deps) coloMoveHooks(logger *log.Logger) aaColoMoveHooks {
	var h aaColoMoveHooks
	if d.coloMove != nil {
		h = *d.coloMove
	}
	if h.now == nil {
		h.now = time.Now
	}
	if h.config == nil {
		h.config = func(ctx context.Context, node string, vmid int) (map[string]any, error) {
			return d.PVE.QEMU().Config(ctx, node, vmid)
		}
	}
	if h.reachable == nil {
		h.reachable = func(ctx context.Context, cfg map[string]any, storages, candidates []string) ([]string, error) {
			return pinWidenReachable(ctx, d, cfg, storages, candidates, logger)
		}
	}
	if h.facts == nil {
		h.facts = func(ctx context.Context, groupTag string) ([]placement.NodeFacts, error) {
			return placement.GatherNodeFacts(ctx, d.PVE.Cluster(), d.PVE.Nodes(), logger, placement.GatherOptions{
				StorageName:         d.Config.VMStorage,
				GroupTag:            groupTag,
				MaintenanceNodeTags: d.Config.MaintenanceNodeTagsValue(),
				RequireNodeHealth:   true,
			})
		}
	}
	if h.stamp == nil {
		h.stamp = func(ctx context.Context, node string) string {
			stamp, err := pve.ReadClusterStamp(ctx, d.PVE, node)
			if err != nil {
				return ""
			}
			return stamp
		}
	}
	if h.migrate == nil {
		h.migrate = func(ctx context.Context, vmid int, from, to string, budget time.Duration) error {
			_, err := ha.MoveIntoSet(ctx, ha.NarrowDepsFor(d.PVE, logger, budget), ha.MoveInput{
				VMID: vmid, Node: from, Set: []string{to}, Targets: []string{to}, Budget: budget,
				AwaitUnanswered: true,
			})
			return err
		}
	}
	return h
}

// aaColoPartner is a planned partner of the joining member.
type aaColoPartner struct {
	vmid int
	node string
	// final says the pairing with the joiner can no longer change, so a
	// shared node with this partner is worth a move.
	final bool
}

// moveJoinerOffPartners moves a joining member off the nodes of its planned
// anti-affinity partners before the join writes the rule that pairs them,
// and returns the function that releases the move's spread claim, which the
// caller runs once the join has returned. It moves the VM only when all of
// these hold:
//
//   - The VM joins for the first time, which means no rule of its group holds
//     it yet and its marker's birth is less than aaColoMoveMaxAge old.
//   - Its plan pairs it with a member on its own node, and that pairing is
//     final, because every index up to the higher of the two is present or
//     held.
//   - Every volume it uses sits on shared storage, it has no PCI device, and
//     no positive affinity rule holds it.
//   - A node outside its partners' nodes, inside its pin's top priority, and
//     in its node's CPU domain when that is known, is online, out of
//     maintenance, reachable, and wins the score.
//   - The spread claim record takes the move's claim, so a sibling that
//     joins at the same moment sees that this VM is leaving.
//
// Anything else leaves the VM where it is, and PVE moves it once the rule is
// written, as it did before. A move that fails or runs past its wait logs a
// warning, and the join writes the rule all the same.
func moveJoinerOffPartners(
	ctx context.Context, deps Deps, ident haIdentity, vmid, index int, born int64, logger *log.Logger,
) (release func()) {
	release = func() {}
	if deps.Config == nil || !deps.Config.PlacementEnabled() || !ident.valid() || ident.Owner == "" ||
		deps.SpreadClaims == nil {
		return release
	}
	h := deps.coloMoveHooks(logger)
	now := h.now()
	if born <= 0 || absDuration(now.Sub(time.Unix(born, 0))) > aaColoMoveMaxAge {
		return release
	}
	sid := haResourceSid(vmid)
	profile, err := haRuleWriteProfile(ctx, deps)
	if err != nil {
		return release
	}
	// The rules are read before the guests, so a later set_vm_metadata of a
	// member that has joined already costs one read.
	set, err := readHARuleSet(ctx, deps.PVE.Cluster())
	if err != nil || aaJoinerInGroupRule(set, ident, sid) || positiveAffinityRule(set, sid) != "" {
		return release
	}
	scan, err := collectGroupMemberSids(ctx, deps, ident)
	if err != nil {
		return release
	}
	home := scan.guests[sid].Node
	if home == "" {
		return release
	}
	planner := newGroupPlanner(ctx, deps, profile, ident, scan, log.NewNopLogger())
	planner.sid, planner.index, planner.checkSelf = sid, index, true
	planner.recordHolds = nil
	if _, err := planner.plan(set); err != nil {
		return release
	}
	partners := aaColoPartners(planner.lastIn, planner.lastOut, scan, sid)
	if !aaColoShares(partners, home, nil) {
		return release
	}
	cfg, err := h.config(ctx, home, vmid)
	if err != nil {
		logger.Debug("set_vm_metadata: the VM's config could not be read, so PVE moves it off its anti-affinity "+
			"partner's node once the rule is written", log.Int(metadataKeyVMID, vmid), log.Err(err))
		return release
	}
	storages, why := aaColoMovableStorages(deps.Config, profile, cfg, home)
	if why != "" {
		logger.Info("set_vm_metadata: the VM shares a node with an anti-affinity partner, and "+why+
			", so PVE moves one of them once the rule is written", log.Int(metadataKeyVMID, vmid),
			log.String("node", home))
		return release
	}
	allowed := aaPinTopNodes(set, sid)
	domain, domainKnown := profile.DomainOf(home)
	var candidates []string
	for _, n := range profile.OnlineNodes() {
		if n != home && (allowed == nil || allowed[n]) && (!domainKnown || domain.Contains(n)) {
			candidates = append(candidates, n)
		}
	}
	if len(candidates) == 0 {
		return release
	}
	if candidates, err = h.reachable(ctx, cfg, storages, candidates); err != nil || len(candidates) == 0 {
		return release
	}
	facts, err := h.facts(ctx, ident.ruleName())
	if err != nil {
		logger.Debug("set_vm_metadata: the node facts could not be read, so PVE moves the VM off its "+
			"anti-affinity partner's node once the rule is written", log.Int(metadataKeyVMID, vmid), log.Err(err))
		return release
	}
	store := deps.SpreadClaims(deps.Config, h.stamp(ctx, home), logger)
	if store == nil {
		return release
	}
	budget := min(haMotionWait(deps), aaColoMoveWaitCap)
	claim, err := store.NewClaim(ident.ruleName(), now.Add(budget+aaColoMoveClaimSlack))
	if err != nil {
		return release
	}
	releaseClaim := func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), aaColoMoveReleaseWait)
		defer cancel()
		if err := claim.Release(rctx); err != nil {
			logger.Debug("set_vm_metadata: the claim of the move could not be released, so it counts until its "+
				"process ends or it expires", log.Int(metadataKeyVMID, vmid), log.Err(err))
		}
	}
	pick := aaColoPick{
		home: home, partners: partners, candidates: candidates, facts: facts,
		mem: aaConfigMemoryBytes(cfg), weights: aaColoWeights(deps.Config),
	}
	target, err := claim.PickLive(ctx, vmid, pick.choose)
	if err != nil || target == "" {
		releaseClaim()
		if err != nil {
			logger.Info("set_vm_metadata: the spread claim record could not take the move's claim, so PVE moves "+
				"the VM off its anti-affinity partner's node once the rule is written", log.Int(metadataKeyVMID, vmid),
				log.String("node", home), log.Err(err))
		}
		return release
	}
	logger.Info("set_vm_metadata: the VM shares a node with an anti-affinity partner, so PVE HA moves it to "+
		"another node before the rule is written, and the partner stays where it is",
		log.Int(metadataKeyVMID, vmid), log.String("from", home), log.String("to", target),
		log.String("wait", budget.String()))
	mctx, cancel := context.WithTimeout(ctx, budget)
	err = h.migrate(mctx, vmid, home, target, budget)
	cancel()
	switch {
	case err == nil:
		logger.Info("set_vm_metadata: PVE HA moved the VM off its anti-affinity partner's node",
			log.Int(metadataKeyVMID, vmid), log.String("node", target))
	case aaColoMoveTimedOut(err, mctx):
		logger.Warn("set_vm_metadata: PVE HA did not finish moving the VM off its anti-affinity partner's node "+
			"within the wait, so the rule is written now. If the move is still running, PVE may move the partner "+
			"too", log.Int(metadataKeyVMID, vmid), log.String("from", home), log.String("to", target),
			log.String("wait", budget.String()), log.Err(err))
	default:
		logger.Warn("set_vm_metadata: PVE HA did not move the VM off its anti-affinity partner's node, so the "+
			"rule is written now and PVE moves one of them", log.Int(metadataKeyVMID, vmid),
			log.String("from", home), log.String("to", target), log.Err(err))
	}
	return releaseClaim
}

// aaColoMoveTimedOut reports whether the move ended because its wait ran out.
func aaColoMoveTimedOut(err error, mctx context.Context) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(mctx.Err(), context.DeadlineExceeded) {
		return true
	}
	var moveErr *ha.MoveError
	return errors.As(err, &moveErr) && moveErr.Failure == ha.MoveIncomplete
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// aaJoinerInGroupRule reports whether a rule of the group, or the group's
// legacy rule, already holds sid, which means the VM has joined before.
func aaJoinerInGroupRule(set *ha.RuleSet, ident haIdentity, sid string) bool {
	base, legacy := ident.ruleName(), legacyRuleName(ident)
	for i := range set.Rules {
		r := &set.Rules[i]
		if (aapattern.IsGroupRule(base, r.ID) || r.ID == legacy) && r.HasResource(sid) {
			return true
		}
	}
	return false
}

// aaColoPartners returns the members the plan puts in a rule with sid, each
// with the node the scan shows it on and whether its pairing with sid is
// final. A pairing is final when every index from 0 up to the higher of the
// two members' indexes is present or held, because the layout places a member
// by its rank among the indexes below it, so no later member can change it.
func aaColoPartners(in aapattern.Input, out aapattern.Output, scan *groupScan, sid string) []aaColoPartner {
	indexes := map[string]int{}
	present := map[int]bool{}
	for _, m := range in.Members {
		if m.Index < 0 {
			continue
		}
		present[m.Index] = true
		if !m.Reserved {
			indexes[m.Key] = m.Index
		}
	}
	final := func(a, b int) bool {
		for i := 0; i <= max(a, b); i++ {
			if !present[i] {
				return false
			}
		}
		return true
	}
	self, selfKnown := indexes[sid]
	seen := map[string]bool{}
	var partners []aaColoPartner
	for _, rule := range slices.Sorted(maps.Keys(out.Rules)) {
		keys := out.Rules[rule]
		if !slices.Contains(keys, sid) {
			continue
		}
		for _, k := range keys {
			if k == sid || seen[k] {
				continue
			}
			seen[k] = true
			vmid, ok := haSidVMID(k)
			node := scan.guests[k].Node
			if !ok || node == "" {
				continue
			}
			idx, known := indexes[k]
			partners = append(partners, aaColoPartner{
				vmid: vmid, node: node, final: selfKnown && known && final(self, idx),
			})
		}
	}
	return partners
}

// haSidVMID returns the VMID of an HA resource id vm:<vmid>.
func haSidVMID(sid string) (int, bool) {
	rest, ok := strings.CutPrefix(sid, "vm:")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil && n > 0
}

// aaColoShares reports whether a partner whose pairing is final sits on
// home, reading each partner's node from moving when a live claim names its
// VM there.
func aaColoShares(partners []aaColoPartner, home string, moving map[int]string) bool {
	for _, p := range partners {
		if p.final && aaColoNode(p, moving) == home {
			return true
		}
	}
	return false
}

func aaColoNode(p aaColoPartner, moving map[int]string) string {
	if n, ok := moving[p.vmid]; ok {
		return n
	}
	return p.node
}

// aaColoMovableStorages returns the storages of the VM's volumes, or the
// reason the VM is left to PVE: a volume on storage that is not shared or not
// known, or a PCI device that holds the VM to its node.
func aaColoMovableStorages(
	cfg *config.CPIConfig, profile *ha.Profile, vmCfg map[string]any, home string,
) ([]string, string) {
	if configHasPCIPassthrough(vmCfg) {
		return nil, "it has a PCI device"
	}
	seen := map[string]bool{}
	var storages []string
	for _, v := range pve.ConfigAccessVolumes(vmCfg, home) {
		storage, _, ok := strings.Cut(v.Volume, ":")
		if !ok || storage == "" {
			return nil, "a volume names no storage"
		}
		if seen[storage] {
			continue
		}
		seen[storage] = true
		if shared, known := profile.StorageShared(cfg, storage); !shared || !known {
			return nil, "it has a volume on storage " + storage + " that is not known to be shared"
		}
		storages = append(storages, storage)
	}
	slices.Sort(storages)
	return storages, ""
}

// aaPinTopNodes returns the nodes of the highest priority in every enabled
// node affinity rule that holds sid, intersected, or nil when no such rule
// holds it. A move inside those nodes keeps the VM's AZ and gives failback no
// reason to pull it back.
func aaPinTopNodes(set *ha.RuleSet, sid string) map[string]bool {
	var out map[string]bool
	for i := range set.Rules {
		r := &set.Rules[i]
		if r.Type != ha.RuleTypeNodeAffinity || r.Disabled || !r.HasResource(sid) || len(r.Nodes) == 0 {
			continue
		}
		top := 0
		first := true
		for _, p := range r.Nodes {
			if first || p > top {
				top, first = p, false
			}
		}
		nodes := map[string]bool{}
		for n, p := range r.Nodes {
			if p == top && (out == nil || out[n]) {
				nodes[n] = true
			}
		}
		out = nodes
	}
	return out
}

// aaConfigMemoryBytes returns the memory the VM's config gives it in bytes,
// or 0 when the config does not say.
func aaConfigMemoryBytes(cfg map[string]any) int64 {
	text, ok := pve.ConfigString(cfg, "memory")
	if !ok {
		return 0
	}
	mb, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil || mb <= 0 {
		return 0
	}
	return mb << 20
}

// aaColoWeights are the weights create_vm scores a member of an
// anti-affinity group with, before any cloud property overrides them.
func aaColoWeights(cfg *config.CPIConfig) placement.Weights {
	w := cfg.EffectiveWeights()
	return placement.Weights{
		Mem: w.Mem, Storage: w.Storage, CPU: w.CPU, GuestCount: w.GuestCount,
		MemorySignal: cfg.MemorySignalValue(), AntiAffinity: placement.DefaultWeights().AntiAffinity,
	}
}

// aaColoPick chooses the move's target in one turn with the spread claim
// record.
type aaColoPick struct {
	home       string
	partners   []aaColoPartner
	candidates []string
	facts      []placement.NodeFacts
	mem        int64
	weights    placement.Weights
}

// choose returns the node to move the VM to, or "" to leave it. A live claim
// that names a partner's VM puts the partner on the claimed node, so the
// second of two siblings that share a node sees that the first is leaving and
// stays. A live claim also counts as a member of the group on its node for the
// anti-affinity axis, in place of the node its VM is tagged on.
func (p aaColoPick) choose(live []spreadclaim.Live) string {
	moving := map[int]string{}
	for _, c := range live {
		if c.VMID > 0 {
			moving[c.VMID] = c.Node
		}
	}
	if !aaColoShares(p.partners, p.home, moving) {
		return ""
	}
	taken := map[string]bool{p.home: true}
	for _, partner := range p.partners {
		taken[aaColoNode(partner, moving)] = true
	}
	var cands []string
	for _, n := range p.candidates {
		if !taken[n] {
			cands = append(cands, n)
		}
	}
	if len(cands) == 0 {
		return ""
	}
	pass, _ := placement.Filter(aaColoClaimCounts(p.facts, live), placement.Request{
		CandidateNodes: cands, RequiredMemBytes: p.mem,
	})
	if len(pass) == 0 {
		return ""
	}
	return placement.Pick(placement.Score(pass, p.weights, nil), nil)
}

// aaColoClaimCounts adds each live claim to the group count of its node. A
// claim whose VM the facts already count on another node moves that count to
// the claimed node.
func aaColoClaimCounts(facts []placement.NodeFacts, live []spreadclaim.Live) []placement.NodeFacts {
	if len(live) == 0 {
		return facts
	}
	out := slices.Clone(facts)
	byNode := map[string]int{}
	for i := range out {
		byNode[out[i].Node] = i
	}
	for _, c := range live {
		at, ok := byNode[c.Node]
		if !ok {
			continue
		}
		if c.VMID > 0 {
			from := -1
			for i := range out {
				if slices.Contains(out[i].SameGroupVMIDs, c.VMID) {
					from = i
					break
				}
			}
			if from == at {
				continue
			}
			if from >= 0 && out[from].SameGroupCount > 0 {
				out[from].SameGroupCount--
			}
		}
		out[at].SameGroupCount++
	}
	return out
}
