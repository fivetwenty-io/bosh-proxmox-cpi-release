package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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

// applyAntiAffinityMembership is the create_vm entry point for HA
// anti-affinity, and it returns the name of the rule the VM was placed in, or
// "" when it was placed in none. It writes only for a VM whose HA decision
// registers it, when pve.ha.anti_affinity is not off, scheduler-soft
// anti-affinity is enabled, and the VM carries an instance-group name. The
// decision already required a profile that lets the CPI write HA state. Every
// failure is returned for the caller, which fails the call when
// propagateHARuleError says so and otherwise runs the VM without HA, so a VM
// never runs under HA without the spread rule it was meant to join.
func applyAntiAffinityMembership(
	ctx context.Context, deps Deps, d *ha.VMDecision, env map[string]any, logger *log.Logger,
) (string, error) {
	if !d.Registers() || deps.Config.HAAntiAffinityValue() == config.HARuleOff || !deps.Config.AntiAffinityEnabled() {
		return "", nil
	}
	ident := haIdentityFor(deps, env)
	if ident.Group == "" {
		return "", nil
	}
	if ident.Owner == "" {
		// A rule the CPI cannot mark as its own is a rule it could never edit
		// or clean up safely, so a request without a director_uuid writes none.
		return "", fmt.Errorf("anti-affinity: the request carries no director_uuid to mark the rule of group %q with",
			ident.Group)
	}
	return ensureAntiAffinityMembershipFor(ctx, deps, ident, d.VMID, d, logger)
}

// ensureAntiAffinityMembership adds the VM to the cluster-level negative
// resource-affinity rule of its BOSH instance group with no eligibility
// decision. See ensureAntiAffinityMembershipFor.
func ensureAntiAffinityMembership(ctx context.Context, deps Deps, ident haIdentity, vmid int, logger *log.Logger) error {
	_, err := ensureAntiAffinityMembershipFor(ctx, deps, ident, vmid, nil, logger)
	return err
}

// ensureAntiAffinityMembershipFor adds the VM to the cluster-level negative
// resource-affinity rule for its BOSH instance group, so PVE enforces spreading
// at the hypervisor level, and returns the name of the rule it was placed in.
// The VM's HA resource must already exist: create_vm stages it before any rule
// names it. Every PVE failure (including "HA not configured on this cluster")
// is returned for the caller. d is the VM's eligibility decision, or nil when
// there is none, and it lets the rule writer remove a resource this call staged
// when a repair cannot be verified.
//
// A negative resource-affinity rule is only meaningful with two or more members,
// so the rule is materialized from the live set of same-group guests plus this
// VM. When fewer than two members exist the rule is not created (and a stale
// single-member rule, if any, is removed).
//
// With pve.cluster_lock at pool, the read, check, and write run under the one
// cluster-scoped rules lock, which the writer's backoff may release before its
// first write lands. With the default off there is no lock. Either way the rule
// writer changes members with a PUT that carries the read's digest, so a write
// planned from a stale read is refused and planned again. PVE takes no digest
// on a delete, though, so without the lock a delete planned from a stale read
// can take out a member that joined after it. Every writer that deletes one of
// a group's rules without the lock therefore writes the group's rules again
// afterwards (see healAfterUnlockedDeletes), and so does this create when its
// own plan deleted a rule the group no longer needed. The repair of a
// confinement pin counts as such a writer too, because it reverts or rewrites
// a group's rule under the digest of its own read (see ha.ApplyResult), and
// the heal after it leaves out the members of the repaired pins.
func ensureAntiAffinityMembershipFor(
	ctx context.Context, deps Deps, ident haIdentity, vmid int, d *ha.VMDecision, logger *log.Logger,
) (string, error) {
	if !ident.valid() {
		return "", nil
	}

	// The version gate resolves the HA profile, which probes the whole
	// cluster when nothing has resolved it yet. That runs before the lock so
	// the probe never eats into the lock's window.
	profile, err := haRuleWriteProfile(ctx, deps)
	if err != nil {
		return "", fmt.Errorf("anti-affinity: %w", err)
	}

	var placedIn string
	var loss groupRuleLoss
	err = withHARulesLock(ctx, deps, vmid, logger, func(ctx context.Context) error {
		var lockedErr error
		placedIn, loss, lockedErr = ensureAntiAffinityMembershipLocked(ctx, deps, profile, ident, vmid, d, logger)
		return lockedErr
	})
	if loss.lost && !deps.Config.ClusterLockEnabled() {
		// This VM's own place is already written and checked, so a heal that
		// fails costs only a sibling that joined the deleted rule meanwhile,
		// and the create goes on.
		if healErr := healAntiAffinityGroup(ctx, deps, ident, haResourceSid(vmid), "", loss.confined,
			logger); healErr != nil {
			logger.Warn("anti-affinity: the group's rules could not be written again after this create deleted "+
				"one of them; a sibling that joined that rule meanwhile may be out of every rule until the next "+
				"create or delete of the group", log.String("rule", ident.ruleName()), log.Err(healErr))
		}
	}
	return placedIn, err
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
// the rules lock when enabled and after the PVE 9 gate has passed. It plans the
// group's rules from the scan and a fresh read, lets the rule writer apply the
// plan, and returns the rule the VM was placed in. It then moves this
// Director's members out of the group's unmarked rule from an older CPI. It
// also reports whether its writes can have taken a member out of one of the
// group's rules, which a delete of a rule left with fewer than two members
// does, and so does a pin repair's write, even when a later step fails.
func ensureAntiAffinityMembershipLocked(
	ctx context.Context, deps Deps, profile *ha.Profile, ident haIdentity, vmid int,
	d *ha.VMDecision, logger *log.Logger,
) (placedIn string, loss groupRuleLoss, err error) {
	sid := haResourceSid(vmid)

	// 1. Compute the full member set: existing same-group guests + this VM.
	scan, scanErr := collectGroupMemberSids(ctx, deps, ident)
	if scanErr != nil {
		// Without the member set we cannot safely write the rule. Warn and stop.
		return "", loss, fmt.Errorf("anti-affinity: scan group members: %w", scanErr)
	}

	// 2. Plan and write the group's rules. Every round reads the HA
	// resources again, because a sibling's create may stage its resource, or
	// roll it back, between two of this writer's attempts.
	planner := newGroupPlanner(ctx, deps, profile, ident, scan, logger)
	planner.sid = sid
	writer, res, err := applyCreatePlan(ctx, deps, profile, planner, d, &loss, logger)
	if err != nil {
		return "", loss, err
	}
	if planner.placedIn != "" && len(res.Wrote) > 0 {
		if err := verifyAntiAffinityMember(ctx, deps, planner.placedIn, sid, logger); err != nil {
			return "", loss, err
		}
	}

	// 3. The unmarked rule from an older CPI gives up the members of ours that
	// the hashed rules now hold. A member the overflow or the placeable cap left
	// out keeps the old rule's protection.
	if err := migrateLegacyAntiAffinityRule(ctx, deps, writer, ident,
		heldByChunks(res.Set, ident, planner.scan.legacy), vmid, &loss, logger); err != nil {
		return "", loss, err
	}
	return planner.placedIn, loss, nil
}

// newGroupPlanner returns a planner of ident's group rules over scan that
// reads the HA resources and, when it needs to, scans the group again on every
// plan. The caller sets sid, or heal, before the plan runs.
func newGroupPlanner(
	ctx context.Context, deps Deps, profile *ha.Profile, ident haIdentity, scan *groupScan, logger *log.Logger,
) *aaPlanner {
	svc := deps.PVE.Cluster()
	return &aaPlanner{
		ident: ident, scan: scan, logger: logger,
		nodes:        len(profileNodeNames(profile)),
		clusterNodes: profileNodeNames(profile),
		placeable:    placeableNodeCount(profile),
		split:        deps.Config.HAAntiAffinityOverflowValue() == config.HAOverflowSplit,
		managed: func() (map[string]struct{}, error) {
			return listHAResourceSids(ctx, svc)
		},
		rescan: func() (*groupScan, error) {
			return collectGroupMemberSids(ctx, deps, ident)
		},
	}
}

// applyGroupPlan lets a rule writer apply planner's plan, and returns the
// writer for any follow-up write. Until this Apply writes, nothing under the
// lock depends on the read it started from, so its backoff may release the
// rules lock and take it again. A create that waits for a sibling's resource
// then holds up no other rule write in the cluster. The legacy migration that
// follows an ensure keeps the lock, because it nests the older CPI's lock
// inside this one.
func applyGroupPlan(
	ctx context.Context, deps Deps, profile *ha.Profile, planner *aaPlanner, d *ha.VMDecision, logger *log.Logger,
) (*ha.RuleWriter, *ha.ApplyResult, error) {
	writer := newHARuleWriterFor(deps, profile, logger, d)
	hold := rulesLockHoldFrom(ctx)
	hold.allowYield(true)
	res, err := writer.Apply(ctx, "anti-affinity", planner.plan)
	hold.allowYield(false)
	return writer, res, err
}

// applyCreatePlan lets a rule writer apply a create's plan, and adds what each
// write leaves for a heal to loss. When the repair of a pin takes a sibling
// out of the write, because only PVE found that the sibling's join put its
// strict pin out of effect, the create plans again with that sibling held
// out, at most aaHealRounds times. In managed mode the writer fails such a
// write even though the repair put it right, and the create would fail with
// a rule that the heal after it writes right anyway. A repair that confined
// this VM itself is never planned around.
//
// Once any round's write needed a repair, the create succeeds only when the
// last read shows the rule the last plan placed this VM in holding it,
// enabled and with no error. In managed mode it otherwise returns the
// writer's refusal, which fails the call. In auto mode it otherwise returns
// placedNotInEffect's error, a plain cloud error, which the caller turns
// into a VM that runs without HA, because a VM never runs under HA without
// the spread rule it was meant to join. That is what happens to an auto
// create whose own pin PVE flags, since the repair takes its VM back out of
// the rule, and to one whose rounds run out while a repair still undoes its
// join.
func applyCreatePlan(
	ctx context.Context, deps Deps, profile *ha.Profile, planner *aaPlanner, d *ha.VMDecision,
	loss *groupRuleLoss, logger *log.Logger,
) (*ha.RuleWriter, *ha.ApplyResult, error) {
	holdOut := map[string]bool{}
	var refused error
	repaired := false
	for round := 1; ; round++ {
		writer, res, err := applyGroupPlan(ctx, deps, profile, planner, d, logger)
		loss.add(res, planner.ident)
		siblings := confinedSiblings(res, planner.sid, holdOut)
		if err != nil && !errors.Is(err, ha.ErrRuleNotInEffect) {
			return writer, res, err
		}
		repaired = repaired || res != nil && len(res.Confined) > 0
		if len(siblings) == 0 || round >= aaHealRounds {
			switch {
			case err != nil:
				return writer, res, err
			case refused != nil && !placedInEffect(res, planner):
				return writer, res, refused
			case repaired && planner.placedIn != "" && !placedInEffect(res, planner):
				return writer, res, placedNotInEffect(res, planner, round)
			}
			return writer, res, nil
		}
		if err != nil {
			refused = err
		}
		for _, s := range siblings {
			holdOut[s] = true
		}
		planner.holdOut = holdOut
		logger.Info("anti-affinity: a pin repair took siblings out of this create's write to keep their pins in "+
			"effect, so the create plans its group's rules again without them",
			log.String("rule", planner.ident.ruleName()), log.String("sid", planner.sid),
			log.String("held_out", strings.Join(siblings, ",")))
	}
}

// confinedSiblings returns the members of the pins a repair in res ran for
// that holdOut does not hold yet, or nil when there are none or when the
// repair ran for sid's own pin.
func confinedSiblings(res *ha.ApplyResult, sid string, holdOut map[string]bool) []string {
	if res == nil || slices.Contains(res.Confined, sid) {
		return nil
	}
	var out []string
	for _, s := range res.Confined {
		if !holdOut[s] && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

// placedInEffect reports whether the last read in res shows the rule the
// planner placed its VM in holding that VM, enabled and with no error.
func placedInEffect(res *ha.ApplyResult, planner *aaPlanner) bool {
	return placedState(res, planner) == ""
}

// placedState says how the last read in res shows the rule the planner placed
// its VM in, or returns "" when that rule holds the VM, enabled and with no
// error.
func placedState(res *ha.ApplyResult, planner *aaPlanner) string {
	if planner.placedIn == "" {
		return "the last plan placed " + planner.sid + " in no rule"
	}
	if res == nil || res.Set == nil {
		return fmt.Sprintf("rule %q could not be read after the last write", planner.placedIn)
	}
	r := res.Set.Find(planner.placedIn)
	switch {
	case r == nil:
		return fmt.Sprintf("rule %q is gone", planner.placedIn)
	case !r.HasResource(planner.sid):
		return fmt.Sprintf("rule %q does not hold %s", planner.placedIn, planner.sid)
	case r.Disabled:
		return fmt.Sprintf("rule %q is disabled", planner.placedIn)
	case len(r.Errors) > 0:
		return fmt.Sprintf("rule %q has errors: %s", planner.placedIn, strings.Join(r.Errors, "; "))
	}
	return ""
}

// placedNotInEffect returns the error of an auto create whose last read,
// after rounds plans and a repair in at least one of them, does not show this
// VM in effect in the rule its last plan placed it in. It describes that last
// read.
func placedNotInEffect(res *ha.ApplyResult, planner *aaPlanner, rounds int) error {
	return cpierrors.Cloud("anti-affinity: after %d plans of the group's rules and a pin repair, %s", rounds,
		placedState(res, planner))
}

// groupRuleLoss is what a write of a group's rules leaves for a heal to make
// up for. lost says that a write can have taken a member out of one of the
// group's rules: a landed delete, which PVE takes without a digest, or any
// write of a pin repair, which goes under the digest of the repair's own
// read. confined holds the members of the confinement pins the repair ran
// for, which the heal leaves where the repair did.
type groupRuleLoss struct {
	lost     bool
	confined []string
}

// groupRuleLossOf returns what res leaves for a heal of ident's group.
func groupRuleLossOf(res *ha.ApplyResult, ident haIdentity) groupRuleLoss {
	var loss groupRuleLoss
	loss.add(res, ident)
	return loss
}

// add adds what res leaves for a heal of ident's group to l.
func (l *groupRuleLoss) add(res *ha.ApplyResult, ident haIdentity) {
	if res == nil {
		return
	}
	base := ident.ruleName()
	l.confined = append(l.confined, res.Confined...)
	for i := range res.Wrote {
		op := &res.Wrote[i]
		if _, ok := aaChunkIndex(base, op.Rule.ID); ok && op.Kind == ha.OpDelete {
			l.lost = true
		}
	}
	for i := range res.Repaired {
		if _, ok := aaChunkIndex(base, res.Repaired[i].Rule.ID); ok {
			l.lost = true
		}
	}
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
//
// A scanned sibling joins a rule only once it is an HA resource. PVE refuses
// a rule that names a resource it does not manage, and the Director creates an
// instance group's VMs in parallel, so a sibling whose create has tagged its
// guest but not yet staged its resource would make every write of this create
// fail. Such a sibling is left out, and its own create adds it once its
// resource is staged. Each create stages its resource and then scans, reads
// the resources, and plans, so of any two siblings the one that staged second
// sees the other as managed. Every write carries the digest of the read it
// was planned from and keeps every member that read held whose guest is live,
// so no write drops another's member, and the group converges on every
// sibling. The scan is older than the read, so a plan that would drop a member
// whose guest the scan missed scans again first. That holds
// without the rules lock, which pve.cluster_lock turns on and which only
// serializes the writers. Without it, siblings whose writes collide on the
// digest plan again, and a writer that plans again usually finds itself
// already added by a sibling that saw it as managed.
type aaPlanner struct {
	ident haIdentity
	// sid is the member the plan is for, or "". A create's own member always
	// joins, because its create staged its resource first.
	sid string
	// heal plans the group's rules again after a delete, for no create of its
	// own. sid then joins only while it is an HA resource, like every scanned
	// member, so a member that another removal purges while the heal runs
	// drops out of the plan on the next read of the resources.
	heal bool
	// exclude is the sid of a member that delete_vm is removing, or "". The
	// plan drops it from the rules and never adds it.
	exclude string
	// holdOut holds the members of the confinement pins that a repair took
	// out of the group's writes. A heal, or a create that plans again after
	// such a repair, keeps such a member where a rule still holds it and adds
	// it to no rule, because the write that adds it would put its pin out of
	// effect again. The plan finds a conflict that the CPI's own feasibility
	// checks can see by itself (see joinConflicts), so holdOut matters only
	// for one that PVE alone reports.
	holdOut   map[string]bool
	scan      *groupScan
	logger    *log.Logger
	nodes     int
	placeable int
	split     bool
	// clusterNodes is the configured node set the feasibility checks use, or
	// nil when it is unknown, which turns off the plan's own check of a join.
	clusterNodes []string
	// managed lists the sids PVE manages as HA resources. It is read on
	// every plan. Nil treats every scanned member as managed.
	managed func() (map[string]struct{}, error)
	// rescan scans the group's members again. A plan calls it before it
	// drops a rule member whose guest the scan did not see, because the scan
	// is older than the rule read it plans from. A sibling whose guest is
	// newer than the scan can have written itself into the rule since, after
	// a digest conflict or a backoff, and only a scan taken after the read
	// can tell that member from one whose guest is gone. Nil keeps the scan.
	rescan func() (*groupScan, error)

	// placedIn is the rule this VM is in after the last plan, or "".
	placedIn string
	logged   bool
	// managedNow is the last plan's read of managed, or nil.
	managedNow map[string]struct{}
	// deferred holds the siblings already logged as left out for now.
	deferred map[string]bool
	// conflicted holds the members already logged as left out because their
	// join would bring on a feasibility conflict.
	conflicted map[string]bool
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
	if p.managed != nil {
		managed, err := p.managed()
		if err != nil {
			return nil, fmt.Errorf("anti-affinity: list HA resources: %w", err)
		}
		p.managedNow = managed
	}
	existing, maxIndex, err := p.existingChunks(set)
	if err != nil {
		return nil, err
	}
	if p.rescan != nil && p.wouldDropUnseen(existing) {
		scan, err := p.rescan()
		if err != nil {
			return nil, fmt.Errorf("anti-affinity: scan group members again: %w", err)
		}
		p.scan = scan
	}
	desired, assigned := p.keptMembers(existing, maxIndex)
	if p.skipOversized(assigned) {
		return nil, nil
	}
	maxIndex = p.assignPending(set, existing, desired, assigned, maxIndex)
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
	if p.sid != "" && p.sid != p.exclude && !assigned[p.sid] {
		total++
	}
	for s := range p.scan.members {
		if s != p.sid && s != p.exclude && !assigned[s] {
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

// wouldDropUnseen reports whether keptMembers would drop a member of an
// existing rule because the scan saw no live guest for it. The excluded
// member goes whatever the scan says, so it never calls for a rescan.
func (p *aaPlanner) wouldDropUnseen(existing map[int]*ha.Rule) bool {
	if !p.scan.liveComplete {
		return false
	}
	for _, r := range existing {
		for _, s := range r.Resources {
			if s == p.exclude {
				continue
			}
			if _, ok := p.scan.live[s]; !ok {
				return true
			}
		}
	}
	return false
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
			if s == p.exclude {
				continue
			}
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
// fewer members than the configured nodes and the placeable nodes. A scanned
// member that is not an HA resource yet waits for its own create to add it.
// This VM is always pending, because create_vm stages its resource first,
// except in a heal, where it joins only while it is managed.
//
// Every other member joins only when the CPI's feasibility checks find that
// its join brings on no conflict that the rule set without it lacks, such as
// a second member whose strict pin allows only the nodes another member's pin
// already needs. A rule that conflicts already takes no such member either,
// because the writer refuses to write it whether or not the join is the
// cause. The write that adds such a member would be refused before it
// is sent, or would put a strict pin out of effect, so the plan leaves the
// member out and writes the rest. Every writer of the group plans the same
// way from the rules it reads, so none of them puts the member back while the
// conflict lasts, and the first plan after the pin goes away or widens adds
// it. This VM's own join is never held out, so a create whose own pin
// conflicts still fails or logs the refusal the way its mode says.
//
// A member whose join conflicts with one rule is tried in each later rule
// with room, which split mode gives a group with more members than nodes,
// and it joins the first one where its join brings on no conflict. It stays
// out only when every rule with room refuses it. No new rule opens for it
// then, because a rule opens only once every rule is full, and every later
// plan tries the member again. A rule that holds members comes before an
// index whose rule a delete left out, and a member that a rule refused never
// fills such a gap, because alone there it would be in no rule.
func (p *aaPlanner) assignPending(
	set *ha.RuleSet, existing map[int]*ha.Rule, desired map[int][]string, assigned map[string]bool, maxIndex int,
) int {
	pending := map[string]struct{}{}
	candidates := maps.Clone(p.scan.members)
	if p.sid != "" {
		if candidates == nil {
			candidates = map[string]struct{}{}
		}
		candidates[p.sid] = struct{}{}
	}
	for s := range candidates {
		if s == p.exclude || p.holdOut[s] && !assigned[s] {
			continue
		}
		if (p.heal || s != p.sid) && !assigned[s] && !p.isManaged(s) {
			p.logDeferred(s)
			continue
		}
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
		fit := p.fitMember(set, existing, desired, maxIndex, room, s)
		target := fit.target
		if target == 0 && !fit.roomy && (p.split || maxIndex == 0) {
			maxIndex++
			target = maxIndex
		}
		if target == 0 {
			if len(fit.refused) > 0 {
				p.logConflicted(s, strings.Join(fit.refused, ","), fit.bad)
			} else {
				p.logOverflow(s, len(desired[1]))
			}
			continue
		}
		assigned[s] = true
		desired[target] = append(desired[target], s)
	}
	return maxIndex
}

// memberFit is where a pending member fits among the group's rules.
type memberFit struct {
	// target is the first rule with room that s can join, or 0.
	target int
	// roomy says that at least one rule had room.
	roomy bool
	// refused names the rules with room that s's join would make conflict,
	// and bad holds the conflicts those joins bring on.
	refused []string
	bad     map[string][]string
}

// fitMember tries s in each of the group's rules that holds members and
// fewer than room of them, in index order, and returns the first one where
// its join brings on no conflict. The create's own VM is never checked,
// except in a heal. Only when no rule with members takes s does it try an
// index the plan has given no members yet, such as one whose rule a delete
// left out, because s alone there ends up in no rule. An index like that
// takes s only when no rule refused it, the way a new rule would, so a member
// whose join conflicts stays out and is logged rather than fill the gap
// alone.
func (p *aaPlanner) fitMember(
	set *ha.RuleSet, existing map[int]*ha.Rule, desired map[int][]string, maxIndex, room int, s string,
) memberFit {
	var fit memberFit
	for n := 1; n <= maxIndex; n++ {
		if len(desired[n]) == 0 || len(desired[n]) >= room {
			continue
		}
		fit.roomy = true
		if p.heal || s != p.sid {
			if conflicts := p.joinConflicts(set, existing[n], n, desired[n], s); len(conflicts) > 0 {
				fit.refused = append(fit.refused, aaChunkName(p.ident.ruleName(), n))
				if fit.bad == nil {
					fit.bad = map[string][]string{}
				}
				maps.Copy(fit.bad, conflicts)
				continue
			}
		}
		fit.target = n
		return fit
	}
	for n := 1; n <= maxIndex; n++ {
		if len(desired[n]) != 0 || room <= 0 {
			continue
		}
		fit.roomy = true
		if len(fit.refused) == 0 {
			fit.target = n
			return fit
		}
	}
	return fit
}

// joinConflicts returns the feasibility conflicts that adding s to the
// group's n-th rule, which the plan has given members so far, would leave in
// the rules, or nil when there are none. It checks the rules the way the
// writer's check before a send does, with the rule written as chunkOps writes
// it and with the members' pins flattened. Like that check, it counts every
// conflict on the n-th rule itself, even one the rule had before the join,
// because the writer refuses to write a rule that conflicts at all. On every
// other rule it counts only a conflict that the rule set without s lacks.
// With the configured nodes unknown it checks nothing, as the writer does.
func (p *aaPlanner) joinConflicts(
	set *ha.RuleSet, cur *ha.Rule, n int, members []string, s string,
) map[string][]string {
	if len(p.clusterNodes) == 0 || len(members) == 0 || set == nil {
		return nil
	}
	name := aaChunkName(p.ident.ruleName(), n)
	with := append(slices.Clone(members), s)
	return ha.IntroducedConflicts(p.chunkCandidate(set, cur, name, members), p.chunkCandidate(set, cur, name, with),
		[]string{name}, p.clusterNodes)
}

// chunkCandidate returns set's rules with the rule name holding members, the
// way chunkOps writes it, and with the members' pins flattened. A rule left
// with fewer than two members is left out, because chunkOps deletes it.
func (p *aaPlanner) chunkCandidate(set *ha.RuleSet, cur *ha.Rule, name string, members []string) []ha.Rule {
	members = slices.Sorted(slices.Values(members))
	flat := map[string]ha.Rule{}
	ops := flattenMemberPins(set, members, p.ident.Owner)
	for i := range ops {
		flat[ops[i].Rule.ID] = ops[i].Rule
	}
	out := make([]ha.Rule, 0, len(set.Rules)+1)
	for i := range set.Rules {
		r := set.Rules[i]
		if r.ID == name {
			continue
		}
		if f, ok := flat[r.ID]; ok {
			r = f
		}
		out = append(out, r)
	}
	if len(members) < 2 {
		return out
	}
	chunk := ha.Rule{ID: name, Type: haRuleType, Affinity: haRuleAffinity, Resources: members,
		Comment: p.ident.marker()}
	if cur != nil {
		chunk = cur.Clone()
		chunk.Resources, chunk.Affinity, chunk.Errors = members, haRuleAffinity, nil
	}
	return append(out, chunk)
}

// logConflicted says once per member that it stays out of the group's rules
// while its join would bring on the conflicts in bad.
func (p *aaPlanner) logConflicted(sid, rule string, bad map[string][]string) {
	if p.conflicted[sid] {
		return
	}
	if p.conflicted == nil {
		p.conflicted = map[string]bool{}
	}
	p.conflicted[sid] = true
	p.logger.Warn("anti-affinity: member left out of the group's rule, because adding it would make the rules "+
		"conflict, which would refuse the write or put a strict pin out of effect; a later write of the group "+
		"adds it once the conflict is gone",
		log.String("sid", sid), log.String("rule", rule),
		log.String("conflicting", strings.Join(slices.Sorted(maps.Keys(bad)), ",")), log.String("joining", p.sid))
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

// isManaged reports whether the last plan's read lists sid as an HA resource.
// Without a read every member counts as managed.
func (p *aaPlanner) isManaged(sid string) bool {
	if p.managedNow == nil {
		return true
	}
	_, ok := p.managedNow[sid]
	return ok
}

// logDeferred says once per sibling that it stays out of the rule until its
// own create adds it.
func (p *aaPlanner) logDeferred(sid string) {
	if p.deferred[sid] {
		return
	}
	if p.deferred == nil {
		p.deferred = map[string]bool{}
	}
	p.deferred[sid] = true
	p.logger.Info("anti-affinity: sibling is not an HA resource yet, so its own create adds it to the rule",
		log.String("sid", sid), log.String("rule", p.ident.ruleName()), log.String("joining", p.sid))
}

// listHAResourceSids returns the sids of every HA resource PVE manages. A
// resource staged as ignored is managed too, and PVE accepts it in a rule.
func listHAResourceSids(ctx context.Context, svc cluster.Service) (map[string]struct{}, error) {
	rows, err := svc.ListHaResources(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := map[string]struct{}{}
	if rows == nil {
		return out, nil
	}
	for _, raw := range *rows {
		var row struct {
			SID string `json:"sid"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, fmt.Errorf("decode an HA resource row: %w", err)
		}
		if row.SID != "" {
			out[row.SID] = struct{}{}
		}
	}
	return out, nil
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
// left, so the plan leaves a sibling whose join that pin would make conflict
// out of the rule, and the writer's check refuses the create's own join.
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

// legacyRuleMigrationPermitKey keys the context value that carries a legacy
// rule migration's permit. Only migrateLegacyAntiAffinityRule sets it.
type legacyRuleMigrationPermitKey struct{}

// withLegacyRuleMigrationPermit returns a context that permits a managed
// allocation guard to admit an update or delete of the unmarked rule named
// rule. The migration sets it after it has proved which of the rule's members
// belong to this Director through their owner, deployment, and job tags, and
// while it holds the lock an older CPI writes that rule under.
func withLegacyRuleMigrationPermit(ctx context.Context, rule string) context.Context {
	return context.WithValue(ctx, legacyRuleMigrationPermitKey{}, rule)
}

// legacyRuleMigrationPermitted reports whether ctx carries the legacy rule
// migration's permit for exactly the rule named rule.
func legacyRuleMigrationPermitted(ctx context.Context, rule string) bool {
	permitted, ok := ctx.Value(legacyRuleMigrationPermitKey{}).(string)
	return ok && permitted != "" && permitted == rule
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
// anything, so it is deleted and nothing is created in its place. A pin repair
// after the migration's write that changes one of the group's rules is added
// to loss.
func migrateLegacyAntiAffinityRule(
	ctx context.Context, deps Deps, writer *ha.RuleWriter, ident haIdentity,
	mine map[string]struct{}, vmid int, loss *groupRuleLoss, logger *log.Logger,
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
	// The permit tells a managed allocation guard that this write is the
	// migration's own, so it admits an edit of this one unmarked rule.
	ctx = withLegacyRuleMigrationPermit(ctx, name)
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
	loss.add(res, ident)
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
//
// PVE takes no digest on a rule delete, so a delete planned from a read that a
// sibling's create has since grown would take the sibling out with the rule.
// The rules lock rules that out. Without it, a removal that deleted one of
// this Director's group rules writes the group's rules again once the purge is
// done (see healAfterUnlockedDeletes), so a sibling that joined the rule
// between the read and the delete gets its place back.
func removeAntiAffinityMembership(ctx context.Context, deps Deps, vmid int, logger *log.Logger) error {
	svc := deps.PVE.Cluster()
	sid := haResourceSid(vmid)
	owner := deps.RequestDirectorUUID
	var deleted map[string]string
	var confined []string

	err := withHARulesLock(ctx, deps, vmid, logger, func(ctx context.Context) error {
		// No profile: a removal only shrinks rules, so the writer leaves the
		// feasibility checks to PVE and the cleanup never probes the cluster.
		writer := newHARuleWriter(deps, nil, logger)
		comments := map[string]string{}
		res, err := writer.Apply(ctx, "anti-affinity cleanup", func(set *ha.RuleSet) ([]ha.RuleOp, error) {
			return planAntiAffinityRemoval(set, sid, owner, comments), nil
		})
		deleted, confined = writtenRuleDeletes(res, comments), confinedBy(res)
		return err
	})
	if err != nil {
		err = fmt.Errorf("anti-affinity: remove %s from its rules: %w", sid, err)
	}

	deregisterHaResource(ctx, svc, sid, owner, logger)
	return errors.Join(err, healAfterUnlockedDeletes(ctx, deps, deleted, owner, sid, confined, logger))
}

// planAntiAffinityRemoval plans the writes that take sid out of every
// bosh-aa-* rule that this Director or an older CPI owns. A rule left with
// fewer than two members is deleted, and comments records the comment of each
// rule a delete is planned for, which the delete itself does not keep.
func planAntiAffinityRemoval(set *ha.RuleSet, sid, owner string, comments map[string]string) []ha.RuleOp {
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
			comments[r.ID] = r.Comment
			continue
		}
		next := r.Clone()
		next.Resources, next.Errors = rest, nil
		ops = append(ops, ha.RuleOp{Kind: ha.OpUpdate, Rule: next})
	}
	return ops
}

// writtenRuleDeletes returns the comment of each rule whose planned delete
// res reports as written, keyed by the rule ID. comments holds the comment of
// each rule a delete was planned for. Every anti-affinity rule that a pin
// repair in res deleted or rewrote counts as well, with the comment the
// repair reports, because the repair writes under the digest of its own read
// and can take out a member as a delete does. It returns nil when none was.
func writtenRuleDeletes(res *ha.ApplyResult, comments map[string]string) map[string]string {
	if res == nil {
		return nil
	}
	var deleted map[string]string
	add := func(id, comment string) {
		if deleted == nil {
			deleted = map[string]string{}
		}
		deleted[id] = comment
	}
	for i := range res.Wrote {
		op := &res.Wrote[i]
		if c, ok := comments[op.Rule.ID]; ok && op.Kind == ha.OpDelete {
			add(op.Rule.ID, c)
		}
	}
	for i := range res.Repaired {
		op := &res.Repaired[i]
		if strings.HasPrefix(op.Rule.ID, haRuleNamePrefix) {
			add(op.Rule.ID, op.Rule.Comment)
		}
	}
	return deleted
}

// confinedBy returns the members of the confinement pins a repair in res ran
// for, or nil.
func confinedBy(res *ha.ApplyResult) []string {
	if res == nil {
		return nil
	}
	return res.Confined
}

// healAfterRepair writes the rules again of every group whose rule a pin
// repair in res deleted or rewrote without the rules lock, for a writer that
// plans no anti-affinity rule of its own, such as a pin write. The writer's
// own write is done by then, so a heal that fails is only logged.
func healAfterRepair(ctx context.Context, deps Deps, res *ha.ApplyResult, logger *log.Logger) {
	deleted := writtenRuleDeletes(res, nil)
	err := healAfterUnlockedDeletes(ctx, deps, deleted, deps.RequestDirectorUUID, "", confinedBy(res), logger)
	if err != nil {
		logger.Warn("anti-affinity: the group's rules could not be written again after a pin repair changed one "+
			"of them; a sibling that joined that rule meanwhile may be out of every rule until the next create "+
			"or delete of the group", log.Err(err))
	}
}

// deletedGroupRules returns the groups whose rules a removal deleted, from the
// rule IDs and the comments they carried. Only a rule whose marker names this
// Director, and whose ID is a rule of the group the marker names, counts.
func deletedGroupRules(deleted map[string]string, owner string) []haIdentity {
	seen := map[string]bool{}
	var out []haIdentity
	ids := make([]string, 0, len(deleted))
	for id := range deleted {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		comment := deleted[id]
		if haOwnershipOf(comment, owner) != haOwnedByUs {
			continue
		}
		m, ok := parseHaMarker(comment)
		if !ok {
			continue
		}
		ident := haIdentity{Owner: owner, Deployment: m.Deployment, Group: m.Group}
		if _, ok := aaChunkIndex(ident.ruleName(), id); !ok || !ident.valid() || seen[ident.ruleName()] {
			continue
		}
		seen[ident.ruleName()] = true
		out = append(out, ident)
	}
	return out
}

// healAfterUnlockedDeletes writes the rules of every group again whose rule a
// writer deleted without the rules lock. deleted maps each deleted rule's ID
// to the comment it carried, and removed is the sid of the VM that leaves its
// rules, which the heal keeps out. confined holds the members of the pins a
// repair ran for, which the heal adds to no rule. PVE takes no digest on a rule delete, so a
// delete planned from a read that a sibling's create has since grown takes the
// sibling out with the rule. The heal plans from a read taken after the
// delete, which puts every managed member back. With the rules lock on, no
// writer can change a rule between another's read and its delete, so there is
// nothing to heal.
func healAfterUnlockedDeletes(
	ctx context.Context, deps Deps, deleted map[string]string, owner, removed string, confined []string,
	logger *log.Logger,
) error {
	if len(deleted) == 0 || deps.Config.ClusterLockEnabled() {
		return nil
	}
	var err error
	for _, ident := range deletedGroupRules(deleted, owner) {
		if healErr := healAntiAffinityGroup(ctx, deps, ident, "", removed, confined, logger); healErr != nil {
			err = errors.Join(err, fmt.Errorf("anti-affinity: write the rules of group %s again after a "+
				"delete: %w", ident.ruleName(), healErr))
		}
	}
	return err
}

// aaHealRounds bounds the plans a heal makes when its own writes delete one
// of the group's rules again, which a heal answers with another heal.
const aaHealRounds = 4

// healAntiAffinityGroup writes the rules of ident's group again after a
// delete without the rules lock. It plans for no create of its own: every
// managed member the scan finds joins a rule, and the member set is read again
// on every plan, so a member that another removal purges meanwhile drops out
// instead of making every write fail. self is the sid of a create that
// deleted one of its group's rules, which joins on the same terms, or "", and
// removed is the sid of a VM that leaves the group, which stays out. confined
// holds the members of the pins a repair ran for, which the heal keeps where
// a rule holds them and adds to no rule. It skips the membership check and
// the legacy migration, because it places no member of its own. A heal whose
// own writes deleted a rule, or whose own write a pin repair undid, heals
// again, at most aaHealRounds times, and each round leaves out the members of
// the pins the rounds before it repaired. That holds in managed mode too,
// where the writer fails a write whose repair confined a member. The heal
// then goes on to the next round with that member held out, and it returns
// the writer's error only when the repair confined no member that was not
// held out already, or when the rounds run out.
func healAntiAffinityGroup(
	ctx context.Context, deps Deps, ident haIdentity, self, removed string, confined []string, logger *log.Logger,
) error {
	profile, err := haRuleWriteProfile(ctx, deps)
	if err != nil {
		return err
	}
	holdOut := map[string]bool{}
	for _, s := range confined {
		holdOut[s] = true
	}
	var notInEffect error
	for range aaHealRounds {
		scan, err := collectGroupMemberSids(ctx, deps, ident)
		if err != nil {
			return fmt.Errorf("scan group members: %w", err)
		}
		planner := newGroupPlanner(ctx, deps, profile, ident, scan, logger)
		planner.sid, planner.heal, planner.exclude, planner.holdOut = self, true, removed, holdOut
		_, res, err := applyGroupPlan(ctx, deps, profile, planner, nil, logger)
		loss := groupRuleLossOf(res, ident)
		grew := false
		for _, s := range loss.confined {
			grew = grew || !holdOut[s]
			holdOut[s] = true
		}
		if err != nil {
			if !errors.Is(err, ha.ErrRuleNotInEffect) || !grew {
				return err
			}
			notInEffect = err
			continue
		}
		notInEffect = nil
		if !loss.lost {
			return nil
		}
	}
	if notInEffect != nil {
		return notInEffect
	}
	return cpierrors.Retriable("anti-affinity: the rules of group %s lost a rule to a delete in each of %d heals",
		ident.ruleName(), aaHealRounds)
}

// pruneGroupAntiAffinityRules deletes every anti-affinity rule of ident's
// group that this Director owns and that holds fewer than two members. A VM
// leaving HA through the purge of its resource drops out of its rule, and a
// rule left with one member spreads nothing, which some PVE versions keep as a
// rule rather than removing it. The group's rules are its hashed rule and that
// rule's chunks. A rule another Director owns, or one with no marker, is left
// alone. It runs under the HA rules lock and reports the writer's error.
// Without the lock, a prune that deleted a rule writes the group's rules again
// afterwards, with the VM kept out, as a removal does.
func pruneGroupAntiAffinityRules(ctx context.Context, deps Deps, ident haIdentity, vmid int, logger *log.Logger) error {
	if !ident.valid() {
		return nil
	}
	base := ident.ruleName()
	var deleted map[string]string
	var confined []string
	err := withHARulesLock(ctx, deps, vmid, logger, func(ctx context.Context) error {
		// No profile: a deletion only shrinks rules, as in
		// removeAntiAffinityMembership.
		writer := newHARuleWriter(deps, nil, logger)
		comments := map[string]string{}
		res, err := writer.Apply(ctx, "anti-affinity prune", func(set *ha.RuleSet) ([]ha.RuleOp, error) {
			var ops []ha.RuleOp
			for i := range set.Rules {
				r := &set.Rules[i]
				if _, ok := aaChunkIndex(base, r.ID); !ok || r.Type != haRuleType {
					continue
				}
				if haOwnershipOf(r.Comment, ident.Owner) != haOwnedByUs || len(r.Resources) >= 2 {
					continue
				}
				ops = append(ops, ha.RuleOp{Kind: ha.OpDelete, Rule: ha.Rule{ID: r.ID}})
				comments[r.ID] = r.Comment
			}
			return ops, nil
		})
		deleted, confined = writtenRuleDeletes(res, comments), confinedBy(res)
		return err
	})
	if err != nil {
		err = fmt.Errorf("anti-affinity: prune the rules of group %s: %w", base, err)
	}
	return errors.Join(err, healAfterUnlockedDeletes(ctx, deps, deleted, ident.Owner, haResourceSid(vmid), confined,
		logger))
}

// deregisterHaResource purges the VM's HA resource, which also removes it from
// any remaining rules, but only when the resource carries this Director's
// ownership marker (see removeOwnedHaResource). A resource with no marker, as
// an operator's own or an older CPI's carries, and one whose marker names
// another Director are left in place, and a missing resource is done. When the
// ownership read fails, it cannot tell whose the resource is, so it skips the
// purge rather than guess. Failures are only logged, because deregistering is
// best-effort cleanup and every caller logs and carries on.
func deregisterHaResource(ctx context.Context, svc cluster.Service, sid, owner string, logger *log.Logger) {
	if err := removeOwnedHaResource(ctx, svc, sid, owner); err != nil {
		logger.Warn("anti-affinity: HA resource left in place (non-fatal)", log.String("sid", sid), log.Err(err))
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
// an empty list (HA may be unconfigured). A row that does not decode fails the
// read, the way ha.ParseRuleList does, because a skipped row could be the rule
// the caller is looking for.
func listHaRules(ctx context.Context, svc cluster.Service) ([]haRuleEntry, error) {
	resp, err := svc.ListHaRules(ctx, &cluster.ListHaRulesParams{})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, nil
	}
	out := make([]haRuleEntry, 0, len(*resp))
	for i, raw := range *resp {
		var e haRuleEntry
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("HA rules list row %d does not parse: %w", i, err)
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
