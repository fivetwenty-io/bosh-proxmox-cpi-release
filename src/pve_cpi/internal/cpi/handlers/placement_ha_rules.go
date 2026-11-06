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
	owner := deps.RequestDirectorUUID
	svc := deps.PVE.Cluster()
	return &ha.RuleWriter{
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
}

// removeOwnedHaResource purges sid when it carries this Director's marker or
// none. Another Director's resource, or one whose owner cannot be read, stays.
func removeOwnedHaResource(ctx context.Context, svc cluster.Service, sid, owner string) error {
	got, err := svc.GetHaResources(ctx, sid)
	switch {
	case err != nil && (isHaNotFound(err) || pve.IsNotFound(err)):
		return nil
	case err != nil:
		return fmt.Errorf("read the owner of %s: %w", sid, err)
	case got != nil && got.Comment != nil && haOwnershipOf(*got.Comment, owner) == haOwnedByOther:
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
// re-drives. A rule that is not in effect does, which the writer returns only
// in managed mode. In managed mode a write the feasibility checks refused does
// too, because managed never leaves a VM without the rule it asked for.
func propagateHARuleError(deps Deps, err error) bool {
	if err == nil {
		return false
	}
	if cpierrors.IsType(err, cpierrors.TypeRetriableCloud) || errors.Is(err, ha.ErrRuleNotInEffect) {
		return true
	}
	return deps.Config.HAModeValue() == config.HAModeManaged && errors.Is(err, ha.ErrRuleInfeasible)
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
