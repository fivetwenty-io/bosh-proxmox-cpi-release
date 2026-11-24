package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/aapattern"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/spreadclaim"
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

// applyAntiAffinityMembership puts a VM into the rules of its anti-affinity
// group, and it returns the name of the rule the VM was placed in, or "" when
// it was placed in none. set_vm_metadata calls it, because the rules choose
// their members by instance index and the Director names the index there. It
// writes only when pve.ha.mode is not off, pve.ha.anti_affinity is not off,
// scheduler-soft anti-affinity is enabled, and the identity names a group.
// Every failure is returned for the caller.
func applyAntiAffinityMembership(
	ctx context.Context, deps Deps, ident haIdentity, vmid int, opts aaJoinOpts, logger *log.Logger,
) (string, error) {
	if !antiAffinityJoinEnabled(deps.Config) || ident.Group == "" {
		return "", nil
	}
	if ident.Owner == "" {
		// A rule the CPI cannot mark as its own is a rule it could never edit
		// or clean up safely, so a request without a director_uuid writes none.
		return "", fmt.Errorf("anti-affinity: the request carries no director_uuid to mark the rule of group %q with",
			ident.Group)
	}
	return ensureAntiAffinityMembershipWith(ctx, deps, ident, vmid, nil, opts, logger)
}

// antiAffinityJoinEnabled reports whether the settings let a VM join its
// group's anti-affinity rules.
func antiAffinityJoinEnabled(cfg *config.CPIConfig) bool {
	return cfg.HAWritesEnabled() && cfg.HAAntiAffinityValue() != config.HARuleOff && cfg.AntiAffinityEnabled()
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
	return ensureAntiAffinityMembershipWith(ctx, deps, ident, vmid, d, aaJoinOpts{index: -1}, logger)
}

// aaJoinOpts tunes how a VM joins its group's rules.
type aaJoinOpts struct {
	// index is the VM's instance index, or -1 to read it from the VM's
	// index tag or name.
	index int
	// checkSelf puts the VM's own join through the feasibility checks every
	// other member's join gets, so a join that would make the rules conflict
	// leaves the VM out instead of sending a write PVE refuses.
	checkSelf bool
}

// ensureAntiAffinityMembershipWith is ensureAntiAffinityMembershipFor with
// the join tuned by opts.
func ensureAntiAffinityMembershipWith(
	ctx context.Context, deps Deps, ident haIdentity, vmid int, d *ha.VMDecision, opts aaJoinOpts,
	logger *log.Logger,
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
		placedIn, loss, lockedErr = ensureAntiAffinityMembershipLocked(ctx, deps, profile, ident, vmid, d, opts,
			logger)
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
// detached and bounded for the same reason as withVMIDLock's: a canceled
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
// rule under. opts are added to the lock's own options.
func acquireAntiAffinityLock(
	ctx context.Context, deps Deps, key string, vmid int, opts ...pve.ClusterLockOption,
) (*pve.ClusterLockHandle, error) {
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
	opts = append([]pve.ClusterLockOption{pve.WithCreateGrace()}, opts...)
	return pve.AcquireClusterLock(ctx, poolSvc, antiAffinityLockPrefix+key, owner, ttl, timeout, opts...)
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
	d *ha.VMDecision, opts aaJoinOpts, logger *log.Logger,
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
	planner.sid, planner.index, planner.checkSelf = sid, opts.index, opts.checkSelf
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
	// out keeps the old rule's protection. A group whose pattern is off wants
	// no rule for any member, so every member of ours leaves the old rule.
	mine := heldByChunks(res.Set, ident, planner.scan.legacy)
	if planner.lastIn.Pattern.Off {
		mine = planner.scan.legacy
	}
	if err := migrateLegacyAntiAffinityRule(ctx, deps, writer, ident, mine, vmid, &loss, logger); err != nil {
		return "", loss, err
	}

	// 4. A member the plan took out of its last rule gets back the
	// priorities its join flattened.
	restoreGroupPins(ctx, deps, profile, planner, res.Set, logger)
	return planner.placedIn, loss, nil
}

// newGroupPlanner returns a planner of ident's group rules over scan that
// reads the HA resources and, when it needs to, scans the group again on every
// plan. The caller sets sid, or heal, before the plan runs.
func newGroupPlanner(
	ctx context.Context, deps Deps, profile *ha.Profile, ident haIdentity, scan *groupScan, logger *log.Logger,
) *aaPlanner {
	svc := deps.PVE.Cluster()
	p := newAAPlanner(deps.Config, profile, ident, scan, logger)
	p.managed = func() (map[string]string, error) {
		return listHAResources(ctx, svc)
	}
	p.rescan = func() (*groupScan, error) {
		return collectGroupMemberSids(ctx, deps, ident)
	}
	p.recordHolds = func(e aaResourceHoldEdit) error {
		return recordResourceHolds(ctx, svc, ident.Owner, e)
	}
	p.liveMoves = groupLiveMoves(ctx, deps, ident, logger)
	p.ownMoves = ownColoMoves(ctx)
	return p
}

// groupLiveMoves returns the read of the live claims in the spread claim
// record of ident's group, or nil when the CPI keeps no such record. A read
// takes one turn with the record and records nothing.
func groupLiveMoves(
	ctx context.Context, deps Deps, ident haIdentity, logger *log.Logger,
) func(node string) ([]spreadclaim.Live, error) {
	if deps.Config == nil || !deps.Config.PlacementEnabled() || deps.SpreadClaims == nil || !ident.valid() ||
		ident.Owner == "" {
		return nil
	}
	var store *spreadclaim.Store
	return func(node string) ([]spreadclaim.Live, error) {
		h := deps.coloMoveHooks(logger)
		if store == nil {
			if store = deps.SpreadClaims(deps.Config, h.stamp(ctx, node), logger); store == nil {
				return nil, nil
			}
		}
		claim, err := store.NewClaim(ident.ruleName(), h.now().Add(time.Minute))
		if err != nil {
			return nil, err
		}
		var live []spreadclaim.Live
		_, err = claim.PickLive(ctx, 0, func(l []spreadclaim.Live) string {
			live = l
			return ""
		})
		return live, err
	}
}

// ownColoMovesKey is the context key of the VMs whose moves off a partner's
// node the call claimed.
type ownColoMovesKey struct{}

// withOwnColoMoves returns ctx carrying the VMs whose moves the call claimed,
// so the call's own plans write their rules while every other call's plans
// hold them out until the move is done.
func withOwnColoMoves(ctx context.Context, own map[int]bool) context.Context {
	if len(own) == 0 {
		return ctx
	}
	return context.WithValue(ctx, ownColoMovesKey{}, own)
}

// ownColoMoves returns the VMs withOwnColoMoves put in ctx, or nil.
func ownColoMoves(ctx context.Context) map[int]bool {
	own, _ := ctx.Value(ownColoMovesKey{}).(map[int]bool)
	return own
}

// recordResourceHolds writes a change of the holds sid's HA resource records.
// A resource that is missing, or whose comment no longer carries owner's
// marker, takes no write, and that is an error too, because the plan must not
// count a hold as recorded where nothing records it. A comment that records
// the change already needs no write.
func recordResourceHolds(ctx context.Context, svc cluster.Service, owner string, e aaResourceHoldEdit) error {
	read := false
	_, err := ha.EditResourceComment(ctx, svc, e.sid, func(comment string) bool {
		return haOwnershipOf(comment, owner) == haOwnedByUs
	}, func(comment string) string {
		read = true
		return e.apply(comment)
	})
	switch {
	case err != nil:
		return err
	case !read:
		return cpierrors.Retriable("anti-affinity: the HA resource %s is gone or no longer carries this Director's "+
			"marker, so it records no hold", e.sid)
	}
	return nil
}

// newAAPlanner returns a planner of ident's group rules over scan that reads
// no PVE state of its own. Its caller sets managed and rescan when the plan
// should read them.
func newAAPlanner(
	cfg *config.CPIConfig, profile *ha.Profile, ident haIdentity, scan *groupScan, logger *log.Logger,
) *aaPlanner {
	return &aaPlanner{
		ident: ident, scan: scan, logger: logger, index: -1, excludeIndex: -1,
		clusterNodes: profileNodeNames(profile),
		topo:         newAATopology(cfg, profile),
		split:        cfg.HAAntiAffinityOverflowValue() == config.HAOverflowSplit,
		pattern:      cfg.HAAntiAffinityPatternValue(),
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
// enabled and with no error. Otherwise it returns placedNotInEffect's error.
// In managed mode that error wraps the writer's last refusal, so it fails
// the call. In auto mode it is a plain cloud error, which the caller turns
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
		if err != nil {
			refused = err
		}
		repaired = repaired || res != nil && len(res.Confined) > 0
		if len(siblings) == 0 || round >= aaHealRounds {
			switch {
			case err != nil:
				return writer, res, err
			case (refused != nil || repaired && planner.placedIn != "") && !placedInEffect(res, planner):
				return writer, res, placedNotInEffect(res, planner, round, refused)
			}
			return writer, res, nil
		}
		for _, s := range siblings {
			holdOut[s] = true
		}
		planner.holdOut = holdOut
		planner.logHeldOut(siblings)
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

// placedNotInEffect returns the error of a create whose last read, after
// rounds plans and a repair in at least one of them, does not show this VM in
// effect in the rule its last plan placed it in. It describes that last read,
// and it wraps refused, the writer's last refusal, when there was one.
func placedNotInEffect(res *ha.ApplyResult, planner *aaPlanner, rounds int, refused error) error {
	msg := fmt.Sprintf("anti-affinity: after %d plans of the group's rules and a pin repair, %s", rounds,
		placedState(res, planner))
	if refused != nil {
		return cpierrors.WrapAs(refused, cpierrors.TypeCloud, msg)
	}
	return cpierrors.Cloud("%s", msg)
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
		if aapattern.IsGroupRule(base, op.Rule.ID) && op.Kind == ha.OpDelete {
			l.lost = true
		}
	}
	for i := range res.Repaired {
		if aapattern.IsGroupRule(base, res.Repaired[i].Rule.ID) {
			l.lost = true
		}
	}
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
		if !aapattern.IsGroupRule(base, r.ID) || r.Disabled {
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

// listHAResources returns the comment of every HA resource PVE manages, keyed
// by its sid. A resource staged as ignored is managed too, and PVE accepts it
// in a rule.
func listHAResources(ctx context.Context, svc cluster.Service) (map[string]string, error) {
	rows, err := svc.ListHaResources(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if rows == nil {
		return out, nil
	}
	for _, raw := range *rows {
		var row struct {
			SID     string `json:"sid"`
			Comment string `json:"comment"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, fmt.Errorf("decode an HA resource row: %w", err)
		}
		if row.SID != "" {
			out[row.SID] = row.Comment
		}
	}
	return out, nil
}

// flattenMemberPins returns the writes that give every member's CPI pin a
// single priority. PVE refuses a node-affinity rule with several priorities
// for a resource that is in a resource-affinity rule, so the pins go flat
// before the member joins. A pin that carries another Director's marker is
// left, so the plan leaves a sibling whose join that pin would make conflict
// out of the rule, and the writer's check refuses the create's own join.
//
// A soft pin of this Director's that names only some of the configured nodes
// is also rewritten to name every one of them. PVE gives the nodes such a pin
// leaves out a lower priority, and the CRM applies the group's rule only to
// the nodes of the highest priority, so the pin would act as a strict one when
// a sibling runs on its node (see ha.SoftPinNamesSomeNodes). An unmarked pin
// is only flattened, because the CPI cannot tell what an older one meant by it.
//
// Before either rewrite of a pin of this Director's, the pin's comment records
// the nodes and priorities it had (see ha.PinRestoreKey), so the restore can
// give them back once the member leaves its last resource-affinity rule. A pin
// that already carries a record keeps it, so a later flatten never replaces
// the priorities the VM had before its first.
func flattenMemberPins(set *ha.RuleSet, members []string, owner string, configured []string) []ha.RuleOp {
	var ops []ha.RuleOp
	for _, s := range members {
		vmid, ok := vmidFromSid(s)
		if !ok {
			continue
		}
		pin := set.Find(naRuleNameFor(vmid))
		if pin == nil {
			continue
		}
		var next *ha.Rule
		ownership := haOwnershipOf(pin.Comment, owner)
		switch {
		case ownership == haOwnedByOther:
			continue
		case ownership == haOwnedByUs && ha.SoftPinNamesSomeNodes(pin, configured):
			next = ha.WithEveryNode(pin, configured)
		case pin.MultiplePriorities():
			next = ha.Flattened(pin)
		default:
			continue
		}
		if ownership == haOwnedByUs {
			next.Comment = ha.WithPinRestoreRecord(next.Comment, pin)
		}
		ops = append(ops, ha.RuleOp{Kind: ha.OpUpdate, Rule: *next})
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
// rule, deregisters its HA resource, and plans the rest of each of its groups
// again. delete_vm has no access to env.bosh, so the group name is unknown at
// delete time: the removal scans every bosh-aa-* rule for the VM's sid. It
// edits only a rule that carries this Director's marker, or an unmarked rule
// from an older CPI, and leaves a rule that names another Director alone. The
// rules change under the rules lock, with a PUT that carries the read's
// digest, and a rule left with fewer than two members is deleted. The HA
// resource is deregistered even when the rule writes fail, because its purge
// also takes the sid out of every rule. All failures are returned for the
// caller to log; cleanup never blocks VM deletion.
//
// With anti-affinity writes on (see antiAffinityJoinEnabled), the delete then
// plans each group the VM was in again, the way pve-ops ha
// --fix-anti-affinity does, so the members it left move into the index
// layout (see replanGroupsAfterDelete). tags are the VM's tags as the delete
// read them before the destroy. A delete has usually taken the sid out of its
// group rules before the destroy (see holdAntiAffinityPlacesBeforeDestroy),
// and the destroy's purge out of every other rule, so the member tag in tags
// is what names the group then. Confinement pins that the removal before the
// destroy repaired come in through ctx (see withAntiAffinityConfined).
//
// PVE takes no digest on a rule delete, so a delete planned from a read that a
// sibling's create has since grown would take the sibling out with the rule.
// The rules lock rules that out. Without it, a removal that deleted one of
// this Director's group rules writes the group's rules again once the purge is
// done (see healAfterUnlockedDeletes, or the plan above when it runs), so a
// sibling that joined the rule between the read and the delete gets its place
// back.
func removeAntiAffinityMembership(ctx context.Context, deps Deps, vmid int, tags string, logger *log.Logger) error {
	svc := deps.PVE.Cluster()
	sid := haResourceSid(vmid)
	owner := deps.RequestDirectorUUID
	var deleted, written map[string]string
	confined := antiAffinityConfined(ctx)

	err := withHARulesLock(ctx, deps, vmid, logger, func(ctx context.Context) error {
		var groupErr error
		if antiAffinityJoinEnabled(deps.Config) && owner != "" {
			var more []string
			written, more, groupErr = removeFromGroupRules(ctx, deps, sid, tags, logger)
			confined = append(confined, more...)
		}
		// No profile: a removal only shrinks rules, so the writer leaves the
		// feasibility checks to PVE and the cleanup never probes the cluster.
		writer := newHARuleWriter(deps, nil, logger)
		comments := map[string]string{}
		res, err := writer.Apply(ctx, "anti-affinity cleanup", func(set *ha.RuleSet) ([]ha.RuleOp, error) {
			return planAntiAffinityRemoval(set, sid, owner, comments), nil
		})
		deleted = writtenRuleDeletes(res, comments)
		if more := writtenRules(res, comments); len(more) > 0 {
			if written == nil {
				written = map[string]string{}
			}
			maps.Copy(written, more)
		}
		confined = append(confined, confinedBy(res)...)
		return errors.Join(groupErr, err)
	})
	if err != nil {
		err = fmt.Errorf("anti-affinity: remove %s from its rules: %w", sid, err)
	}

	deregisterHaResource(ctx, svc, sid, owner, logger)
	if !antiAffinityJoinEnabled(deps.Config) || owner == "" {
		return errors.Join(err, healAfterUnlockedDeletes(ctx, deps, deleted, owner, sid, confined, logger))
	}
	return errors.Join(err, replanGroupsAfterDelete(ctx, deps, vmid, tags, written, confined, logger))
}

// removeFromGroupRules takes sid out of the rules of each of this Director's
// groups that a rule names it in or whose member tag is in tags, and records
// the hold that keeps its place free for the recreate of its index (see
// aaHold). Each group's removal plans from the group's rules and HA
// resources, so the holds that other deletes of the group recorded stay
// recorded wherever a write moves them. It runs under the rules lock, before
// planAntiAffinityRemoval takes sid out of every other rule, and returns the
// comment of each group rule it wrote, keyed by the rule ID, and the members
// of the pins a repair ran for.
func removeFromGroupRules(
	ctx context.Context, deps Deps, sid, tags string, logger *log.Logger,
) (map[string]string, []string, error) {
	svc := deps.PVE.Cluster()
	owner := deps.RequestDirectorUUID
	set, err := readHARuleSet(ctx, svc)
	if err != nil {
		return nil, nil, fmt.Errorf("anti-affinity: read the HA rules: %w", err)
	}
	resources, err := listHAResources(ctx, svc)
	if err != nil {
		return nil, nil, fmt.Errorf("anti-affinity: read the HA resources: %w", err)
	}
	tagged := memberTagsOf(tags)
	written := map[string]string{}
	var confined []string
	var errs error
	for _, ident := range antiAffinityGroupsOf(owner, set, resources) {
		if !slices.Contains(tagged, ident.ruleName()) && !groupRuleNames(set, ident, sid) {
			continue
		}
		scan, err := collectGroupMemberSids(ctx, deps, ident)
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("anti-affinity: scan the members of group %s: %w",
				ident.ruleName(), err))
			continue
		}
		p := newGroupPlanner(ctx, deps, nil, ident, scan, logger)
		p.heal, p.removal, p.exclude = true, true, sid
		p.excludeIndex = guestIndex(pve.GuestRef{Tags: tags})
		comments := map[string]string{}
		writer := newHARuleWriter(deps, nil, logger)
		res, err := writer.Apply(ctx, "anti-affinity cleanup", func(set *ha.RuleSet) ([]ha.RuleOp, error) {
			ops, err := p.plan(set)
			for i := range ops {
				if r := set.Find(ops[i].Rule.ID); r != nil {
					comments[r.ID] = r.Comment
				} else {
					comments[ops[i].Rule.ID] = ops[i].Rule.Comment
				}
			}
			return ops, err
		})
		maps.Copy(written, writtenRules(res, comments))
		confined = append(confined, confinedBy(res)...)
		if err == nil && res != nil {
			logRestoreWaits(p, res.Set, logger)
		}
		if err != nil {
			errs = errors.Join(errs, fmt.Errorf("anti-affinity: take %s out of the rules of group %s: %w", sid,
				ident.ruleName(), err))
		}
	}
	return written, confined, errs
}

// groupRuleNames reports whether one of ident's group rules that this
// Director owns names sid.
func groupRuleNames(set *ha.RuleSet, ident haIdentity, sid string) bool {
	for i := range set.Rules {
		r := &set.Rules[i]
		if aapattern.IsGroupRule(ident.ruleName(), r.ID) && r.HasResource(sid) &&
			haOwnershipOf(r.Comment, ident.Owner) == haOwnedByUs {
			return true
		}
	}
	return false
}

// replanGroupsAfterDelete plans the rules of every group the deleted VM was in
// again, each under the HA rules lock, through the heal that pve-ops ha
// --fix-anti-affinity uses. So every write carries the digest of its read,
// flattens the members' pins, and passes the join conflict checks. The groups
// are those of the rules the removal wrote, from the comments in written, and
// those whose member tag is in tags, which it finds from the markers on the
// rules and the HA resources. Only this Director's groups count. The deleted
// VM stays out of every plan, and confined holds the members of the pins a
// repair in the removal ran for. A plan that moves a member from one rule to
// another can make PVE live-migrate it, which is how a group from an older
// release reaches the index layout. Every failure is returned for the caller
// to log, and none of them fails the delete.
func replanGroupsAfterDelete(
	ctx context.Context, deps Deps, vmid int, tags string, written map[string]string, confined []string,
	logger *log.Logger,
) error {
	owner := deps.RequestDirectorUUID
	sid := haResourceSid(vmid)
	groups := deletedGroupRules(written, owner)
	known := map[string]bool{}
	for _, g := range groups {
		known[g.ruleName()] = true
	}
	var missing map[string]bool
	for _, tag := range memberTagsOf(tags) {
		if !known[tag] {
			if missing == nil {
				missing = map[string]bool{}
			}
			missing[tag] = true
		}
	}
	var err error
	if len(missing) > 0 {
		found, findErr := antiAffinityGroupsNamed(ctx, deps, owner, missing)
		if findErr != nil {
			err = fmt.Errorf("anti-affinity: find the groups of %s's member tags: %w", sid, findErr)
		}
		groups = append(groups, found...)
	}
	deps = withRequestHAProfile(deps)
	index := guestIndex(pve.GuestRef{Tags: tags})
	for _, ident := range groups {
		healErr := withHARulesLock(ctx, deps, vmid, logger, func(ctx context.Context) error {
			return healAntiAffinityGroupHolding(ctx, deps, ident, "", sid, index, confined, logger)
		})
		if healErr != nil {
			err = errors.Join(err, fmt.Errorf("anti-affinity: plan the rules of group %s again after the delete: %w",
				ident.ruleName(), healErr))
			continue
		}
		logger.Info("anti-affinity: planned the group's rules again after the delete",
			log.String("rule", ident.ruleName()), log.String("sid", sid))
	}
	return err
}

// memberTagsOf returns the anti-affinity member tags in a VM's tags, each the
// name bosh-aa-<hash12> of a group's first rule, in the order they appear.
func memberTagsOf(tags string) []string {
	var out []string
	for _, t := range parseTagsField(tags) {
		t = strings.ToLower(t)
		if isMemberTag(t) && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// isMemberTag reports whether tag is a group's member tag, bosh-aa- followed
// by twelve lower-case hex digits.
func isMemberTag(tag string) bool {
	hash, ok := strings.CutPrefix(tag, haRuleNamePrefix)
	if !ok || len(hash) != 12 {
		return false
	}
	for _, c := range hash {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// antiAffinityGroupsNamed returns owner's groups whose first rule is named in
// names, from the markers on the rules and the HA resources.
func antiAffinityGroupsNamed(
	ctx context.Context, deps Deps, owner string, names map[string]bool,
) ([]haIdentity, error) {
	svc := deps.PVE.Cluster()
	set, err := readHARuleSet(ctx, svc)
	if err != nil {
		return nil, fmt.Errorf("read the HA rules: %w", err)
	}
	resources, err := listHAResources(ctx, svc)
	if err != nil {
		return nil, fmt.Errorf("read the HA resources: %w", err)
	}
	var out []haIdentity
	for _, ident := range antiAffinityGroupsOf(owner, set, resources) {
		if names[ident.ruleName()] {
			out = append(out, ident)
		}
	}
	return out, nil
}

// planAntiAffinityRemoval plans the writes that take sid out of every
// bosh-aa-* rule that this Director or an older CPI owns. A rule left with
// fewer than two members is deleted, and comments records the comment each
// rule had when a write was planned for it, which a delete does not keep and
// an update changes.
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
		comments[r.ID] = r.Comment
		if len(rest) < 2 {
			// Empty or now single-member: the rule no longer spreads anything.
			ops = append(ops, ha.RuleOp{Kind: ha.OpDelete, Rule: ha.Rule{ID: r.ID}})
			continue
		}
		next := r.Clone()
		next.Resources, next.Errors = rest, nil
		// The member stays an HA resource until the purge that follows, so
		// the write records it as leaving, and no plan of the group puts it
		// back meanwhile.
		if m, ok := parseHaMarker(r.Comment); ok && haOwnershipOf(r.Comment, owner) == haOwnedByUs {
			next.Comment = withLeavingMembers(r.Comment, append(slices.Clone(m.Left), sid))
		}
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

// writtenRules returns the comment of each rule that res reports written,
// whether deleted or updated, keyed by the rule ID. comments holds the comment
// of each rule a write was planned for. Every anti-affinity rule that a pin
// repair in res deleted or rewrote counts too, as in writtenRuleDeletes. It
// returns nil when none was.
func writtenRules(res *ha.ApplyResult, comments map[string]string) map[string]string {
	if res == nil {
		return nil
	}
	written := writtenRuleDeletes(res, comments)
	for i := range res.Wrote {
		op := &res.Wrote[i]
		if c, ok := comments[op.Rule.ID]; ok {
			if written == nil {
				written = map[string]string{}
			}
			written[op.Rule.ID] = c
		}
	}
	return written
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

// deletedGroupRules returns the groups of the rules a removal deleted or
// wrote, from the rule IDs and the comments they carried. Only a rule whose
// marker names this Director, and whose ID is a rule of the group the marker
// names, counts.
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
		if !aapattern.IsGroupRule(ident.ruleName(), id) || !ident.valid() || seen[ident.ruleName()] {
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
	return healAntiAffinityGroupHolding(ctx, deps, ident, self, removed, -1, confined, logger)
}

// healAntiAffinityGroupHolding is healAntiAffinityGroup with removed's
// instance index, or -1 when the caller does not know it. With the index
// known, from the caller or from removed's guest, the plan holds removed's
// place free for the recreate of its index instead of moving another member
// into it (see aaPlanner.excludeIndex).
func healAntiAffinityGroupHolding(
	ctx context.Context, deps Deps, ident haIdentity, self, removed string, removedIndex int, confined []string,
	logger *log.Logger,
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
		planner.excludeIndex = removedIndex
		_, res, err := applyGroupPlan(ctx, deps, profile, planner, nil, logger)
		if err == nil {
			restoreGroupPins(ctx, deps, profile, planner, res.Set, logger)
		}
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
// rule rather than removing it. The group's rules are every rule that
// aapattern.IsGroupRule names, its block rules and its pattern rules. A rule another Director owns, or one with no marker, is left
// alone. A live hold that a deleted rule records moves first to a rule of
// the group the prune keeps, or to a member's HA resource (see carryHolds).
// A hold that can't move, because the HA resources can't be listed or the
// resource's write fails, keeps its rule, and the prune returns a retriable
// error. It runs under the HA rules lock and reports the writer's error. Without
// the lock, a prune that deleted a rule writes the group's rules again
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
		holds := newGroupPlanner(ctx, deps, nil, ident, &groupScan{}, logger)
		comments := map[string]string{}
		res, err := writer.Apply(ctx, "anti-affinity prune", func(set *ha.RuleSet) ([]ha.RuleOp, error) {
			var ops []ha.RuleOp
			deleting := map[string]bool{}
			for i := range set.Rules {
				r := &set.Rules[i]
				if !aapattern.IsGroupRule(base, r.ID) || r.Type != haRuleType {
					continue
				}
				if haOwnershipOf(r.Comment, ident.Owner) != haOwnedByUs || len(r.Resources) >= 2 {
					continue
				}
				ops = append(ops, ha.RuleOp{Kind: ha.OpDelete, Rule: ha.Rule{ID: r.ID}})
				comments[r.ID] = r.Comment
				deleting[r.ID] = true
			}
			// The holds of the rules that go move first, so the group never
			// stops recording them. A hold that can't move keeps its rule,
			// and the prune fails with a retriable error.
			carried, err := holds.carryHolds(set, deleting, haResourceSid(vmid))
			if err != nil {
				return nil, err
			}
			return append(carried, ops...), nil
		})
		deleted, confined = writtenRuleDeletes(res, comments), confinedBy(res)
		if err == nil && len(deleted) > 0 {
			restoreGroupPinsAfterPrune(ctx, deps, ident, haResourceSid(vmid), res.Set, logger)
		}
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
	// guests holds the guest of each sid in members, from which a plan reads
	// the member's instance index.
	guests map[string]pve.GuestRef
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
	return scanGroupMembers(guests, len(excluded) == 0, ident), nil
}

// scanGroupMembers finds the members of ident's group among guests, which
// lists every live guest when complete is true. See collectGroupMemberSids.
func scanGroupMembers(guests []pve.GuestRef, complete bool, ident haIdentity) *groupScan {
	scan := &groupScan{
		members:      make(map[string]struct{}),
		legacy:       make(map[string]struct{}),
		live:         make(map[string]struct{}, len(guests)),
		liveComplete: complete,
		guests:       make(map[string]pve.GuestRef),
	}
	groupTag := ident.ruleName()
	legacyTags := legacyMemberTags(ident)
	for _, g := range guests {
		sid := "vm:" + strconv.Itoa(g.VMID)
		scan.live[sid] = struct{}{}
		switch {
		case tagsContain(g.Tags, groupTag):
			scan.members[sid] = struct{}{}
			scan.guests[sid] = g
		case len(legacyTags) > 0 && tagsContainAll(g.Tags, legacyTags):
			scan.members[sid] = struct{}{}
			scan.legacy[sid] = struct{}{}
			scan.guests[sid] = g
		}
	}
	return scan
}

// legacyMemberTags returns the tags that together prove a guest without the
// member tag is this Director's member of the group, or nil when the identity
// cannot name them.
func legacyMemberTags(ident haIdentity) []string {
	owner := ownerTagFor(ident.Owner)
	if owner == "" || ident.Deployment == "" || ident.Group == "" {
		return nil
	}
	return []string{legacyJobTag(ident.Group), legacyDeploymentTag(ident.Deployment), owner}
}

// legacyJobTag and legacyDeploymentTag spell the older job and deployment tags
// the way PVE stores them, in lower case, as ownerTagFor spells the owner tag.
// PVE cannot tell two names that differ only in case apart, so the legacy proof
// cannot either. Every reader of these tags builds them here, so the rule scan
// and the soft-spread scorer agree.
func legacyJobTag(group string) string { return "job--" + strings.ToLower(group) }

func legacyDeploymentTag(deployment string) string {
	return "deployment--" + strings.ToLower(deployment)
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

// tagsContain reports whether the PVE tags field contains want, ignoring case
// because PVE stores tags in lower case, and honoring PVE's ";"/"," separators (reuses parseTagsField).
func tagsContain(tags, want string) bool {
	if tags == "" || want == "" {
		return false
	}
	for _, t := range parseTagsField(tags) {
		if strings.EqualFold(t, want) {
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

// holdAntiAffinityPlacesBeforeDestroy takes the VM out of the rules of each of
// this Director's anti-affinity groups it is in, and records the hold that
// keeps its place for the recreate of its index, while its guest and its HA
// resource still exist. It runs under the rules lock, after the stop and
// before the destroy, whose purge then finds the VM in no rule of its groups.
//
// The removal after the destroy (see removeAntiAffinityMembership) records the
// same hold, but there it runs after the purge has taken the VM out of its
// rules. A sibling's delete or join that plans the group in between sees the
// VM's index missing with no hold, which looks like a hold that has lapsed,
// and its plan regroups the members above it. Here, until the write lands,
// every plan sees the VM in its rule, and once it lands, the hold keeps its
// place. A write that meets a digest conflict backs off and plans again, and
// the VM is still in its rule meanwhile, so no plan ever sees its place as a
// gap. The write records the VM as leaving the rule (see wantRule), so no
// plan puts it back before the purge.
//
// It returns the members of the confinement pins a repair ran for, which the
// removal after the destroy passes on to its plan of each group (see
// withAntiAffinityConfined). A failure is returned for the caller to log, and
// the removal after the destroy records the hold as before.
func holdAntiAffinityPlacesBeforeDestroy(
	ctx context.Context, deps Deps, vmid int, tags string, logger *log.Logger,
) ([]string, error) {
	if !antiAffinityJoinEnabled(deps.Config) || deps.RequestDirectorUUID == "" {
		return nil, nil
	}
	sid := haResourceSid(vmid)
	var confined []string
	err := withHARulesLock(ctx, deps, vmid, logger, func(ctx context.Context) error {
		var err error
		_, confined, err = removeFromGroupRules(ctx, deps, sid, tags, logger)
		return err
	})
	if err != nil {
		return confined, fmt.Errorf("anti-affinity: hold the places of %s before its destroy: %w", sid, err)
	}
	return confined, nil
}

// antiAffinityConfinedKey is the context key of the members of the
// confinement pins a repair ran for before the destroy.
type antiAffinityConfinedKey struct{}

// withAntiAffinityConfined returns ctx carrying the members of the
// confinement pins a repair in holdAntiAffinityPlacesBeforeDestroy ran for,
// so the removal after the destroy restores them with the rest.
func withAntiAffinityConfined(ctx context.Context, confined []string) context.Context {
	if len(confined) == 0 {
		return ctx
	}
	return context.WithValue(ctx, antiAffinityConfinedKey{}, slices.Clone(confined))
}

// antiAffinityConfined returns the members withAntiAffinityConfined put in
// ctx, or nil.
func antiAffinityConfined(ctx context.Context) []string {
	confined, _ := ctx.Value(antiAffinityConfinedKey{}).([]string)
	return slices.Clone(confined)
}
