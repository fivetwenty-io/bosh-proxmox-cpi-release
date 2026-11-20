package ha

import (
	"context"
	"fmt"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// The failback flag of an HA resource makes PVE HA move the resource back to
// the highest-priority node of its node-affinity rule once that node can take
// it again, a node leaving maintenance included. PVE ranks the nodes of a rule
// before it applies resource affinity, and a strict rule leaves PVE no node
// outside it, so failback never moves a VM to a node its pin forbids or onto a
// node that holds a member of its anti-affinity group.
//
// The CPI gives failback only to a VM whose every volume is on shared storage.
// A replicate VM keeps it off: after a failover PVE reverses the replication
// job, and its first run back to the old source rolls that node's volumes back
// with zfs recv -F, so moving the VM back there loses the writes made since
// the failover. A hold VM and a VM with a volume on node-local storage keep it
// off too, because there is no other node their volumes can follow them to.

// FailbackAPI is what ReconcileFailback reads and writes.
type FailbackAPI interface {
	GetHaResources(ctx context.Context, sid string) (*sdkcluster.GetHaResourcesResponse, error)
	UpdateHaResources(ctx context.Context, sid string, params *sdkcluster.UpdateHaResourcesParams) error
}

// ReconcileFailback sets the failback flag of the resource sid to want when it
// reads otherwise, and reports whether it wrote. It leaves alone, and reports
// no change for, a resource that is missing and one whose comment owned does
// not accept. A resource a narrowing barrier holds is written too, because the
// barrier sets only the requested state and the comment, and its fallback
// registers the resource again with the flags the caller's decision names. It
// never turns failback on while the comment holds it off with a fresh hold token or a rebalance token (see
// FailbackAllowed), and it then reports no change. A read or a write that
// fails is returned.
func ReconcileFailback(ctx context.Context, api FailbackAPI, sid string, want bool, owned func(string) bool) (
	bool, error,
) {
	if api == nil || owned == nil {
		return false, cpierrors.Cloud("ha failback: the resource client and the ownership check must be set")
	}
	got, err := api.GetHaResources(ctx, sid)
	switch {
	case err != nil && isResourceNotFound(err):
		return false, nil
	case err != nil:
		return false, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("ha failback: read the HA resource %s", sid))
	case got == nil:
		return false, cpierrors.Retriable("ha failback: the read of the HA resource %s returned nothing", sid)
	case got.Comment == nil || !owned(*got.Comment):
		return false, nil
	case FailbackOf(got.Failback) == want:
		return false, nil
	case want && !FailbackAllowed(want, *got.Comment, time.Now()):
		return false, nil
	}
	if err := api.UpdateHaResources(ctx, sid, &sdkcluster.UpdateHaResourcesParams{Failback: &want}); err != nil {
		return false, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			fmt.Sprintf("ha failback: set failback %v on the HA resource %s", want, sid))
	}
	return true, nil
}

// RaiseFailback turns failback on for the resource sid only while
// resources.cfg still has digest, the digest of the read the caller decided
// from. PVE checks the digest under its HA domain lock, so a write that any
// other writer of resources.cfg came before, such as an attach that turned
// failback off or a narrowing barrier that holds the resource, is refused, and
// RaiseFailback reports that it wrote nothing. An empty digest writes nothing
// either, because the raise must never land unchecked. Any other failure is
// returned.
func RaiseFailback(ctx context.Context, api FailbackAPI, sid, digest string) (bool, error) {
	if api == nil {
		return false, cpierrors.Cloud("ha failback: the resource client must be set")
	}
	if digest == "" {
		return false, nil
	}
	on := true
	err := api.UpdateHaResources(ctx, sid, &sdkcluster.UpdateHaResourcesParams{Failback: &on, Digest: &digest})
	switch {
	case err == nil:
		return true, nil
	case IsDigestConflict(err):
		return false, nil
	}
	return false, cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
		fmt.Sprintf("ha failback: set failback on the HA resource %s", sid))
}

// FailbackFacts are what the expected failback flag of a CPI resource rests on
// when no HA decision is at hand, which is how the doctor judges a resource.
type FailbackFacts struct {
	// Enabled is the effective pve.ha.failback.
	Enabled bool

	// Replicate is true when the VM has a replication job of this Director,
	// or its pin carries a token of the replicate policy.
	Replicate bool

	// Local is true when a volume other than an ISO is on node-local storage.
	Local bool

	// Unknown is true when the storage of a volume could not be judged.
	Unknown bool

	// SelfPinned is true when the VM's pin is strict and names one node, the
	// pin a hold VM gets. Failback has no node to move such a VM to.
	SelfPinned bool
}

// ExpectedFailback returns the failback flag a CPI resource with the facts f
// should carry. known is false when the facts do not settle it: a storage
// that could not be judged, or a strict self pin, which the HA decision gives
// both a hold VM and a VM only one node can take, and under which failback
// moves nothing.
func ExpectedFailback(f FailbackFacts) (want, known bool) {
	switch {
	case !f.Enabled, f.Replicate, f.Local:
		return false, true
	case f.Unknown, f.SelfPinned:
		return false, false
	}
	return true, true
}
