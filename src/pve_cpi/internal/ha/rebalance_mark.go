package ha

import (
	"context"
	"fmt"
	"strings"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// RebalanceMarkKey opens the token pve-ops ha rebalance appends to the comment
// of a VM's HA resource, in the same write that turns failback off, before it
// asks PVE HA to migrate the VM. The token names the node the VM moves to. It
// holds failback off for as long as it is there, with no expiry, because the
// VM's pin may still prefer the node the VM leaves until the rebalance writes
// the pin that names the target. The rebalance drops the token in the write
// that restores failback, and a rebalance that stopped part way leaves it for
// the next run to finish, which the doctor reports as rebalance_pending.
const RebalanceMarkKey = "bosh-rebalance-to="

// RebalancePending returns the node the rebalance token on comment names, and
// whether the comment carries one. A token with no node names nothing.
func RebalancePending(comment *string) (string, bool) {
	if comment == nil {
		return "", false
	}
	for _, f := range strings.Fields(*comment) {
		if node, ok := strings.CutPrefix(f, RebalanceMarkKey); ok && node != "" {
			return node, true
		}
	}
	return "", false
}

// WithRebalanceMark returns comment with a rebalance token that names node,
// replacing any rebalance token it carried. The rest of the comment, which
// starts with the ownership marker, is kept in order.
func WithRebalanceMark(comment, node string) string {
	stripped, _ := WithoutRebalanceMark(comment)
	if stripped == "" {
		return RebalanceMarkKey + node
	}
	return stripped + " " + RebalanceMarkKey + node
}

// WithoutRebalanceMark returns comment without its rebalance token, and
// whether it carried one. The rest of the comment is kept in order.
func WithoutRebalanceMark(comment string) (string, bool) {
	fields := strings.Fields(comment)
	kept := make([]string, 0, len(fields))
	for _, f := range fields {
		if !strings.HasPrefix(f, RebalanceMarkKey) {
			kept = append(kept, f)
		}
	}
	return strings.Join(kept, " "), len(kept) != len(fields)
}

// MarkRebalance turns failback off on the resource sid and puts a rebalance
// token that names node on its comment, in one write that names the digest of
// read, the caller's read of the resource. It reports whether the write
// landed. A write PVE refuses because resources.cfg changed after read lands
// nothing and reports false with no error, and the caller decides afresh. An
// empty digest writes nothing, because the token must never replace a token
// another writer added unchecked. Any other failure is returned.
func MarkRebalance(ctx context.Context, api FailbackAPI, sid string, read *sdkcluster.GetHaResourcesResponse,
	node string,
) (bool, error) {
	if api == nil || read == nil || read.Comment == nil || node == "" {
		return false, cpierrors.Cloud("ha rebalance: the resource client, the read of the resource, and the target " +
			"node must be set")
	}
	off := false
	comment := WithRebalanceMark(*read.Comment, node)
	return writeAgainstRead(ctx, api, sid, read, &sdkcluster.UpdateHaResourcesParams{Failback: &off,
		Comment: &comment}, "set failback off and mark the move to "+node+" on")
}

// UnmarkRebalance sets failback to failback on the resource sid and drops the
// rebalance token from its comment, in one write that names the digest of
// read, the caller's read of the resource. It reports whether the write
// landed, and a write PVE refuses because resources.cfg changed after read
// reports false with no error, so the caller reads and decides afresh. The
// token and failback off then stay, which keeps the VM where it is. An empty
// digest writes nothing. Any other failure is returned.
func UnmarkRebalance(ctx context.Context, api FailbackAPI, sid string, read *sdkcluster.GetHaResourcesResponse,
	failback bool,
) (bool, error) {
	if api == nil || read == nil || read.Comment == nil {
		return false, cpierrors.Cloud("ha rebalance: the resource client and the read of the resource must be set")
	}
	comment, _ := WithoutRebalanceMark(*read.Comment)
	return writeAgainstRead(ctx, api, sid, read, &sdkcluster.UpdateHaResourcesParams{Failback: &failback,
		Comment: &comment}, fmt.Sprintf("set failback %v and drop the rebalance mark from", failback))
}

// writeAgainstRead sends params to the resource sid with the digest of read,
// and reports whether the write landed. A refusal because resources.cfg
// changed, and a read that named no digest, land nothing and return no error.
func writeAgainstRead(ctx context.Context, api FailbackAPI, sid string, read *sdkcluster.GetHaResourcesResponse,
	params *sdkcluster.UpdateHaResourcesParams, what string,
) (bool, error) {
	if read.Digest == "" {
		return false, nil
	}
	digest := read.Digest
	params.Digest = &digest
	err := api.UpdateHaResources(ctx, sid, params)
	switch {
	case err == nil:
		return true, nil
	case IsDigestConflict(err):
		return false, nil
	}
	return false, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
		fmt.Sprintf("ha rebalance: %s the HA resource %s", what, sid))
}
