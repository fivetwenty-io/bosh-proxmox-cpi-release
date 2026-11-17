// list_guests.go: authoritative cluster-wide guest enumeration.
//
// The /cluster/resources index trails node-local state by minutes, so any
// decision fed by it can miss a young VM or see a dead one. This helper is
// the fleet-wide counterpart of FindVMAuthoritative (find_vm.go): membership
// comes from corosync-backed /cluster/config/nodes, guests come from each
// node's own /nodes/<n>/qemu listing (served from that node's pmxcfs view,
// no lag), and an unlistable node fails the enumeration loudly instead of
// silently shrinking the result. Callers that can tolerate partial data must
// say so explicitly by handling the typed error, never by this helper
// guessing for them.
package pve

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// GuestRef is one guest from an authoritative per-node listing.
type GuestRef struct {
	// VMID is the guest's unique cluster-wide ID.
	VMID int
	// Node is the node whose listing reported the guest; with per-node
	// listings this is the node the guest actually lives on.
	Node string
	// Name is the guest name; empty when unnamed.
	Name string
	// Tags is the raw semicolon-delimited PVE tag string; empty when the
	// guest has no tags. Split with SplitTags for token tests.
	Tags string
	// Status is the guest power state as reported by its node ("running",
	// "stopped"); empty when the listing omits it.
	Status string
	// Template reports whether the guest is frozen as a PVE template.
	Template bool
}

// SplitTags returns the guest's tag tokens (PVE semicolon/comma delimited);
// empty slice when untagged.
func (g GuestRef) SplitTags() []string {
	return splitPVETags(g.Tags)
}

// ListGuestsAuthoritative enumerates every QEMU guest in the cluster from
// authoritative sources: node membership via ListClusterConfigNodes
// (corosync-backed, no lag) and guests via each node's own qemu listing. The
// per-node listings ride RetryOnTransient; when any node still cannot be
// listed (including a member that is simply powered off) the whole
// enumeration fails with a retriable error naming the node(s), because a
// silent partial result is exactly the stale-index shape this helper exists
// to replace (the unlistable node is the likeliest holder of the guest a
// caller is about to act on). logger may be nil.
//
// This strict form is for absence proofs: any caller whose next step is
// destructive when a guest is NOT found (disk holder scans, sole-owner
// refcounts, template sweeps gating a base-volume delete) must use it, since
// a powered-off node's guests still reference their disks and tags in
// config. Advisory or presence-driven callers that can safely act on a
// reduced fleet use ListGuestsAuthoritativeTolerant instead.
//
// When a member's listing fails and the entry node does not report the
// cluster quorate and lists that member offline, the listing stops retrying
// it after that one attempt. Nothing brings the member back while quorum is
// gone, and each attempt against a member that is gone can take tens of
// seconds. The member still fails the enumeration, and the refusal names the
// lost quorum. The cluster's status is read once per enumeration for this,
// with a single attempt, and a failed read leaves the full retry budget.
func ListGuestsAuthoritative(ctx context.Context, c Client, logger *log.Logger) ([]GuestRef, error) {
	guests, _, err := listGuestsAuthoritative(ctx, c, logger, nil, nil)
	return guests, err
}

// ListGuestsAuthoritativeTolerant is ListGuestsAuthoritative with
// offline-member tolerance. Corosync config membership includes members that
// are powered off, rebooting, or removed without a clean delnode, and their
// per-node listing can never answer; without tolerance one such member
// blocks every enumeration-fed operation cluster-wide. Before the fan-out,
// /cluster/status is consulted best-effort: when the cluster is quorate, a
// member it reports offline is excluded (with a Warn) rather than failing
// the enumeration. The excluded member names come back as the second return
// value, and a non-empty list means the result proves nothing about guests
// on those nodes: callers must treat it as "unenumerated", never as
// "absent". When the status read fails, the cluster is not quorate (a
// minority partition has no authority to declare peers down), or every
// member is reported offline, the tolerance is withheld and the strict
// fail-loud rule applies unreduced.
//
// A member can also go offline during the fan-out. A rebooting node stops
// answering its API before corosync drops it from membership, so the status
// read before the fan-out still reports it online, and its listing then fails
// on every attempt the retry budget allows. To keep that window from stalling
// the call, each failed attempt on a member reads /cluster/status again, and a
// member the quorate cluster now reports offline is excluded on the spot, the
// same way as one reported offline before the fan-out. A member the cluster
// still reports online keeps the full retry budget, because a brief blip on
// it is what the retries exist for.
//
// Only advisory or presence-driven callers belong here: the VMID allocator's
// union leg, the IP-conflict probes, the anti-affinity live set (which must
// gate its drops on an empty excluded list), and the orphan-template prune
// (under-enumeration only skips candidates). Absence proofs, and the pve-cid
// inventory (which must surface a partial fleet rather than silently
// under-report), use the strict ListGuestsAuthoritative.
func ListGuestsAuthoritativeTolerant(ctx context.Context, c Client, logger *log.Logger) ([]GuestRef, []string, error) {
	// Guard before the status consult: offlineClusterNodes dereferences the
	// client, and both entry points must classify a nil client identically
	// instead of one of them panicking.
	if c == nil || c.Nodes() == nil {
		return nil, nil, cpierrors.Cloud("ListGuestsAuthoritative: nodes service unavailable")
	}
	// One status attempt per failed listing attempt: the listing's own retry
	// loop already paces the reads, and a status read with its own retries
	// here would multiply the stall this check exists to cut short. The same
	// read answers whether the quorate cluster now reports the member offline
	// and whether the cluster has lost quorum and lists it offline.
	stop := func(node string, cause error) error {
		return memberStopFromStatus(ctx, c, logger, node, cause)
	}
	return listGuestsAuthoritative(ctx, c, logger, offlineClusterNodes(ctx, c, logger), stop)
}

// ListMemberGuests lists the QEMU guests of the named members only, each with
// the strict form's full retry budget and no offline tolerance. It is for a
// caller of ListGuestsAuthoritativeTolerant whose answer would rest on what the
// excluded members hold. A corosync link that drops for a few seconds gets a
// member excluded even though its guests keep running, and listing it again
// with the full budget rides out a blip like that before the caller concludes
// anything from the member's absence. A member that still doesn't answer fails
// the call with the partial-fleet error, which names it and is retriable for
// a transport fault.
func ListMemberGuests(ctx context.Context, c Client, logger *log.Logger, nodes []string) ([]GuestRef, error) {
	if ctx == nil {
		return nil, cpierrors.Cloud("ListMemberGuests: ctx must not be nil")
	}
	if c == nil || c.Nodes() == nil {
		return nil, cpierrors.Cloud("ListMemberGuests: nodes service unavailable")
	}
	var guests []GuestRef
	var failedNodes []string
	var failedErrs []error
	var noQuorum []string
	quorum := newQuorumLossProbe(ctx, c, logger)
	seen := make(map[string]bool, len(nodes))
	for _, node := range nodes {
		if seen[node] {
			continue
		}
		seen[node] = true
		nodeGuests, err := listNodeGuests(ctx, c, logger, node, quorum.stop)
		if err != nil {
			failedNodes = append(failedNodes, node)
			failedErrs = append(failedErrs, err)
			var noQuorumErr *memberUnreachableWithoutQuorumError
			if errors.As(err, &noQuorumErr) {
				noQuorum = append(noQuorum, node)
			}
			continue
		}
		guests = append(guests, nodeGuests...)
	}
	if len(failedNodes) > 0 {
		return nil, partialFleetError(failedNodes, failedErrs, failedMembersReportedOffline(ctx, c, logger, failedNodes), noQuorum)
	}
	return guests, nil
}

// failedMembersReportedOffline returns the members among failed that the
// quorate cluster reports offline, read with a single status attempt after a
// listing with the strict budget gave up on them. The strict listing never
// leaves such a member out, because its callers need every member's guests
// and a member that is offline still holds the configs of its guests. Naming
// it as offline in the refusal tells the operator that the call can only
// succeed once the member is back. A status read that fails, or a cluster
// without quorum, names none, and the refusal reads as it did before.
func failedMembersReportedOffline(ctx context.Context, c Client, logger *log.Logger, failed []string) []string {
	offline := readOfflineClusterNodes(ctx, c, logger, 1)
	if len(offline) == 0 {
		return nil
	}
	var out []string
	for _, node := range failed {
		if offline[node] {
			out = append(out, node)
		}
	}
	return out
}

// ClusterReportsMemberOffline reports whether the quorate cluster now reports
// node offline. It reads /cluster/status with a single attempt, because its
// callers ask after a read of that node has already failed, and a status read
// with a retry budget of its own would stretch the stall it exists to cut
// short. Any failure, and a cluster without quorum, answers false.
func ClusterReportsMemberOffline(ctx context.Context, c Client, logger *log.Logger, node string) bool {
	if c == nil {
		return false
	}
	return readOfflineClusterNodes(ctx, c, logger, 1)[node]
}

// memberWentOfflineError ends a member's listing retries because the quorate
// cluster began reporting the member offline between attempts. It keeps the
// failed attempt's error for the log, but it neither unwraps to that error nor
// repeats its text, because the retry classifiers read both, and a transient
// cause would otherwise keep the loop going.
type memberWentOfflineError struct {
	node  string
	cause error
}

func (e *memberWentOfflineError) Error() string {
	return "cluster reports member " + e.node + " offline"
}

// memberUnreachableWithoutQuorumError ends a member's listing retries because
// the entry node does not report the cluster quorate and lists the member
// offline. Without quorum nothing brings the member back within the retry
// budget, and each attempt against a member that is gone can take tens of
// seconds. Unlike memberWentOfflineError it never excludes the member, so the
// enumeration still fails, only sooner. Like that error, it neither unwraps to
// the failed attempt's error nor repeats its text, so the retry classifiers
// stop on it.
type memberUnreachableWithoutQuorumError struct {
	node  string
	cause error
}

func (e *memberUnreachableWithoutQuorumError) Error() string {
	return "member " + e.node + " is offline and " + ReasonNotQuorate
}

// quorumLossProbe reads the cluster's status at most once per enumeration,
// the first time a member's listing fails, and remembers whether the entry
// node reported the cluster without quorum and which members it listed
// offline. A failed read proves nothing, so it answers false for every member.
type quorumLossProbe struct {
	once    sync.Once
	ctx     context.Context
	c       Client
	logger  *log.Logger
	lost    bool
	offline map[string]bool
}

func newQuorumLossProbe(ctx context.Context, c Client, logger *log.Logger) *quorumLossProbe {
	return &quorumLossProbe{ctx: ctx, c: c, logger: logger}
}

// stop returns a *memberUnreachableWithoutQuorumError when the cluster has
// lost quorum and lists node offline, and nil otherwise.
func (p *quorumLossProbe) stop(node string, cause error) error {
	p.once.Do(func() {
		if p.c == nil || p.c.Cluster() == nil {
			return
		}
		q, err := ReadClusterQuorum(p.ctx, p.c.Cluster())
		if err != nil {
			if p.logger != nil {
				p.logger.Debug("ListGuestsAuthoritative: cluster status unreadable for the quorum check", log.Err(err))
			}
			return
		}
		p.lost, p.offline = q.Lost(), q.Offline
	})
	if p.lost && p.offline[node] {
		return &memberUnreachableWithoutQuorumError{node: node, cause: cause}
	}
	return nil
}

// memberStopFromStatus reads the cluster's status with a single attempt after
// a member's listing attempt failed. It returns a *memberWentOfflineError when
// the quorate cluster now reports the member offline, a
// *memberUnreachableWithoutQuorumError when the cluster has lost quorum and
// lists the member offline, and nil when the read fails or the member is
// listed online.
func memberStopFromStatus(ctx context.Context, c Client, logger *log.Logger, node string, cause error) error {
	if c == nil || c.Cluster() == nil {
		return nil
	}
	q, err := ReadClusterQuorum(ctx, c.Cluster())
	if err != nil {
		if logger != nil {
			logger.Debug("ListGuestsAuthoritative: cluster status unreadable after a failed listing", log.Err(err))
		}
		return nil
	}
	switch {
	case !q.Offline[node]:
		return nil
	case q.Lost():
		return &memberUnreachableWithoutQuorumError{node: node, cause: cause}
	case q.Reported && q.Quorate:
		return &memberWentOfflineError{node: node, cause: cause}
	}
	return nil
}

// listGuestsAuthoritative is the shared fan-out. offline is the set of
// members to exclude (nil for the strict form); excluded reports the names
// actually skipped, in membership order. stop, when it isn't nil, is asked
// after each failed listing attempt whether to stop retrying that member. A
// *memberWentOfflineError excludes the member, and a
// *memberUnreachableWithoutQuorumError fails it at once. The strict form
// passes nil and gets a check that only stops a member the cluster lists
// offline after losing quorum, so an absence proof still rides out an outage
// on a quorate cluster through the whole retry budget, and it still fails
// when a member cannot be listed.
func listGuestsAuthoritative(
	ctx context.Context, c Client, logger *log.Logger, offline map[string]bool, stop func(string, error) error,
) ([]GuestRef, []string, error) {
	if ctx == nil {
		return nil, nil, cpierrors.Cloud("ListGuestsAuthoritative: ctx must not be nil")
	}
	if c == nil || c.Nodes() == nil {
		return nil, nil, cpierrors.Cloud("ListGuestsAuthoritative: nodes service unavailable")
	}

	// ListClusterMemberNames covers the never-clustered host: corosync
	// membership answers empty (or with a resolved API error) there, and the
	// helper falls back to GET /nodes, which names exactly the standalone
	// host itself.
	nodeNames, err := ListClusterMemberNames(ctx, c)
	if err != nil {
		return nil, nil, cpierrors.Wrap(err, "ListGuestsAuthoritative: enumerate cluster membership")
	}
	if len(nodeNames) == 0 {
		// Membership can never be legitimately empty on a live host; treat
		// it as the enumeration failing, not as "no guests".
		return nil, nil, cpierrors.Retriable("ListGuestsAuthoritative: cluster membership listing returned no nodes; cannot enumerate guests")
	}

	guests := make([]GuestRef, 0, 64)
	var excluded []string
	var failedNodes []string
	var failedErrs []error
	var noQuorum []string
	strict := offline == nil && stop == nil
	if stop == nil {
		stop = newQuorumLossProbe(ctx, c, logger).stop
	}
	listedAny := false
	seenNodes := make(map[string]bool, len(nodeNames))
	for _, node := range nodeNames {
		if seenNodes[node] {
			continue
		}
		seenNodes[node] = true
		if offline[node] {
			excluded = append(excluded, node)
			if logger != nil {
				logger.Warn("ListGuestsAuthoritative: excluding member the cluster reports offline",
					log.String("node", node))
			}
			continue
		}
		nodeGuests, listErr := listNodeGuests(ctx, c, logger, node, stop)
		var noQuorumErr *memberUnreachableWithoutQuorumError
		if errors.As(listErr, &noQuorumErr) {
			failedNodes = append(failedNodes, node)
			failedErrs = append(failedErrs, listErr)
			noQuorum = append(noQuorum, node)
			if logger != nil {
				logger.Warn("ListGuestsAuthoritative: member is offline and the cluster has no quorum; not retrying its listing",
					log.String("node", node), log.Err(noQuorumErr.cause))
			}
			continue
		}
		var offlineErr *memberWentOfflineError
		if errors.As(listErr, &offlineErr) {
			excluded = append(excluded, node)
			if logger != nil {
				logger.Warn("ListGuestsAuthoritative: excluding member the cluster began reporting offline during its listing",
					log.String("node", node), log.Err(offlineErr.cause))
			}
			continue
		}
		if listErr != nil {
			failedNodes = append(failedNodes, node)
			failedErrs = append(failedErrs, listErr)
			if logger != nil {
				logger.Warn("ListGuestsAuthoritative: node listing failed",
					log.String("node", node), log.Err(listErr))
			}
			continue
		}
		listedAny = true
		guests = append(guests, nodeGuests...)
	}
	if len(failedNodes) > 0 {
		// Only the strict form names the failed members the cluster reports
		// offline. The tolerant form leaves such a member out instead, so a
		// member it failed on was still reported online.
		var failedOffline []string
		if strict {
			failedOffline = failedMembersReportedOffline(ctx, c, logger, failedNodes)
		}
		return nil, nil, partialFleetError(failedNodes, failedErrs, failedOffline, noQuorum)
	}
	if !listedAny {
		// Every member excluded as offline: an empty result here would read
		// as "no guests" to a destructive caller, which is not a conclusion
		// a fully-dark cluster supports.
		return nil, nil, cpierrors.Retriable(
			"ListGuestsAuthoritative: every cluster member reports offline; cannot enumerate guests")
	}
	return guests, excluded, nil
}

// offlineClusterNodes reads /cluster/status best-effort and returns the set
// of member names the cluster itself reports offline. Any failure (status
// unavailable, unreachable, undecodable) returns nil, and so does a status
// that lacks a quorate cluster row: a cluster that cannot form a majority
// has no authority to declare which of its members are down, and a node on
// the minority side of a partition keeps serving GETs while its view
// reports every majority member offline. The caller then treats every
// member as online and keeps its fail-loud behavior, so this tolerance can
// only ever relax the enumeration on the quorate cluster's own authority,
// never on a guess.
func offlineClusterNodes(ctx context.Context, c Client, logger *log.Logger) map[string]bool {
	return readOfflineClusterNodes(ctx, c, logger, 0)
}

// readOfflineClusterNodes is offlineClusterNodes with the status read's
// attempt budget given; maxAttempts <= 0 takes RetryOnTransient's default.
func readOfflineClusterNodes(ctx context.Context, c Client, logger *log.Logger, maxAttempts int) map[string]bool {
	if c.Cluster() == nil {
		return nil
	}
	// RetryOnTransient absorbs a single blip (pvedaemon worker recycle) before
	// conceding: without it, one transient failure during a legitimate node
	// outage silently withholds the tolerance and turns what should be a
	// tolerated enumeration into a hard fail-loud, exactly when the tolerance
	// was supposed to keep FindVMAuthoritative / the tolerant enumeration
	// working. The direction stays fail-safe either way (withheld tolerance
	// only means fail-loud, never a wrong destroy), so an exhausted retry
	// still degrades to "treat every member as online" rather than
	// propagating an error.
	var resp *cluster.ListStatusResponse
	err := RetryOnTransient(ctx, logger, "offline_cluster_nodes", maxAttempts, func() error {
		var inner error
		resp, inner = c.Cluster().ListStatus(ctx)
		return inner
	})
	if err != nil || resp == nil {
		if err != nil && logger != nil {
			logger.Debug("ListGuestsAuthoritative: cluster status unavailable; treating every member as online",
				log.Err(err))
		}
		return nil
	}
	quorate := false
	var offline map[string]bool
	for _, raw := range *resp {
		var item struct {
			Type    string   `json:"type"`
			Name    string   `json:"name"`
			Online  *pveBool `json:"online"`
			Quorate *pveBool `json:"quorate"`
		}
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		if item.Type == statusRowTypeCluster {
			quorate = item.Quorate != nil && bool(*item.Quorate)
			continue
		}
		if item.Type != resourceTypeNode || item.Name == "" {
			continue
		}
		if item.Online != nil && !bool(*item.Online) {
			if offline == nil {
				offline = map[string]bool{}
			}
			offline[item.Name] = true
		}
	}
	if !quorate {
		if offline != nil && logger != nil {
			logger.Warn("ListGuestsAuthoritative: cluster status is not quorate; withholding offline-member tolerance")
		}
		return nil
	}
	return offline
}

// listNodeGuests lists one node's QEMU guests through RetryOnTransient and
// decodes them into GuestRefs. Malformed elements are skipped, not fatal
// (matches ResolveTemplateVMIDForNode), logged so schema drift leaves a
// trail. When stop is set and returns an error after a failed attempt, the
// retries stop with that error.
func listNodeGuests(
	ctx context.Context, c Client, logger *log.Logger, node string, stop func(string, error) error,
) ([]GuestRef, error) {
	var raws []json.RawMessage
	listErr := RetryOnTransient(ctx, logger, "list_guests_node", 0, func() error {
		resp, inner := c.Nodes().ListQemu(ctx, node, nil)
		if inner != nil {
			if stop != nil {
				if stopErr := stop(node, inner); stopErr != nil {
					return stopErr
				}
			}
			return inner
		}
		raws = nil
		if resp != nil {
			raws = *resp
		}
		return nil
	})
	if listErr != nil {
		return nil, listErr
	}
	guests := make([]GuestRef, 0, len(raws))
	for i, raw := range raws {
		var item qemuListItem
		if jsonErr := json.Unmarshal(raw, &item); jsonErr != nil {
			if logger != nil {
				logger.Debug("ListGuestsAuthoritative: skipping malformed qemu list entry",
					log.String("node", node), log.Int("index", i), log.Err(jsonErr))
			}
			continue
		}
		if item.Vmid.Int() <= 0 {
			continue
		}
		g := GuestRef{VMID: int(item.Vmid.Int()), Node: node}
		if item.Name != nil {
			g.Name = *item.Name
		}
		if item.Tags != nil {
			g.Tags = *item.Tags
		}
		if item.Status != nil {
			g.Status = *item.Status
		}
		g.Template = item.Template != nil && bool(*item.Template)
		guests = append(guests, g)
	}
	return guests, nil
}

// partialFleetError classifies an enumeration that could not list every
// node. A permanent API verdict (400 bad request, 403 missing grant) stays
// permanent: no number of retries adds the grant or fixes the request, and
// hiding it behind a retriable aggregate would spin the Director forever on
// a settled answer. Only a real API answer gets that treatment; an
// unclassifiable transport error (partition, refused connection) keeps the
// retriable default, because a rebooting or briefly partitioned node answers
// on the Director's next attempt, and concluding from a partial fleet would
// repeat the stale-index bug with a different source. Both shapes carry a
// GuestEnumerationError, so an audit can name the failed nodes without
// repeating the transport text. failedOffline names the failed members the
// quorate cluster reports offline, and only the retriable shape carries it,
// because a member that gave a real API answer was reachable. noQuorum names
// the failed members whose listing stopped early because the cluster has lost
// quorum and lists them offline, and only the retriable shape carries it too.
func partialFleetError(failedNodes []string, failedErrs []error, failedOffline, noQuorum []string) error {
	for i, fe := range failedErrs {
		if _, isAPIVerdict := apiHTTPCode(fe); !isAPIVerdict {
			continue
		}
		if cpierrors.IsType(WrapError(fe), cpierrors.TypeRetriableCloud) {
			continue
		}
		return cpierrors.Wrap(&GuestEnumerationError{Nodes: []string{failedNodes[i]}, Cause: WrapError(fe)}, "ListGuestsAuthoritative")
	}
	return cpierrors.WrapAs(&GuestEnumerationError{Nodes: failedNodes, Offline: failedOffline, NoQuorum: noQuorum},
		cpierrors.TypeRetriableCloud, "ListGuestsAuthoritative")
}
