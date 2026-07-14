package pve

import (
	"context"
	mrand "math/rand/v2"
	"sync"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

// globalVNIMu is the process-level mutex that serialises VNI allocation,
// mirroring globalVMIDMu. It prevents two goroutines within the same CPI
// process from reading the vnet list simultaneously and returning the same
// VNI. Cross-process races are handled by the caller's conflict handling
// (PVE rejects duplicate tags within a zone).
var globalVNIMu sync.Mutex

// listUsedVNIs returns the set of vnet tags currently in use cluster-wide.
// Pending vnets are included (ListSDNVnets passes pending=true) so tags on
// staged-but-unapplied vnets are never double-allocated. Untagged vnets
// (Tag == 0) are skipped — 0 is not a valid VNI.
func listUsedVNIs(ctx context.Context, c Client) (map[int]struct{}, error) {
	vnets, err := ListSDNVnets(ctx, c)
	if err != nil {
		return nil, cpierrors.Wrap(err, "vni: list vnets")
	}
	used := make(map[int]struct{}, len(vnets))
	for _, v := range vnets {
		if v.Tag != 0 {
			used[int(v.Tag)] = struct{}{}
		}
	}
	return used, nil
}

// nextVNIInRange picks a random start offset within [start, end] and scans the
// full range exactly once (wrapping at end back to start), returning the first
// VNI not present in used. Randomising the entry point scatters concurrent CPI
// processes across the band, mirroring nextVMIDInRange.
func nextVNIInRange(used map[int]struct{}, start, end int) (int, error) {
	if end < start {
		return 0, cpierrors.Cloud("no free VNI in range [%d, %d]: invalid range (end < start)",
			start, end)
	}
	width := end - start + 1
	randomOffset := mrand.IntN(width) // #nosec G404 -- VNI collision-avoidance offset; non-cryptographic
	for i := 0; i < width; i++ {
		candidate := start + (randomOffset+i)%width
		if _, taken := used[candidate]; !taken {
			return candidate, nil
		}
	}
	return 0, cpierrors.Cloud(
		"no free VNI in range [%d, %d]: all %d IDs exhausted — widen sdn_vni_range_start/sdn_vni_range_end "+
			"or set cloud_properties.vnet_tag explicitly",
		start, end, width)
}

// NextVNI returns a free vnet tag (VNI) within [start, end], consulting the
// cluster-wide vnet list for tags already in use. The vnet list is fetched
// outside the process-level mutex so a slow PVE API call does not block other
// goroutines; the mutex is held only around the in-memory scan+select.
//
// Inputs and failure modes:
//   - ctx nil / c nil → *cpierrors.Error before any SDK call.
//   - list failure → wrapped *cpierrors.Error.
//   - band exhausted → *cpierrors.Error naming both config keys and the
//     cloud_properties.vnet_tag escape hatch.
func NextVNI(ctx context.Context, c Client, start, end int) (int, error) {
	if ctx == nil {
		return 0, cpierrors.Cloud("NextVNI: ctx must not be nil")
	}
	if c == nil {
		return 0, cpierrors.Cloud("NextVNI: client must not be nil")
	}
	used, err := listUsedVNIs(ctx, c)
	if err != nil {
		return 0, err
	}
	globalVNIMu.Lock()
	defer globalVNIMu.Unlock()
	return nextVNIInRange(used, start, end)
}
