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
	"fmt"
	"strings"

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
func ListGuestsAuthoritative(ctx context.Context, c Client, logger *log.Logger) ([]GuestRef, error) {
	guests, _, err := listGuestsAuthoritative(ctx, c, logger, nil)
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
	return listGuestsAuthoritative(ctx, c, logger, offlineClusterNodes(ctx, c, logger))
}

// listGuestsAuthoritative is the shared fan-out. offline is the set of
// members to exclude (nil for the strict form); excluded reports the names
// actually skipped, in membership order.
func listGuestsAuthoritative(ctx context.Context, c Client, logger *log.Logger, offline map[string]bool) ([]GuestRef, []string, error) {
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
		listedAny = true
		nodeGuests, listErr := listNodeGuests(ctx, c, logger, node)
		if listErr != nil {
			failedNodes = append(failedNodes, node)
			failedErrs = append(failedErrs, listErr)
			if logger != nil {
				logger.Warn("ListGuestsAuthoritative: node listing failed",
					log.String("node", node), log.Err(listErr))
			}
			continue
		}
		guests = append(guests, nodeGuests...)
	}
	if len(failedNodes) > 0 {
		return nil, nil, partialFleetError(failedNodes, failedErrs)
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
	if c.Cluster() == nil {
		return nil
	}
	resp, err := c.Cluster().ListStatus(ctx)
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
// trail.
func listNodeGuests(ctx context.Context, c Client, logger *log.Logger, node string) ([]GuestRef, error) {
	var raws []json.RawMessage
	listErr := RetryOnTransient(ctx, logger, "list_guests_node", 0, func() error {
		resp, inner := c.Nodes().ListQemu(ctx, node, nil)
		if inner != nil {
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
		if item.Vmid <= 0 {
			continue
		}
		g := GuestRef{VMID: int(item.Vmid), Node: node}
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
// repeat the stale-index bug with a different source.
func partialFleetError(failedNodes []string, failedErrs []error) error {
	for i, fe := range failedErrs {
		if _, isAPIVerdict := apiHTTPCode(fe); !isAPIVerdict {
			continue
		}
		if cpierrors.IsType(WrapError(fe), cpierrors.TypeRetriableCloud) {
			continue
		}
		return cpierrors.Wrap(WrapError(fe), fmt.Sprintf(
			"ListGuestsAuthoritative: could not list guests on node %s; refusing to decide from a partial fleet",
			failedNodes[i]))
	}
	return cpierrors.Retriable(
		"ListGuestsAuthoritative: could not list guests on node(s) %s; refusing to decide from a partial fleet",
		strings.Join(failedNodes, ","))
}
