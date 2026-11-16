package handlers

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/placement"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/spreadclaim"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/vmcid"
)

// spreadClaimReleaseTimeout bounds the release of a claim, which runs detached
// from the request so a cancelled create still gives its claim up.
const spreadClaimReleaseTimeout = 5 * time.Second

// EndpointCacheSpreadClaims is the production source of spread claim records.
// It keeps the record in the endpoint cache directory that cfg names, scoped
// to the cluster that clusterStamp identifies (see spreadClaimScope). It
// returns nil, which turns the claims off, when no scope can be named.
func EndpointCacheSpreadClaims(cfg *config.CPIConfig, clusterStamp string, logger *log.Logger) *spreadclaim.Store {
	scope := spreadClaimScope(cfg, clusterStamp)
	if scope == "" {
		return nil
	}
	return &spreadclaim.Store{Dir: endpointcache.OptionsFor(cfg), Scope: scope, Log: logger}
}

// spreadClaimScope names the cluster a claim counts in. Two cpi_config
// entries can reach one cluster through different hosts, and two clusters can
// both have a node called pve1, so the scope is the cluster's own identity:
// the stamp of its pve-root-ca, which every node of a cluster shares. A create
// that could not read the stamp falls back to the entry's configured
// endpoints, as the HA episode record does, and an entry with neither gets ""
// and makes no claim, since an empty scope would be shared by every such
// entry.
func spreadClaimScope(cfg *config.CPIConfig, clusterStamp string) string {
	if clusterStamp != "" && clusterStamp != vmcid.UnknownCluster {
		return "pve-root-ca:" + clusterStamp
	}
	if scope := ha.EpisodeScope(cfg); scope != "" {
		return "endpoints:" + scope
	}
	return ""
}

// spreadClaims is one create's claim on the node it picks for its instance
// group. The soft spread counts a group's members on each node from the
// member tag, which a create stamps only once its clone is configured, so
// creates that score at the same moment would not see each other. Each create
// claims the node it picks and counts the live claims of its group on top of
// the tagged members. A nil *spreadClaims picks without
// claims, which is what every caller gets when anti-affinity, placement
// scoring, or the claim source is off.
//
// The claim is held until create_vm returns, because a sibling reads the
// member tags before it reads the claims, and a tag that lands between the two
// reads would otherwise be missed by both. Once the create has allocated its
// VM it binds the VMID to the claim, and a sibling whose facts already count
// that VMID from its tag leaves the claim out.
type spreadClaims struct {
	claim  *spreadclaim.Claim
	logger *log.Logger
}

// newSpreadClaims returns the claim of a create in an instance group that
// placement spreads with the anti-affinity axis, or nil when there is none. A
// create that a target_node or a local disk pins claims its node too, so the
// siblings that score while it clones count it. expires is when the create's
// work ends at the latest, which bounds a claim that a crash leaves behind.
func newSpreadClaims(deps Deps, parsed *createVMParsedArgs, expires time.Time, logger *log.Logger) *spreadClaims {
	if deps.SpreadClaims == nil || deps.Config == nil || !deps.Config.PlacementEnabled() || parsed == nil {
		return nil
	}
	group := antiAffinityScorerGroup(deps, parsed.env)
	if !group.enabled() {
		return nil
	}
	store := deps.SpreadClaims(deps.Config, parsed.clusterStamp, logger)
	if store == nil {
		return nil
	}
	claim, err := store.NewClaim(group.claimKey(), expires)
	if err != nil {
		logger.Warn("create_vm: placement: cannot claim a node for the instance group, so the spread counts "+
			"only the members already tagged", log.Err(err))
		return nil
	}
	return &spreadClaims{claim: claim, logger: logger}
}

// spreadClaimExpiry is when a create's claim lapses if nothing releases it: the
// create's deadline, or its create budget from now when ctx has none.
func spreadClaimExpiry(ctx context.Context, deps Deps) time.Time {
	if deadline, ok := ctx.Deadline(); ok {
		return deadline
	}
	return time.Now().Add(createVMIDClaimBudget(deps))
}

// claimKey names the group in the claim record. It is the member tag, or the
// legacy tags a request without a director_uuid spreads on.
func (g scorerGroup) claimKey() string {
	if g.tag != "" {
		return g.tag
	}
	return strings.Join(g.legacy, ";")
}

// scoreAndPick scores pass and picks a node. With a claim it does so in one
// turn with the claim record: it adds the live claims of the group to each
// node's member count, picks, and claims the winner, so concurrent creates of
// the group see each other's picks.
func (s *spreadClaims) scoreAndPick(
	ctx context.Context, pass []placement.NodeFacts, weights placement.Weights, logger *log.Logger, az string,
) (chosen string, ranked []string) {
	if s == nil || len(pass) == 0 {
		return scoreAndPickWithRanked(pass, weights, logger, az)
	}
	s.pick(ctx, pass, func(counts map[string]int) string {
		chosen, ranked = scoreAndPickWithRanked(withClaimCounts(pass, counts, logger), weights, logger, az)
		return chosen
	})
	return chosen, ranked
}

// pick runs choose in one turn with the claim record, with the live claims of
// the group per node, leaving out the claims of siblings that facts already
// count from their tag, and claims the node choose returns.
//
// The two counts can disagree for a moment, which only biases one pick. A
// sibling whose member tag lands before its create binds the VMID counts
// twice, once from the tag and once from its claim, until the bind. And when
// a rolled-back VMID is allocated again while a fact still carries the old
// VM's tag, the claim bound to that VMID is left out until the facts catch up.
func (s *spreadClaims) pick(ctx context.Context, facts []placement.NodeFacts, choose func(map[string]int) string) {
	tagged := map[int]bool{}
	for i := range facts {
		for _, v := range facts[i].SameGroupVMIDs {
			if v > 0 {
				tagged[v] = true
			}
		}
	}
	_, err := s.claim.PickExcept(ctx, func(vmid int) bool { return tagged[vmid] }, choose)
	s.logPickError(err)
}

// logPickError says what a failed turn with the record cost the spread.
func (s *spreadClaims) logPickError(err error) {
	switch {
	case err == nil:
	case errors.Is(err, spreadclaim.ErrUnavailable):
		s.logger.Warn("create_vm: placement: the spread claim record could not be used, so the spread counts "+
			"only the members already tagged", log.Err(err))
	case errors.Is(err, spreadclaim.ErrNotRecorded):
		s.logger.Warn("create_vm: placement: the spread counted the claims of in-flight siblings, but this "+
			"create's claim could not be recorded, so siblings that score while it clones do not count it",
			log.Err(err))
	case errors.Is(err, spreadclaim.ErrRewritten):
		s.logger.Warn("create_vm: placement: the spread claim record was damaged and was rewritten with this "+
			"create's claim, so the claims of siblings already in flight were lost", log.Err(err))
	default:
		s.logger.Warn("create_vm: placement: the spread claim record failed", log.Err(err))
	}
}

// hold claims node for a create whose node was decided without scoring, such
// as one that target_node or a local disk pins, so siblings count it.
func (s *spreadClaims) hold(ctx context.Context, node string) {
	s.move(ctx, node)
}

// withClaimCounts returns a copy of pass in which each node's member count
// includes the group's live claims on it.
func withClaimCounts(pass []placement.NodeFacts, counts map[string]int, logger *log.Logger) []placement.NodeFacts {
	if len(counts) == 0 {
		return pass
	}
	out := slices.Clone(pass)
	for i := range out {
		if n := counts[out[i].Node]; n > 0 {
			out[i].SameGroupCount += n
			logger.Debug("create_vm: placement: counting in-flight instance group members",
				log.String("node", out[i].Node), log.Int("in_flight", n),
				log.Int("same_group_count", out[i].SameGroupCount))
		}
	}
	return out
}

// move claims node instead, as the fallback loop does before it tries an
// alternate, and as a storage plan does once it settles on its node. A claim
// that the record already holds on node is left as it is, and one whose last
// write failed is written again.
func (s *spreadClaims) move(ctx context.Context, node string) {
	if s == nil || node == "" || s.claim.Holds(node) {
		return
	}
	if err := s.claim.Move(ctx, node); err != nil {
		s.logger.Warn("create_vm: placement: cannot move the instance group claim to the node the create uses",
			log.String("node", node), log.Err(err))
	}
}

// bind names the VM the create allocated on the claimed node, so a sibling
// that counts the VM from its member tag leaves the claim out.
func (s *spreadClaims) bind(ctx context.Context, vmid int) {
	if s == nil || vmid <= 0 {
		return
	}
	if err := s.claim.Bind(ctx, vmid); err != nil {
		s.logger.Warn("create_vm: placement: cannot name the new VM in the instance group claim, so a sibling "+
			"may count it twice until this create returns", log.Int("vmid", vmid), log.Err(err))
	}
}

// release gives the claim up when create_vm returns. It runs detached from
// ctx, so a cancelled create still releases, and it is free to call again.
func (s *spreadClaims) release(ctx context.Context) {
	if s == nil || s.claim.Node() == "" {
		return
	}
	relCtx, cancel := detachedContext(ctx, spreadClaimReleaseTimeout)
	defer cancel()
	if err := s.claim.Release(relCtx); err != nil {
		s.logger.Warn("create_vm: placement: cannot release the instance group claim; it lapses with the create's "+
			"deadline", log.Err(err))
	}
}
