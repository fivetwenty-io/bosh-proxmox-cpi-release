package handlers

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/endpointcache"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/placement"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/spreadclaim"
)

// spreadClaimReleaseTimeout bounds the release of a claim, which runs detached
// from the request so a cancelled create still gives its claim up.
const spreadClaimReleaseTimeout = 5 * time.Second

// EndpointCacheSpreadClaims is the production source of spread claim records.
// It keeps each cluster entry's record in the endpoint cache directory that
// cfg names.
func EndpointCacheSpreadClaims(cfg *config.CPIConfig, logger *log.Logger) *spreadclaim.Store {
	return &spreadclaim.Store{Dir: endpointcache.OptionsFor(cfg), Scope: ha.EpisodeScope(cfg), Log: logger}
}

// spreadClaims is one create's claim on the node it picks for its instance
// group. The soft spread counts a group's members on each node from the
// member tag, which a create stamps only once its clone is configured, so
// creates that score at the same moment would not see each other. Each create
// claims the node it picks, counts the live claims of its group on top of the
// tagged members, and gives its claim up once its clone is configured, because
// the tag carries the count from then on. A nil *spreadClaims picks without
// claims, which is what every caller gets when anti-affinity, placement
// scoring, or the claim source is off.
type spreadClaims struct {
	claim  *spreadclaim.Claim
	logger *log.Logger
}

// newSpreadClaims returns the claim of a create whose placement scores nodes
// with the anti-affinity axis, or nil when it does not. expires is when the
// create's work ends at the latest, which bounds a claim that a crash leaves
// behind.
func newSpreadClaims(deps Deps, parsed *createVMParsedArgs, expires time.Time, logger *log.Logger) *spreadClaims {
	if deps.SpreadClaims == nil || deps.Config == nil || !deps.Config.PlacementEnabled() || parsed == nil ||
		parsed.cloudProps.TargetNode != "" {
		return nil
	}
	group := antiAffinityScorerGroup(deps, parsed.env)
	if !group.enabled() {
		return nil
	}
	store := deps.SpreadClaims(deps.Config, logger)
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
	_, err := s.claim.Pick(ctx, func(counts map[string]int) string {
		chosen, ranked = scoreAndPickWithRanked(withClaimCounts(pass, counts, logger), weights, logger, az)
		return chosen
	})
	if err != nil {
		s.logger.Warn("create_vm: placement: the spread claim record could not be used, so the spread counts "+
			"only the members already tagged", log.Err(err))
	}
	return chosen, ranked
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
// alternate. A claim that already holds node is left as it is.
func (s *spreadClaims) move(ctx context.Context, node string) {
	if s == nil || node == "" || s.claim.Node() == node {
		return
	}
	if err := s.claim.Move(ctx, node); err != nil {
		s.logger.Warn("create_vm: placement: cannot move the instance group claim to the alternate node",
			log.String("node", node), log.Err(err))
	}
}

// release gives the claim up. It runs detached from ctx, so a cancelled create
// still releases, and it is free to call again.
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
