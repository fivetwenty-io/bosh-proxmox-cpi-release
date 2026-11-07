package handlers

import (
	"context"
	"errors"
	"fmt"

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
		for _, e := range p.Health.Nodes {
			if e.Placeable {
				n++
			}
		}
		return n
	}
	return len(p.OnlineNodes())
}

// withHARulesLock runs fn under the cluster-scoped rules lock when the cluster
// lock is enabled. A lock that cannot be taken is returned as the retriable
// error acquireAntiAffinityLock builds, and fn does not run.
func withHARulesLock(ctx context.Context, deps Deps, vmid int, logger *log.Logger, fn func() error) error {
	if !deps.Config.ClusterLockEnabled() {
		return fn()
	}
	handle, err := acquireAntiAffinityLock(ctx, deps, haRulesLockKey, vmid)
	if err != nil {
		return err
	}
	defer releaseAntiAffinityLock(ctx, handle, haRulesLockKey, logger)
	return fn()
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
