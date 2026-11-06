package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// clusterLockOwner builds the owner token for an anti-affinity lock. It names
// the group and the VM, and pve.ProcessLockOwner adds the host, the process,
// and a per-process sequence, so the token is unique per acquisition across
// every Director that shares the cluster.
func clusterLockOwner(key string, vmid int) string {
	return pve.ProcessLockOwner(fmt.Sprintf("aa-%s-%d", key, vmid))
}

// antiAffinityLockPrefix prefixes an instance group's anti-affinity lock name.
const antiAffinityLockPrefix = "aa-"

// antiAffinityLockMinTTL is the shortest claim the anti-affinity lock records,
// whatever cluster_lock_timeout_sec says. Part of every claim is spent before
// the read-modify-write starts or after it must stop: up to a second lost when
// the expiry is cut to whole seconds, the create and its admission, the grace
// pause, two confirming reads, and the margin a release leaves before the
// expiry. That comes to about ten seconds, which is more than twice a small
// setting. A claim that expires inside the grace can be stolen while its
// holder still runs, and one inside the release margin is never deleted, so the lock
// pool stands until the claim expires. Thirty seconds leaves about twenty for
// the read-modify-write.
const antiAffinityLockMinTTL = 30 * time.Second

// haRuleNamePrefix namespaces every CPI-managed HA anti-affinity rule so a
// cluster-wide scan can find and clean them by vmid without knowing the group.
// A rule is named bosh-aa-<hash12> (see haIdentity), and the same string is the
// member tag. Rules from older CPIs are named bosh-aa-<group> and carry no
// ownership marker.
const haRuleNamePrefix = "bosh-aa-"

// haResourceType and haRuleAffinity are the fixed PVE values for negative
// (spread-apart) resource-affinity rules on PVE 9.x.
const (
	haRuleType     = "resource-affinity"
	haRuleAffinity = "negative"
)

// haRuleEntry is the subset of a GET /cluster/ha/rules list entry the CPI reads.
// Resources is decoded flexibly (see parseHaResources) because PVE may return
// it as a CSV string, an array, or an object keyed by resource id.
type haRuleEntry struct {
	Rule      string          `json:"rule"`
	Type      string          `json:"type"`
	Affinity  string          `json:"affinity"`
	Comment   string          `json:"comment"`
	Resources json.RawMessage `json:"resources"`
}

// haResourceSid renders the PVE HA resource id for a VMID, e.g. 100 -> "vm:100".
func haResourceSid(vmid int) string {
	return "vm:" + strconv.Itoa(vmid)
}

// applyAntiAffinityMembership is the create_vm entry point for opt-in HA
// anti-affinity. It is a no-op unless pve.ha.anti_affinity is "on" in a mode
// that writes HA and the VM carries an instance-group name. Generic HA failures
// (HA unconfigured, a transient rule-write hiccup) are logged non-fatally,
// preserving the best-effort intent. An error propagateHARuleError names is
// returned so the director re-drives rather than silently dropping the VM from
// its spread rule: a retriable error (cluster-lock timeout, read-after-write
// verify failure, rules that kept changing), a rule PVE left out of effect in
// managed mode, or in managed mode a membership the feasibility checks refused.
func applyAntiAffinityMembership(ctx context.Context, deps Deps, vmid int, env map[string]any, logger *log.Logger) error {
	if !deps.Config.HAAntiAffinityRulesEnabled() {
		return nil
	}
	ident := haIdentityFor(deps, env)
	if ident.Group == "" {
		return nil
	}
	if ident.Owner == "" {
		// A rule the CPI cannot mark as its own is a rule it could never edit
		// or clean up safely, so a request without a director_uuid writes none.
		logger.Warn("create_vm: HA anti-affinity membership skipped because the request carries no director_uuid "+
			"to mark the rule with", log.Int(metadataKeyVMID, vmid), log.String("group", ident.Group))
		return nil
	}
	if aaErr := ensureAntiAffinityMembership(ctx, deps, ident, vmid, logger); aaErr != nil {
		if propagateHARuleError(deps, aaErr) {
			return aaErr
		}
		logger.Warn("create_vm: HA anti-affinity membership not fully applied (non-fatal)",
			log.Int(metadataKeyVMID, vmid), log.String("group", ident.Group), log.Err(aaErr))
	}
	return nil
}

// ensureAntiAffinityMembership registers the VM as a PVE HA resource and adds it
// to the cluster-level negative resource-affinity rule for its BOSH instance
// group, so PVE enforces spreading at the hypervisor level. Every PVE failure
// (including "HA not configured on this cluster") is returned for the caller,
// which logs it unless it must fail the call (propagateHARuleError).
//
// A negative resource-affinity rule is only meaningful with two or more members,
// so the rule is materialized from the live set of same-group guests plus this
// VM. When fewer than two members exist the rule is not created (and a stale
// single-member rule, if any, is removed).
//
// The whole read, check, and write runs under the one cluster-scoped rules lock
// when the cluster lock is enabled, and the rule writer changes members with a
// PUT that carries the read's digest, so there is never a window without the
// rule.
func ensureAntiAffinityMembership(ctx context.Context, deps Deps, ident haIdentity, vmid int, logger *log.Logger) error {
	if !ident.valid() {
		return nil
	}

	// The version gate resolves the HA profile, which probes the whole
	// cluster when nothing has resolved it yet. That runs before the lock so
	// the probe never eats into the lock's window.
	profile, err := haRuleWriteProfile(ctx, deps)
	if err != nil {
		return fmt.Errorf("anti-affinity: %w", err)
	}

	return withHARulesLock(ctx, deps, vmid, logger, func() error {
		return ensureAntiAffinityMembershipLocked(ctx, deps, profile, ident, vmid, logger)
	})
}

// releaseAntiAffinityLock releases an anti-affinity lock. The release is
// detached and bounded for the same reason as withVMIDLock's: a cancelled
// request must not orphan the sentinel pool, and the anti-affinity lock's
// longer TTL makes an orphan proportionally more disruptive to concurrent
// create_vm calls. A failure is only logged.
func releaseAntiAffinityLock(ctx context.Context, handle *pve.ClusterLockHandle, key string, logger *log.Logger) {
	relCtx, relCancel := detachedContext(ctx, lockReleaseTimeout)
	defer relCancel()
	if relErr := handle.Release(relCtx); relErr != nil {
		logger.Warn("anti-affinity: release cluster lock failed (non-fatal)",
			log.String("group", key), log.Err(relErr))
	}
}

// acquireAntiAffinityLock acquires a cross-process sentinel lock. Separated so
// the lock plumbing (owner token, timeout/TTL from config) stays out of the RMW
// body. key is haRulesLockKey, the one lock every CPI rule write holds, or the
// bare group name, which is the key an older CPI locks its bosh-aa-<group>
// rule under.
func acquireAntiAffinityLock(ctx context.Context, deps Deps, key string, vmid int) (*pve.ClusterLockHandle, error) {
	poolSvc := deps.PVE.Pools()
	if poolSvc == nil {
		// No pool service wired: cannot lock. Treat as retriable so the operator
		// notices the misconfiguration rather than racing silently.
		return nil, cpierrors.WrapAs(
			cpierrors.Cloud("anti-affinity: cluster_lock_mode=pool but no pool service available"),
			cpierrors.TypeRetriableCloud, "anti-affinity: acquire cluster lock")
	}
	// TTL and timeout are deliberately decoupled: TTL is 2× the acquire
	// timeout so a holder whose RMW runs for the full timeout duration is not
	// stolen mid-flight by a concurrent waiter. A crashed holder (RMW aborted,
	// release never called) is reclaimed once the TTL lapses, which is
	// acceptable for the advisory lock use-case. The TTL never drops below
	// antiAffinityLockMinTTL, which raises it only for a setting under 15
	// seconds, and the wait stays the setting either way.
	timeout := time.Duration(deps.Config.ClusterLockTimeoutSecValue()) * time.Second
	ttl := max(2*timeout, antiAffinityLockMinTTL)
	owner := clusterLockOwner(key, vmid)
	// Two holders at once would place two instances of a group on one node,
	// so this lock pays the grace pause on every create to narrow that.
	return pve.AcquireClusterLock(ctx, poolSvc, antiAffinityLockPrefix+key, owner, ttl, timeout, pve.WithCreateGrace())
}

// ensureAntiAffinityMembershipLocked is the read-modify-write body, run under
// the rules lock when enabled and after the PVE 9 gate has passed. It registers
// the HA resource with the ownership marker, plans the group's rules from the
// scan and a fresh read, and lets the rule writer apply the plan. It then moves
// this Director's members out of the group's unmarked rule from an older CPI.
func ensureAntiAffinityMembershipLocked(
	ctx context.Context, deps Deps, profile *ha.Profile, ident haIdentity, vmid int, logger *log.Logger,
) error {
	svc := deps.PVE.Cluster()
	sid := haResourceSid(vmid)
	marker := ident.marker()

	// 1. Register the VM as an HA resource (idempotent; "already defined" is fine).
	if err := svc.CreateHaResources(ctx, &cluster.CreateHaResourcesParams{Sid: sid, Comment: &marker}); err != nil {
		if !isHaAlreadyExists(err) {
			logger.Warn("anti-affinity: register HA resource failed (continuing best-effort)",
				log.String("sid", sid), log.Err(err))
		}
	}

	// 2. Compute the full member set: existing same-group guests + this VM.
	scan, scanErr := collectGroupMemberSids(ctx, deps, ident)
	if scanErr != nil {
		// Without the member set we cannot safely write the rule. Warn and stop.
		return fmt.Errorf("anti-affinity: scan group members: %w", scanErr)
	}

	// 3. Plan and write the group's rules.
	planner := &aaPlanner{
		ident: ident, sid: sid, scan: scan, logger: logger,
		nodes:     len(profileNodeNames(profile)),
		placeable: placeableNodeCount(profile),
		split:     deps.Config.HAAntiAffinityOverflowValue() == config.HAOverflowSplit,
	}
	writer := newHARuleWriter(deps, profile, logger)
	res, err := writer.Apply(ctx, "anti-affinity", planner.plan)
	if err != nil {
		return err
	}
	if planner.placedIn != "" && len(res.Wrote) > 0 {
		if err := verifyAntiAffinityMember(ctx, deps, planner.placedIn, sid, logger); err != nil {
			return err
		}
	}

	// 4. The unmarked rule from an older CPI gives up the members of ours that
	// the hashed rules now hold. A member the overflow or the placeable cap left
	// out keeps the old rule's protection.
	return migrateLegacyAntiAffinityRule(ctx, deps, writer, ident, heldByChunks(res.Set, ident, scan.legacy), vmid,
		logger)
}

// aaPlanner plans a group's negative rules from the member scan and a read of
// the rules. It is called again after every write with a fresh read, so it
// returns no writes once the rules hold the plan.
//
// A group's rule is bosh-aa-<hash12>. With pve.ha.anti_affinity_overflow
// split, a group with more members than the cluster's configured nodes spills
// into bosh-aa-<hash12>-2, -3, and so on, because PVE disables a negative rule
// with more members than configured nodes. With skip, the default, such a group
// gets no rule written, and the reason is logged. Either way a member joins a
// rule only while that rule has fewer members than placeable nodes, so a node in
// maintenance never makes the CPI rewrite a rule, and a replacement member waits
// until it can be placed apart. Existing members keep their rule.
type aaPlanner struct {
	ident     haIdentity
	sid       string
	scan      *groupScan
	logger    *log.Logger
	nodes     int
	placeable int
	split     bool

	// placedIn is the rule this VM is in after the last plan, or "".
	placedIn string
	logged   bool
}

// aaChunkName names a group's n-th rule, the first being the bare name.
func aaChunkName(base string, n int) string {
	if n <= 1 {
		return base
	}
	return base + "-" + strconv.Itoa(n)
}

// aaChunkIndex returns n when id is the group's n-th rule.
func aaChunkIndex(base, id string) (int, bool) {
	if id == base {
		return 1, true
	}
	rest, ok := strings.CutPrefix(id, base+"-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 2 || strconv.Itoa(n) != rest {
		return 0, false
	}
	return n, true
}

func (p *aaPlanner) plan(set *ha.RuleSet) ([]ha.RuleOp, error) {
	p.placedIn = ""
	existing, maxIndex, err := p.existingChunks(set)
	if err != nil {
		return nil, err
	}
	desired, assigned := p.keptMembers(existing, maxIndex)
	if p.skipOversized(assigned) {
		return nil, nil
	}
	maxIndex = p.assignPending(desired, assigned, maxIndex)
	return p.chunkOps(set, existing, desired, maxIndex), nil
}

// skipOversized reports whether skip mode leaves the group's rules alone,
// because the group has more members than the cluster's configured nodes. PVE
// disables a negative rule that large, so skip writes no rule: a new group gets
// none, and an existing rule keeps the members it has. Split mode, and a cluster
// whose configured nodes are unknown, never skip.
func (p *aaPlanner) skipOversized(assigned map[string]bool) bool {
	if p.split || p.nodes <= 0 {
		return false
	}
	total := len(assigned)
	if !assigned[p.sid] {
		total++
	}
	for s := range p.scan.members {
		if s != p.sid && !assigned[s] {
			total++
		}
	}
	if total <= p.nodes {
		return false
	}
	if !p.logged {
		p.logged = true
		p.logger.Warn("anti-affinity: this group has more instances than the cluster has nodes, so no rule is "+
			"written for it (pve.ha.anti_affinity_overflow is skip; split divides the group into rules that fit)",
			log.String("rule", p.ident.ruleName()), log.String("sid", p.sid), log.Int("members", total),
			log.Int("configured_nodes", p.nodes), log.String("overflow", config.HAOverflowSkip))
	}
	return true
}

// heldByChunks returns the sids of mine that one of the group's hashed rules
// holds in set.
func heldByChunks(set *ha.RuleSet, ident haIdentity, mine map[string]struct{}) map[string]struct{} {
	if len(mine) == 0 || set == nil {
		return nil
	}
	base := ident.ruleName()
	out := map[string]struct{}{}
	for i := range set.Rules {
		r := &set.Rules[i]
		if _, ok := aaChunkIndex(base, r.ID); !ok || r.Disabled {
			continue
		}
		for _, sid := range r.Resources {
			if _, ok := mine[sid]; ok {
				out[sid] = struct{}{}
			}
		}
	}
	return out
}

// existingChunks returns the group's rules keyed by chunk index, and the
// highest index. A rule under our hashed name that does not carry our marker is
// not ours to rewrite, so it fails the plan.
func (p *aaPlanner) existingChunks(set *ha.RuleSet) (map[int]*ha.Rule, int, error) {
	base := p.ident.ruleName()
	existing := map[int]*ha.Rule{}
	maxIndex := 0
	for i := range set.Rules {
		r := &set.Rules[i]
		n, ok := aaChunkIndex(base, r.ID)
		if !ok {
			continue
		}
		if haOwnershipOf(r.Comment, p.ident.Owner) != haOwnedByUs || r.Type != haRuleType {
			return nil, 0, fmt.Errorf("anti-affinity: rule %q exists without this Director's ownership marker; "+
				"leaving it alone", r.ID)
		}
		existing[n] = r
		maxIndex = max(maxIndex, n)
	}
	return existing, maxIndex, nil
}

// keptMembers returns the members each existing rule keeps.
//
// This ensure path only ever grows a rule: the existing members stay, whatever
// the scan says. However authoritative the scan, its TAG view can still trail
// reality (a same-group twin mid-create has no tag yet), and the delete path
// (removeAntiAffinityMembership) is the sole authority for shrinking. The one
// exception is a sid whose GUEST no longer exists anywhere in the fleet, which
// is dropped only when the enumeration covered every member (liveComplete),
// because a rule around a dangling sid would name a resource PVE no longer has
// and wedge every later create.
func (p *aaPlanner) keptMembers(existing map[int]*ha.Rule, maxIndex int) (map[int][]string, map[string]bool) {
	desired := map[int][]string{}
	assigned := map[string]bool{}
	for n := 1; n <= maxIndex; n++ {
		r := existing[n]
		if r == nil {
			continue
		}
		for _, s := range r.Resources {
			if _, ok := p.scan.live[s]; !ok && p.scan.liveComplete {
				p.logger.Info("anti-affinity: dropping rule member with no live guest",
					log.String("rule", r.ID), log.String("sid", s))
				continue
			}
			if assigned[s] {
				continue
			}
			assigned[s] = true
			desired[n] = append(desired[n], s)
		}
	}
	return desired, assigned
}

// assignPending places this VM and every scanned member not yet in a rule into
// the first rule with room, opening a new rule in split mode (or when the group
// has none), and returns the new highest index. A rule has room while it holds
// fewer members than the configured nodes and the placeable nodes.
func (p *aaPlanner) assignPending(desired map[int][]string, assigned map[string]bool, maxIndex int) int {
	pending := map[string]struct{}{p.sid: {}}
	for s := range p.scan.members {
		pending[s] = struct{}{}
	}
	room := p.nodes
	if room <= 0 {
		room = math.MaxInt
	}
	if p.placeable > 0 {
		room = min(room, p.placeable)
	}
	for _, s := range strings.Split(sidsCSV(pending), ",") {
		if s == "" || assigned[s] {
			continue
		}
		target := 0
		for n := 1; n <= maxIndex; n++ {
			if len(desired[n]) < room {
				target = n
				break
			}
		}
		if target == 0 && (p.split || maxIndex == 0) {
			maxIndex++
			target = maxIndex
		}
		if target == 0 || len(desired[target]) >= room {
			p.logOverflow(s, len(desired[1]))
			continue
		}
		assigned[s] = true
		desired[target] = append(desired[target], s)
	}
	return maxIndex
}

// chunkOps returns the writes that make the group's rules hold desired, led by
// the pin flattening their members need, or nil when the rules already do.
func (p *aaPlanner) chunkOps(set *ha.RuleSet, existing map[int]*ha.Rule, desired map[int][]string,
	maxIndex int) []ha.RuleOp {
	base := p.ident.ruleName()
	var pins, ops []ha.RuleOp
	for n := 1; n <= maxIndex; n++ {
		name := aaChunkName(base, n)
		members := desired[n]
		sort.Strings(members)
		cur := existing[n]
		if len(members) < 2 {
			// A negative rule is meaningless below two members: remove a
			// stale one left from an earlier, larger membership.
			if cur != nil {
				ops = append(ops, ha.RuleOp{Kind: ha.OpDelete, Rule: ha.Rule{ID: name}})
			}
			continue
		}
		if slices.Contains(members, p.sid) {
			p.placedIn = name
		}
		pins = append(pins, flattenMemberPins(set, members, p.ident.Owner)...)
		want := ha.Rule{ID: name, Type: haRuleType, Affinity: haRuleAffinity, Resources: members,
			Comment: p.ident.marker()}
		switch {
		case cur == nil:
			ops = append(ops, ha.RuleOp{Kind: ha.OpCreate, Rule: want})
		case !slices.Equal(cur.Resources, members) || cur.Affinity != haRuleAffinity:
			next := cur.Clone()
			next.Resources, next.Affinity, next.Errors = members, haRuleAffinity, nil
			ops = append(ops, ha.RuleOp{Kind: ha.OpUpdate, Rule: next})
		}
	}
	if len(ops) == 0 {
		return nil
	}
	return append(pins, ops...)
}

// logOverflow says once per plan why a member stays out of every rule: in
// split mode, or when the configured nodes are unknown, only because the
// cluster has fewer placeable nodes than the group has members.
func (p *aaPlanner) logOverflow(sid string, size int) {
	if p.logged {
		return
	}
	p.logged = true
	mode := config.HAOverflowSkip
	if p.split {
		mode = config.HAOverflowSplit
	}
	p.logger.Warn("anti-affinity: member left out of the group's rules, because the cluster has fewer placeable "+
		"nodes than the group has members; a later create adds it once it can be placed apart from every other "+
		"member",
		log.String("sid", sid), log.String("rule", p.ident.ruleName()), log.Int("members", size),
		log.Int("configured_nodes", p.nodes), log.Int("placeable_nodes", p.placeable),
		log.String("overflow", mode))
}

// flattenMemberPins returns the writes that give every member's CPI pin a
// single priority. PVE refuses a node-affinity rule with several priorities
// for a resource that is in a resource-affinity rule, so the pins go flat
// before the member joins. A pin that carries another Director's marker is
// left, and the feasibility checks then refuse the membership.
func flattenMemberPins(set *ha.RuleSet, members []string, owner string) []ha.RuleOp {
	var ops []ha.RuleOp
	for _, s := range members {
		vmid, ok := vmidFromSid(s)
		if !ok {
			continue
		}
		pin := set.Find(naRuleNameFor(vmid))
		if pin == nil || !pin.MultiplePriorities() || haOwnershipOf(pin.Comment, owner) == haOwnedByOther {
			continue
		}
		ops = append(ops, ha.RuleOp{Kind: ha.OpUpdate, Rule: *ha.Flattened(pin)})
	}
	return ops
}

// vmidFromSid parses vm:<vmid>.
func vmidFromSid(sid string) (int, bool) {
	rest, ok := strings.CutPrefix(sid, "vm:")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil && n > 0
}

// legacyRuleName is the name an older CPI gave the group's rule, bosh-aa-<group>.
func legacyRuleName(ident haIdentity) string {
	return haRuleNamePrefix + ident.Group
}

// migrateLegacyAntiAffinityRule moves this Director's members out of the
// group's unmarked rule from an older CPI. Two Directors may share that rule, so
// the CPI never adopts it whole. It removes only the members in mine, which the
// scan proved belong to this Director and deployment, from the old rule. A rule
// that carries a marker is never touched. This migration runs for one release,
// and after that the CPI ignores unmarked rules.
//
// An older CPI rewrites the rule by delete and create under the lock keyed on
// the bare group name, not the rules lock this create holds, so when the
// cluster lock is enabled the migration also holds that key for its whole run,
// inside the rules lock. It snapshots the old rule, takes the key, and writes
// only when a fresh read still shows the snapshot's comment and members. When
// they differ, or the lock is busy, it writes nothing, and a later create of
// the group retries. The remaining members are written with a PUT under the
// read's digest. When fewer than two would remain, the rule no longer spreads
// anything, so it is deleted and nothing is created in its place.
func migrateLegacyAntiAffinityRule(
	ctx context.Context, deps Deps, writer *ha.RuleWriter, ident haIdentity,
	mine map[string]struct{}, vmid int, logger *log.Logger,
) error {
	if len(mine) == 0 {
		return nil
	}
	name := legacyRuleName(ident)
	if name == ident.ruleName() {
		return nil
	}
	svc := deps.PVE.Cluster()
	old, err := findHaRule(ctx, svc, name)
	if err != nil {
		return fmt.Errorf("anti-affinity: read unmarked rule %q for migration: %w", name, err)
	}
	if old == nil || haOwnershipOf(old.Comment, ident.Owner) != haUnmarked {
		return nil
	}
	snapshot := parseHaResources(old.Resources)
	remaining, moved := splitLegacyMembers(snapshot, mine)
	if moved == 0 {
		return nil
	}

	if deps.Config.ClusterLockEnabled() {
		handle, lockErr := acquireAntiAffinityLock(ctx, deps, ident.Group, vmid)
		if lockErr != nil {
			logger.Warn("anti-affinity: unmarked rule migration deferred because its lock is not available; "+
				"a later create of the group retries", log.String("rule", name), log.Err(lockErr))
			return nil
		}
		defer releaseAntiAffinityLock(ctx, handle, ident.Group, logger)
	}

	aborted := false
	res, err := writer.Apply(ctx, "anti-affinity migration", func(set *ha.RuleSet) ([]ha.RuleOp, error) {
		fresh := set.Find(name)
		switch {
		case fresh == nil:
			// Gone: deleted by this migration's own write, or by someone else.
			return nil, nil
		case fresh.Comment == old.Comment && len(remaining) >= 2 && sameSidSlice(fresh.Resources, remaining):
			// This migration's PUT has landed.
			return nil, nil
		case fresh.Comment != old.Comment || !sameSidSlice(fresh.Resources, snapshot):
			aborted = true
			return nil, nil
		}
		aborted = false
		if len(remaining) < 2 {
			return []ha.RuleOp{{Kind: ha.OpDelete, Rule: ha.Rule{ID: name}}}, nil
		}
		next := fresh.Clone()
		next.Resources = strings.Split(sidsCSV(remaining), ",")
		next.Errors = nil
		return []ha.RuleOp{{Kind: ha.OpUpdate, Rule: next}}, nil
	})
	if err != nil {
		return fmt.Errorf("anti-affinity: migrate unmarked rule %q: %w", name, err)
	}
	if len(res.Wrote) == 0 {
		if aborted {
			logger.Warn("anti-affinity: unmarked rule changed while this create read it; migration aborted, "+
				"and a later create of the group retries", log.String("rule", name), log.String("read", sidsCSV(snapshot)))
		}
		return nil
	}
	logger.Info("anti-affinity: moved this Director's members out of an unmarked rule",
		log.String("from", name), log.String("to", ident.ruleName()),
		log.Int("moved", moved), log.Int("left", len(remaining)), log.Bool("old_rule_kept", len(remaining) >= 2))
	return nil
}

// sameSidSlice reports whether have holds exactly the sids in want.
func sameSidSlice(have []string, want map[string]struct{}) bool {
	if len(have) != len(want) {
		return false
	}
	for _, s := range have {
		if _, ok := want[s]; !ok {
			return false
		}
	}
	return true
}

// splitLegacyMembers splits an old rule's members into the ones that stay,
// which are every member not in mine, and a count of the ones that move.
func splitLegacyMembers(have, mine map[string]struct{}) (remaining map[string]struct{}, moved int) {
	remaining = make(map[string]struct{}, len(have))
	for s := range have {
		if _, ok := mine[s]; ok {
			moved++
			continue
		}
		remaining[s] = struct{}{}
	}
	return remaining, moved
}

// verifyAntiAffinityMember performs the opt-in read-after-write check: it
// re-lists the HA rules and asserts that sid is present in ruleName's member
// set. A concurrent writer that recreated the rule without this VM (lost-update)
// is caught here and surfaced as a retriable error so the director re-drives,
// rather than silently losing the spread guarantee. A no-op when
// antiaffinity_verify is off.
//
// A single re-read is used here. PVE HA-rule reads are strongly consistent
// within the same cluster because pmxcfs serializes writes; a false-negative
// (rule absent) is therefore a real concurrent drop, not read lag, so a single
// re-read is sufficient. A false-negative triggers a bounded director re-drive
// (not a storm): the director re-issues create_vm once before declaring failure.
func verifyAntiAffinityMember(ctx context.Context, deps Deps, ruleName, sid string, logger *log.Logger) error {
	if !deps.Config.AntiAffinityVerifyEnabled() {
		return nil
	}
	svc := deps.PVE.Cluster()
	rule, err := findHaRule(ctx, svc, ruleName)
	if err != nil {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("anti-affinity: verify re-list rule %q", ruleName))
	}
	if rule == nil {
		return cpierrors.WrapAs(
			cpierrors.Cloud("anti-affinity: rule %q absent after recreate (concurrent drop)", ruleName),
			cpierrors.TypeRetriableCloud, "anti-affinity: verify membership")
	}
	members := parseHaResources(rule.Resources)
	if _, ok := members[sid]; !ok {
		return cpierrors.WrapAs(
			cpierrors.Cloud("anti-affinity: %s missing from rule %q after recreate (concurrent drop)", sid, ruleName),
			cpierrors.TypeRetriableCloud, "anti-affinity: verify membership")
	}
	logger.Debug("anti-affinity: read-after-write verify passed",
		log.String("rule", ruleName), log.String("sid", sid))
	return nil
}

// removeAntiAffinityMembership removes a VM from any CPI-managed HA anti-affinity
// rule and deregisters its HA resource. It is keyed solely on vmid because
// delete_vm has no access to env.bosh (the group name is unknown at delete
// time): it scans every bosh-aa-* rule for the VM's sid. It edits only a rule
// that carries this Director's marker, or an unmarked rule from an older CPI,
// and leaves a rule that names another Director alone. The rules change under
// the rules lock, with a PUT that carries the read's digest, and a rule left
// with fewer than two members is deleted. The HA resource is deregistered even
// when the rule writes fail, because its purge also takes the sid out of every
// rule. All failures are returned for the caller to log; cleanup never blocks
// VM deletion.
func removeAntiAffinityMembership(ctx context.Context, deps Deps, vmid int, logger *log.Logger) error {
	svc := deps.PVE.Cluster()
	sid := haResourceSid(vmid)
	owner := deps.RequestDirectorUUID

	err := withHARulesLock(ctx, deps, vmid, logger, func() error {
		// No profile: a removal only shrinks rules, so the writer leaves the
		// feasibility checks to PVE and the cleanup never probes the cluster.
		writer := newHARuleWriter(deps, nil, logger)
		_, err := writer.Apply(ctx, "anti-affinity cleanup", func(set *ha.RuleSet) ([]ha.RuleOp, error) {
			var ops []ha.RuleOp
			for i := range set.Rules {
				r := &set.Rules[i]
				if !strings.HasPrefix(r.ID, haRuleNamePrefix) || r.Type != "" && r.Type != haRuleType {
					continue
				}
				if haOwnershipOf(r.Comment, owner) == haOwnedByOther || !r.HasResource(sid) {
					continue
				}
				rest := make([]string, 0, len(r.Resources))
				for _, s := range r.Resources {
					if s != sid {
						rest = append(rest, s)
					}
				}
				if len(rest) < 2 {
					// Empty or now single-member: the rule no longer spreads anything.
					ops = append(ops, ha.RuleOp{Kind: ha.OpDelete, Rule: ha.Rule{ID: r.ID}})
					continue
				}
				next := r.Clone()
				next.Resources, next.Errors = rest, nil
				ops = append(ops, ha.RuleOp{Kind: ha.OpUpdate, Rule: next})
			}
			return ops, nil
		})
		return err
	})
	if err != nil {
		err = fmt.Errorf("anti-affinity: remove %s from its rules: %w", sid, err)
	}

	deregisterHaResource(ctx, svc, sid, owner, logger)
	return err
}

// deregisterHaResource purges the VM's HA resource, which also removes it from
// any remaining rules. It leaves a resource whose marker names another
// Director, and treats a missing resource as done. A not-found read still sends
// the not-found-tolerant purge. When the ownership read fails for any other
// reason, it cannot tell whose the resource is, so it skips the purge rather
// than guess. Failures are only logged,
// because deregistering is best-effort cleanup and every caller logs and
// carries on.
func deregisterHaResource(ctx context.Context, svc cluster.Service, sid, owner string, logger *log.Logger) {
	got, readErr := svc.GetHaResources(ctx, sid)
	switch {
	case readErr != nil && !isHaNotFound(readErr) && !pve.IsNotFound(readErr):
		logger.Warn("anti-affinity: HA resource purge skipped because its ownership could not be read",
			log.String("sid", sid), log.Err(readErr))
		return
	case readErr == nil && got != nil && got.Comment != nil && haOwnershipOf(*got.Comment, owner) == haOwnedByOther:
		logger.Debug("anti-affinity: HA resource belongs to another Director; leaving it", log.String("sid", sid))
		return
	}
	purge := true
	if err := svc.DeleteHaResources(ctx, sid, &cluster.DeleteHaResourcesParams{Purge: &purge}); err != nil {
		if !isHaNotFound(err) {
			logger.Debug("anti-affinity: deregister HA resource (non-fatal)",
				log.String("sid", sid), log.Err(err))
		}
	}
}

// groupScan is what collectGroupMemberSids learns from the fleet.
type groupScan struct {
	// members holds the sids of the guests that carry the group's member tag,
	// plus the legacy members.
	members map[string]struct{}
	// legacy holds the sids that carry no member tag yet but whose old tags
	// (job--, deployment--, and the owner tag) prove they belong to this
	// Director and deployment. They are what the migration moves.
	legacy map[string]struct{}
	// live holds the sids of EVERY live guest regardless of tag.
	live map[string]struct{}
	// liveComplete is true only when the enumeration covered every cluster
	// member.
	liveComplete bool
}

// collectGroupMemberSids scans the cluster for QEMU guests that belong to the
// group and returns their HA sids ("vm:<vmid>") as a set, plus the sids of EVERY
// live guest regardless of tag (the union step uses the latter to drop an
// inherited rule member whose guest is provably gone), plus liveComplete,
// which is true only when the enumeration covered every cluster member.
// When it is false the live set proves nothing about absence: a guest on an
// offline-excluded member is unenumerated, not gone, so the union step must
// keep every inherited sid. The VM being created is not yet tagged, so the
// caller adds its own sid afterwards.
//
// A guest belongs to the group when it carries the member tag bosh-aa-<hash12>.
// For one release, a guest from an older CPI also belongs when it carries
// job--<group>, deployment--<deployment>, and this Director's owner tag, which
// is the evidence that it is ours and not another Director's guest of the same
// name. A guest without the owner tag, or a group whose deployment is unknown,
// is never claimed.
//
// The scan reads authoritative per-node listings, not the /cluster/resources
// index: the index lags by minutes, and a same-group VM created moments ago
// would be invisible to it, so a rule recomputed from the index could drop a
// live member. Tolerant form so an offline member does not block every
// create in the group; an enumeration failure is still returned (retriable)
// rather than answered from a partial fleet.
func collectGroupMemberSids(ctx context.Context, deps Deps, ident haIdentity) (*groupScan, error) {
	guests, excluded, err := pve.ListGuestsAuthoritativeTolerant(ctx, deps.PVE, nil)
	if err != nil {
		return nil, err
	}
	scan := &groupScan{
		members:      make(map[string]struct{}),
		legacy:       make(map[string]struct{}),
		live:         make(map[string]struct{}, len(guests)),
		liveComplete: len(excluded) == 0,
	}
	groupTag := ident.ruleName()
	legacyTags := legacyMemberTags(ident)
	for _, g := range guests {
		sid := "vm:" + strconv.Itoa(g.VMID)
		scan.live[sid] = struct{}{}
		switch {
		case tagsContain(g.Tags, groupTag):
			scan.members[sid] = struct{}{}
		case len(legacyTags) > 0 && tagsContainAll(g.Tags, legacyTags):
			scan.members[sid] = struct{}{}
			scan.legacy[sid] = struct{}{}
		}
	}
	return scan, nil
}

// legacyMemberTags returns the tags that together prove a guest without the
// member tag is this Director's member of the group, or nil when the identity
// cannot name them.
func legacyMemberTags(ident haIdentity) []string {
	owner := ownerTagFor(ident.Owner)
	if owner == "" || ident.Deployment == "" || ident.Group == "" {
		return nil
	}
	return []string{"job--" + ident.Group, "deployment--" + ident.Deployment, owner}
}

// tagsContainAll reports whether the PVE tags field holds every tag in want.
func tagsContainAll(tags string, want []string) bool {
	for _, w := range want {
		if !tagsContain(tags, w) {
			return false
		}
	}
	return true
}

// findHaRule returns the rule named ruleName, or nil when it does not exist.
func findHaRule(ctx context.Context, svc cluster.Service, ruleName string) (*haRuleEntry, error) {
	rules, err := listHaRules(ctx, svc)
	if err != nil {
		return nil, err
	}
	for i := range rules {
		if rules[i].Rule == ruleName {
			return &rules[i], nil
		}
	}
	return nil, nil
}

// listHaRules fetches and decodes every HA rule. A nil response is treated as
// an empty list (HA may be unconfigured).
func listHaRules(ctx context.Context, svc cluster.Service) ([]haRuleEntry, error) {
	resp, err := svc.ListHaRules(ctx, &cluster.ListHaRulesParams{})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, nil
	}
	out := make([]haRuleEntry, 0, len(*resp))
	for _, raw := range *resp {
		var e haRuleEntry
		if json.Unmarshal(raw, &e) != nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// parseHaResources decodes a rule's "resources" field, which PVE may serialize
// as a CSV string ("vm:100,vm:101"), a JSON array, or an object keyed by sid.
// It returns the set of resource ids.
func parseHaResources(raw json.RawMessage) map[string]struct{} {
	out := make(map[string]struct{})
	if len(raw) == 0 {
		return out
	}
	// String CSV.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		addCSVSids(out, s)
		return out
	}
	// Array of strings.
	var arr []string
	if json.Unmarshal(raw, &arr) == nil {
		for _, v := range arr {
			if v = strings.TrimSpace(v); v != "" {
				out[v] = struct{}{}
			}
		}
		return out
	}
	// Object keyed by sid.
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		for k := range obj {
			if k = strings.TrimSpace(k); k != "" {
				out[k] = struct{}{}
			}
		}
	}
	return out
}

// addCSVSids splits a comma-separated sid list into out.
func addCSVSids(out map[string]struct{}, csv string) {
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out[p] = struct{}{}
		}
	}
}

// sidsCSV renders a sid set as a deterministic (sorted) comma-separated string.
func sidsCSV(sids map[string]struct{}) string {
	keys := make([]string, 0, len(sids))
	for k := range sids {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// tagsContain reports whether the PVE tags field contains an exact match for
// want, honoring PVE's ";"/"," separators (reuses parseTagsField).
func tagsContain(tags, want string) bool {
	if tags == "" || want == "" {
		return false
	}
	for _, t := range parseTagsField(tags) {
		if t == want {
			return true
		}
	}
	return false
}

// isHaAlreadyExists reports whether err indicates the HA resource is already
// defined (an idempotent no-op for ensureAntiAffinityMembership).
func isHaAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already defined") || strings.Contains(msg, "already exists")
}

// isHaNotFound reports whether err indicates the HA rule/resource is absent
// (an idempotent no-op for delete paths).
func isHaNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such") ||
		strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "not defined") ||
		strings.Contains(msg, "404")
}
