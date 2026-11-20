package ha

import (
	"context"
	"strings"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// DrainMarkKey opens the token pve-ops ha drain appends to the comment of a
// VM's HA resource, in the same write that sets the resource's state to
// stopped, when the VM can't leave a node in maintenance. The token names the
// drained node. pve-ops ha undrain finds the VMs to start again by it, and it
// drops the token in the same write that sets the state back to started, so a
// VM is never stopped by a drain without the token, nor started with it.
const DrainMarkKey = "bosh-drain-stopped="

// DrainStopped returns the node the drain token on comment names, and whether
// the comment carries one. A token with no node names nothing.
func DrainStopped(comment *string) (string, bool) {
	if comment == nil {
		return "", false
	}
	for _, f := range strings.Fields(*comment) {
		if node, ok := strings.CutPrefix(f, DrainMarkKey); ok && node != "" {
			return node, true
		}
	}
	return "", false
}

// WithDrainMark returns comment with a drain token that names node, replacing
// any drain token it carried. The rest of the comment, which starts with the
// ownership marker, is kept in order.
func WithDrainMark(comment, node string) string {
	stripped, _ := WithoutDrainMark(comment)
	if stripped == "" {
		return DrainMarkKey + node
	}
	return stripped + " " + DrainMarkKey + node
}

// WithoutDrainMark returns comment without its drain token, and whether it
// carried one. The rest of the comment is kept in order.
func WithoutDrainMark(comment string) (string, bool) {
	fields := strings.Fields(comment)
	kept := make([]string, 0, len(fields))
	for _, f := range fields {
		if !strings.HasPrefix(f, DrainMarkKey) {
			kept = append(kept, f)
		}
	}
	return strings.Join(kept, " "), len(kept) != len(fields)
}

// StopForDrain sets the state of the resource sid to stopped and puts a drain
// token that names node on its comment, in one write that names the digest of
// read, the caller's read of the resource. PVE HA then shuts the VM down. It
// reports whether the write landed. A write PVE refuses because resources.cfg
// changed after read lands nothing and reports false with no error, so the
// caller reads and decides afresh. An empty digest writes nothing, because
// the token must never replace a token another writer added unchecked. Any
// other failure is returned.
func StopForDrain(ctx context.Context, api FailbackAPI, sid string, read *sdkcluster.GetHaResourcesResponse,
	node string,
) (bool, error) {
	if api == nil || read == nil || read.Comment == nil || node == "" {
		return false, cpierrors.Cloud("ha drain: the resource client, the read of the resource, and the " +
			"drained node must be set")
	}
	stopped := "stopped"
	comment := WithDrainMark(*read.Comment, node)
	return writeAgainstRead(ctx, api, sid, read, &sdkcluster.UpdateHaResourcesParams{State: &stopped,
		Comment: &comment}, "ha drain: set state stopped and mark the drain of "+node+" on")
}

// StartAfterDrain sets the state of the resource sid to started and drops the
// drain token from its comment, in one write that names the digest of read,
// the caller's read of the resource. It reports whether the write landed, and
// a write PVE refuses because resources.cfg changed after read reports false
// with no error, so the caller reads and decides afresh. An empty digest
// writes nothing. Any other failure is returned.
func StartAfterDrain(ctx context.Context, api FailbackAPI, sid string, read *sdkcluster.GetHaResourcesResponse,
) (bool, error) {
	if api == nil || read == nil || read.Comment == nil {
		return false, cpierrors.Cloud("ha undrain: the resource client and the read of the resource must be set")
	}
	started := "started"
	comment, _ := WithoutDrainMark(*read.Comment)
	return writeAgainstRead(ctx, api, sid, read, &sdkcluster.UpdateHaResourcesParams{State: &started,
		Comment: &comment}, "ha undrain: set state started and drop the drain mark from")
}
