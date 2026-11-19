package handlers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
)

// haRulesLockKey names the one cluster-scoped lock every CPI rule write holds,
// across its read, its checks, and its write. With antiAffinityLockPrefix it
// becomes the sentinel pool bosh-lock-aa-_rules. It stays inside the
// bosh-lock-aa- namespace the managed VM guard admits, and a sanitized group
// name never holds an underscore, so it never collides with an older CPI's
// per-group key aa-<group>.
const haRulesLockKey = "_rules"

// resolveHAProfile returns the request's HA profile, probing the cluster when
// nothing is wired.
func resolveHAProfile(ctx context.Context, deps Deps) (*ha.Profile, error) {
	resolve := deps.HAProfile
	if resolve == nil {
		resolve = probeHAProfile(deps)
	}
	return resolve(ctx)
}

// profileNodeNames returns the configured nodes the profile lists, or nil when
// there is no profile or it lists no node. A nil list means the configured node
// set is unknown: the rule writer then skips its own feasibility checks and
// relies on PVE's, the overflow cap is off, and every hard pin is strict.
func profileNodeNames(p *ha.Profile) []string {
	if p == nil || len(p.Nodes) == 0 {
		return nil
	}
	var out []string
	for i := range p.Nodes {
		if p.Nodes[i].Name != "" {
			out = append(out, p.Nodes[i].Name)
		}
	}
	return out
}

// placeableNodeCount returns how many nodes can take a VM now: the health
// report's placeable nodes, or the online nodes when there is no report.
func placeableNodeCount(p *ha.Profile) int {
	if p == nil {
		return 0
	}
	if p.Health != nil {
		n := 0
		for i := range p.Health.Nodes {
			if p.Health.Nodes[i].Placeable {
				n++
			}
		}
		return n
	}
	return len(p.OnlineNodes())
}

// withHARulesLock runs fn under the cluster-scoped rules lock when the cluster
// lock is enabled. A lock that cannot be taken is returned as the retriable
// error acquireAntiAffinityLock builds, and fn does not run. Under a context
// from withHARulesLockTryOnce the lock is tried once (see pve.WithoutWait). fn gets a context
// that carries the hold, and the rule writers built from it wait through the
// hold (see rulesLockHold), so fn must use that context for its writes. A
// request whose fn runs here sweeps the replicate VMs whose pins still name
// their source alone after its work (see withReplicaWidenSweep).
func withHARulesLock(
	ctx context.Context, deps Deps, vmid int, logger *log.Logger, fn func(ctx context.Context) error,
) error {
	if !deps.Config.ClusterLockEnabled() {
		noteHARulesLock(ctx, deps)
		return fn(ctx)
	}
	var opts []pve.ClusterLockOption
	if haRulesLockTriesOnce(ctx) {
		opts = append(opts, pve.WithoutWait())
	}
	handle, err := acquireAntiAffinityLock(ctx, deps, haRulesLockKey, vmid, opts...)
	if err != nil {
		return err
	}
	noteHARulesLock(ctx, deps)
	hold := &rulesLockHold{deps: deps, vmid: vmid, logger: logger, handle: handle, heldWait: rulesLockHeldWaitBudget}
	defer hold.release(ctx)
	return fn(context.WithValue(ctx, rulesLockHoldKey{}, hold))
}

// haRulesLockTryOnceKey is the context key that makes withHARulesLock try the
// rules lock once.
type haRulesLockTryOnceKey struct{}

// withHARulesLockTryOnce returns ctx with which withHARulesLock tries the rules
// lock once and, while another process holds it, returns at once with an
// error that matches pve.ErrClusterLockHeld instead of waiting. The replica
// widening pass uses it, because it would rather skip a VM than make the
// calls that queue on the lock wait longer.
func withHARulesLockTryOnce(ctx context.Context) context.Context {
	return context.WithValue(ctx, haRulesLockTryOnceKey{}, true)
}

// haRulesLockTriesOnce reports whether ctx came from withHARulesLockTryOnce.
func haRulesLockTriesOnce(ctx context.Context) bool {
	once, _ := ctx.Value(haRulesLockTryOnceKey{}).(bool)
	return once
}

// rulesLockHeldWaitBudget bounds the time one hold of the rules lock spends in
// the rule writer's backoff while it keeps the lock. Every CPI rule write in
// the cluster queues behind this lock, and the lock's claim budgets about
// twenty seconds for the read-modify-write at its shortest, so a holder that
// slept through a run of refusals would push the creates queued behind it
// toward their cluster_lock_timeout_sec wait and spend the claim. Past the
// budget the writer tries again without waiting.
const rulesLockHeldWaitBudget = 2 * time.Second

// rulesLockHoldKey is the context key of the rulesLockHold that a
// withHARulesLock call hands to fn.
type rulesLockHoldKey struct{}

// rulesLockHold is one withHARulesLock call's hold of the rules lock. The rule
// writers built by newHARuleWriterFor pay their backoff through it. A writer
// that may yield, which is the anti-affinity ensure before it has written
// anything, releases the lock for the wait and takes it again, so a create that
// waits for a sibling's resource does not hold up every other rule write in
// the cluster. Every other wait keeps the lock and is cut to what remains of
// rulesLockHeldWaitBudget. A hold is used by one request's goroutine only.
type rulesLockHold struct {
	deps   Deps
	vmid   int
	logger *log.Logger
	// handle is the lock while it is held, and nil once a wait released it
	// and could not take it again.
	handle *pve.ClusterLockHandle
	// yield lets an idle writer's wait release the lock.
	yield bool
	// heldWait is what remains of rulesLockHeldWaitBudget for this hold.
	heldWait time.Duration
}

// rulesLockHoldFrom returns the hold ctx carries, or nil when the caller holds
// no rules lock.
func rulesLockHoldFrom(ctx context.Context) *rulesLockHold {
	hold, _ := ctx.Value(rulesLockHoldKey{}).(*rulesLockHold)
	return hold
}

// allowYield sets whether an idle writer's wait may release the lock. It is
// safe on a nil hold.
func (h *rulesLockHold) allowYield(on bool) {
	if h != nil {
		h.yield = on
	}
}

// wait pays the backoff d. With yield on and an idle writer it releases the
// lock, sleeps, and takes the lock again with a fresh claim, which also renews
// the hold's budget. A lock that cannot be taken again ends the wait with the
// acquire's retriable error, and the writer stops. Otherwise it sleeps with the
// lock held for no longer than the budget that remains.
func (h *rulesLockHold) wait(
	ctx context.Context, d time.Duration, idle bool, sleep func(context.Context, time.Duration) error,
) error {
	if !h.yield || !idle || h.handle == nil {
		d = min(d, h.heldWait)
		if d <= 0 {
			return nil
		}
		h.heldWait -= d
		return sleep(ctx, d)
	}
	releaseAntiAffinityLock(ctx, h.handle, haRulesLockKey, h.logger)
	h.handle = nil
	if err := sleep(ctx, d); err != nil {
		return err
	}
	handle, err := acquireAntiAffinityLock(ctx, h.deps, haRulesLockKey, h.vmid)
	if err != nil {
		return err
	}
	h.handle, h.heldWait = handle, rulesLockHeldWaitBudget
	return nil
}

// release gives the lock up when the hold still has it.
func (h *rulesLockHold) release(ctx context.Context) {
	if h.handle != nil {
		releaseAntiAffinityLock(ctx, h.handle, haRulesLockKey, h.logger)
		h.handle = nil
	}
}

// haRuleBackoff returns the wait the request's rule writers pay between
// attempts. Under a rules lock it goes through the hold, and without one it
// sleeps, on deps.HARuleSleep when a test set it.
func haRuleBackoff(deps Deps) func(ctx context.Context, d time.Duration, idle bool) error {
	sleep := deps.HARuleSleep
	if sleep == nil {
		sleep = ha.SleepContext
	}
	return func(ctx context.Context, d time.Duration, idle bool) error {
		if hold := rulesLockHoldFrom(ctx); hold != nil {
			return hold.wait(ctx, d, idle, sleep)
		}
		return sleep(ctx, d)
	}
}

// newHARuleWriter builds the rule writer for one request. The caller holds the
// rules lock (withHARulesLock), so the writer takes none itself, which lets the
// unmarked-rule migration take the older CPI's key inside the same lock.
func newHARuleWriter(deps Deps, profile *ha.Profile, logger *log.Logger) *ha.RuleWriter {
	return newHARuleWriterFor(deps, profile, logger, nil)
}

// newHARuleWriterFor is newHARuleWriter for a call that holds the eligibility
// decision of one VM. The decision carries the CRM uid its state read saw and
// whether this call staged the VM's resource as ignored, which lets the writer
// remove that resource when a repair of its confinement pin cannot be verified.
// Without a decision the writer never removes a resource.
func newHARuleWriterFor(deps Deps, profile *ha.Profile, logger *log.Logger, decision *ha.VMDecision) *ha.RuleWriter {
	owner := deps.RequestDirectorUUID
	svc := deps.PVE.Cluster()
	writer := &ha.RuleWriter{
		API: svc,
		Owned: func(r *ha.Rule) bool {
			return owner != "" && haOwnershipOf(r.Comment, owner) == haOwnedByUs
		},
		ClusterNodes: profileNodeNames(profile),
		Managed:      deps.Config.HAModeValue() == config.HAModeManaged,
		Logger:       logger,
		Backoff:      haRuleBackoff(deps),
		Repair: ha.RepairDeps{
			RemoveResource: func(ctx context.Context, sid string) error {
				return removeOwnedHaResource(ctx, svc, sid, owner)
			},
		},
	}
	if decision != nil {
		writer.Repair = decision.RepairDeps(
			func(ctx context.Context, vmid int) (*pve.HAServiceState, error) {
				return pve.ReadHAServiceState(ctx, svc, deps.PVE.Nodes(), pve.HAServiceStateInput{
					VMID: vmid, VMNode: decision.Node, Kind: pve.HADecisionLive, HealthOptions: haHealthOptions(deps),
				})
			},
			writer.Repair.RemoveResource,
		)
	}
	return writer
}

// removeOwnedHaResource purges sid only when it carries this Director's
// marker. An unmarked resource may be an operator's, and nothing here can prove
// it belongs to this VM, so it stays, as does another Director's resource or one
// whose owner cannot be read.
func removeOwnedHaResource(ctx context.Context, svc cluster.Service, sid, owner string) error {
	return removeOwnedHaResourceIf(ctx, svc, sid, owner, nil)
}

// removeOwnedHaResourceIf is removeOwnedHaResource with a further check of the
// marker it read. A non-nil check runs on that comment after the ownership
// check, and an error from it keeps the resource and is returned.
func removeOwnedHaResourceIf(
	ctx context.Context, svc cluster.Service, sid, owner string, check func(comment string) error,
) error {
	got, err := svc.GetHaResources(ctx, sid)
	switch {
	case err != nil && (isHaNotFound(err) || pve.IsNotFound(err)):
		return nil
	case err != nil:
		return fmt.Errorf("read the owner of %s: %w", sid, err)
	case got == nil:
		return fmt.Errorf("read the owner of %s: no resource returned", sid)
	case got.Comment == nil || haOwnershipOf(*got.Comment, owner) == haUnmarked:
		return fmt.Errorf("%s carries no ownership marker, so the CPI cannot prove it is this VM's", sid)
	case haOwnershipOf(*got.Comment, owner) != haOwnedByUs:
		return fmt.Errorf("%s belongs to another Director", sid)
	}
	if check != nil {
		if err := check(*got.Comment); err != nil {
			return err
		}
	}
	purge := true
	if err := svc.DeleteHaResources(ctx, sid, &cluster.DeleteHaResourcesParams{Purge: &purge}); err != nil &&
		!isHaNotFound(err) {
		return err
	}
	return nil
}

// propagateHARuleError reports whether a rule writer's error must fail the
// call instead of being logged. A retriable error always does, so the Director
// re-drives, and so does a rule that is not in effect, which the writer returns
// only in managed mode. A pin with no admissible node is retriable, so it fails
// the call in either mode.
//
// In managed mode every other error fails the call too, because managed never
// leaves a VM without a rule it asked for. That covers a write the feasibility
// checks or PVE refused, a rule that carries another Director's marker or has
// the wrong type, a failed member scan or rules read, a request with no
// director_uuid to mark the rule with, and the PVE 9 gate. In auto they are
// logged, which keeps the best-effort intent. A failed HA resource registration
// is logged in both modes, because the rule write that follows fails on its own
// when the resource is missing.
func propagateHARuleError(deps Deps, err error) bool {
	if err == nil {
		return false
	}
	if cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || errors.Is(err, ha.ErrRuleNotInEffect) {
		return true
	}
	return deps.Config.HAModeValue() == config.HAModeManaged
}

// inResourceAffinityRule reports whether sid is in an enabled resource-affinity
// rule, which means PVE refuses a node-affinity rule with several priorities
// for it.
func inResourceAffinityRule(set *ha.RuleSet, sid string) bool {
	for i := range set.Rules {
		r := &set.Rules[i]
		if r.Type == ha.RuleTypeResourceAffinity && !r.Disabled && r.HasResource(sid) {
			return true
		}
	}
	return false
}
