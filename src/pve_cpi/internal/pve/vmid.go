package pve

import (
	"context"
	"encoding/json"
	"fmt"
	mrand "math/rand/v2"
	"regexp"
	"sync"
	"time"

	sdkcluster "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

// Default VMID ranges.
// VM range expanded to 5999; the former stemcell sub-range [5000,5999] is now
// part of the general VM pool. Disk synthetic VMIDs remain in [9000,9999].
const (
	VMIDRangeVMStart   = 100
	VMIDRangeVMEnd     = 5999
	VMIDRangeDiskStart = 9000
	VMIDRangeDiskEnd   = 9999
)

// allocOpts holds resolved configuration for a single NextVMID call or
// AllocateWithRetry invocation. Backoff fields are consumed only by the retry
// helpers; NextVMID ignores them.
type allocOpts struct {
	rangeStart int
	rangeEnd   int
	noBackoff  bool
}

// AllocOption is a functional option for NextVMID and the retry helpers.
type AllocOption func(*allocOpts)

// WithRange sets a custom inclusive VMID range [start, end].
// start must be ≥ 100 and end must be > start. Invalid values are silently
// ignored and the caller-supplied defaults apply.
func WithRange(start, end int) AllocOption {
	return func(o *allocOpts) {
		if start >= 100 && end > start {
			o.rangeStart = start
			o.rangeEnd = end
		}
	}
}

// WithNoBackoff disables the jittered sleep between retry attempts in
// AllocateWithRetry / AllocateDiskWithRetry. Tests use this so the retry
// loop stays deterministic.
func WithNoBackoff() AllocOption {
	return func(o *allocOpts) {
		o.noBackoff = true
	}
}

// retryBackoff sleeps a small jittered duration to decorrelate retry herds
// across concurrent CPI processes. Returns the chosen duration so callers
// can log it before sleeping. Returns 0 when the sleep is skipped.
func retryBackoff(noBackoff bool) time.Duration {
	if noBackoff {
		return 0
	}
	// Uniform 50–250 ms.
	d := 50*time.Millisecond + time.Duration(mrand.Int64N(int64(200*time.Millisecond)))
	time.Sleep(d)
	return d
}

// globalVMIDMu is the process-level mutex that serialises VMID allocation.
// It prevents two goroutines within the same CPI process from reading the
// cluster VM list simultaneously and returning the same VMID.
// Cross-process races (two CPI invocations) are handled by the caller's
// retry-on-conflict pattern (PVE rejects duplicate VMID creation).
var globalVMIDMu sync.Mutex

// vmidEntry is a minimal struct used to extract the vmid field from
// cluster/resources or nodes/qemu JSON items.
type vmidEntry struct {
	Vmid *int64 `json:"vmid"`
}

// listClusterVMIDs calls GET /cluster/resources?type=vm and returns the set of
// all VMID integers currently registered in the cluster (across all nodes).
// Returns a non-nil error wrapped as *cpierrors.Error on any failure.
func listClusterVMIDs(ctx context.Context, c Client) (map[int]struct{}, error) {
	typeStr := "vm"
	resp, err := c.Cluster().ListResources(ctx, &sdkcluster.ListResourcesParams{Type: &typeStr})
	if err != nil {
		return nil, cpierrors.Wrap(err, "vmid: list cluster resources")
	}
	if resp == nil {
		return nil, cpierrors.Cloud("vmid: nil response from cluster resources")
	}

	used := make(map[int]struct{}, len(*resp))
	for _, raw := range *resp {
		var entry vmidEntry
		if jsonErr := json.Unmarshal(raw, &entry); jsonErr != nil {
			// Skip malformed entries; a single bad item should not abort allocation.
			continue
		}
		if entry.Vmid != nil {
			used[int(*entry.Vmid)] = struct{}{}
		}
	}
	return used, nil
}

// nextVMIDInRange scans [start, end] and returns the lowest VMID not present
// in used. Returns a *cpierrors.Error if the entire range is exhausted.
func nextVMIDInRange(used map[int]struct{}, start, end int) (int, error) {
	for candidate := start; candidate <= end; candidate++ {
		if _, taken := used[candidate]; !taken {
			return candidate, nil
		}
	}
	return 0, cpierrors.Cloud("no free VMID in range [%d, %d]: all %d IDs exhausted",
		start, end, end-start+1)
}

// NextVMID returns the lowest free VMID in the range specified by opts (default
// [VMIDRangeVMStart, VMIDRangeVMEnd]). The cluster VM list is fetched under a
// process-level mutex to prevent within-process races. Cross-process races are
// handled at the caller layer via retry-on-conflict.
//
// Inputs and failure modes:
//   - ctx nil → returns *cpierrors.Error before any SDK call.
//   - c nil → returns *cpierrors.Error before any SDK call.
//   - SDK failure → returns *cpierrors.Error wrapping the SDK error.
//   - Range exhausted → returns *cpierrors.Error "no free VMID in range".
func NextVMID(ctx context.Context, c Client, opts ...AllocOption) (int, error) {
	if ctx == nil {
		return 0, cpierrors.Cloud("NextVMID: ctx must not be nil")
	}
	if c == nil {
		return 0, cpierrors.Cloud("NextVMID: client must not be nil")
	}

	ao := &allocOpts{
		rangeStart: VMIDRangeVMStart,
		rangeEnd:   VMIDRangeVMEnd,
	}
	for _, opt := range opts {
		opt(ao)
	}

	globalVMIDMu.Lock()
	defer globalVMIDMu.Unlock()

	used, err := listClusterVMIDs(ctx, c)
	if err != nil {
		return 0, err
	}

	return nextVMIDInRange(used, ao.rangeStart, ao.rangeEnd)
}

// NextDiskVMID returns the lowest free VMID in [VMIDRangeDiskStart, VMIDRangeDiskEnd].
// Disk VMIDs are synthetic identifiers used for unattached persistent volumes
// managed as QEMU VMs (for snapshotting support).
//
// node and storage may be empty: when set, the function ALSO unions VMIDs
// extracted from volume names ("vm-9NNN-disk-N") on that storage into the
// "used" set. This is critical for synthetic VMIDs because the cluster VM
// list does NOT include orphaned persistent disks — without the storage
// scan, a stale "vm-9000-disk-0" left behind on storage would be invisible
// to NextDiskVMID and the next call would happily return 9000 again,
// colliding on lvcreate.
func NextDiskVMID(ctx context.Context, c Client, node, storage string) (int, error) {
	if ctx == nil {
		return 0, cpierrors.Cloud("NextDiskVMID: ctx must not be nil")
	}
	if c == nil {
		return 0, cpierrors.Cloud("NextDiskVMID: client must not be nil")
	}

	globalVMIDMu.Lock()
	defer globalVMIDMu.Unlock()

	used, err := listClusterVMIDs(ctx, c)
	if err != nil {
		return 0, err
	}

	if node != "" && storage != "" {
		storageUsed, sErr := listStorageVMIDs(ctx, c, node, storage)
		if sErr != nil {
			return 0, sErr
		}
		for id := range storageUsed {
			used[id] = struct{}{}
		}
	}

	return nextVMIDInRange(used, VMIDRangeDiskStart, VMIDRangeDiskEnd)
}

// volumeVMIDRegexp matches PVE volume names of the form "vm-NNN-disk-N",
// extracting the VMID. Anchored on each side so we don't accidentally match
// substrings.
var volumeVMIDRegexp = regexp.MustCompile(`(?:^|[/:])vm-(\d+)-disk-\d+(?:\.\w+)?$`)

// listStorageVMIDs returns the set of VMIDs that have at least one volume
// named "vm-{vmid}-disk-{N}" on the given storage. Non-VM-disk volumes
// (ISOs, backups, snippets) are skipped because they don't follow the
// vm-N-disk-N naming convention.
//
// On API error returns a wrapped *cpierrors.Error. An empty content list
// is not an error — returns an empty map.
func listStorageVMIDs(ctx context.Context, c Client, node, storage string) (map[int]struct{}, error) {
	nodesSvc := c.Nodes()
	if nodesSvc == nil {
		// Test fixtures may stub Nodes() as nil; treat as no observed
		// volumes. Production sdkClient.Nodes() is always non-nil.
		return map[int]struct{}{}, nil
	}
	resp, err := nodesSvc.ListStorageContent(ctx, node, storage, nil)
	if err != nil {
		return nil, cpierrors.Wrap(err, fmt.Sprintf("vmid: list storage %q content on node %q", storage, node))
	}
	used := make(map[int]struct{})
	if resp == nil {
		return used, nil
	}
	for _, raw := range *resp {
		var entry struct {
			VolID    string `json:"volid"`
			Filename string `json:"filename"`
		}
		if jsonErr := json.Unmarshal(raw, &entry); jsonErr != nil {
			continue
		}
		// Prefer volid ("storage:vm-9000-disk-0"); fall back to filename.
		target := entry.VolID
		if target == "" {
			target = entry.Filename
		}
		if target == "" {
			continue
		}
		matches := volumeVMIDRegexp.FindStringSubmatch(target)
		if len(matches) < 2 {
			continue
		}
		var vmid int
		if _, scanErr := fmt.Sscanf(matches[1], "%d", &vmid); scanErr != nil {
			continue
		}
		if vmid > 0 {
			used[vmid] = struct{}{}
		}
	}
	return used, nil
}

// AllocateWithRetry wraps a VMID allocation + create operation with up to
// maxAttempts retries on VMID conflict. On each attempt it calls NextVMID
// (which re-fetches the cluster list under the mutex) then calls create.
// If create returns an error for which isConflict returns true, the allocation
// is retried with a fresh cluster list after a short jittered sleep that
// decorrelates retry herds across concurrent CPI processes. Any other error
// is returned immediately.
//
// On final exhaustion the error message includes the last VMID attempted so
// operator logs surface the contended range.
//
// Returns 0 and a *cpierrors.Error if:
//   - all maxAttempts are exhausted.
//   - NextVMID fails (range exhausted, SDK error, etc.).
//   - create returns a non-conflict error.
func AllocateWithRetry(
	ctx context.Context,
	c Client,
	create func(vmid int) error,
	isConflict func(err error) bool,
	maxAttempts int,
	opts ...AllocOption,
) (int, error) {
	if ctx == nil {
		return 0, cpierrors.Cloud("AllocateWithRetry: ctx must not be nil")
	}
	if c == nil {
		return 0, cpierrors.Cloud("AllocateWithRetry: client must not be nil")
	}
	if create == nil {
		return 0, cpierrors.Cloud("AllocateWithRetry: create func must not be nil")
	}
	if maxAttempts <= 0 {
		maxAttempts = 3
	}

	ao := &allocOpts{}
	for _, opt := range opts {
		opt(ao)
	}

	var lastVMID int
	for attempt := 0; attempt < maxAttempts; attempt++ {
		vmid, err := NextVMID(ctx, c, opts...)
		if err != nil {
			return 0, err
		}
		lastVMID = vmid
		if createErr := create(vmid); createErr != nil {
			if isConflict != nil && isConflict(createErr) {
				if attempt < maxAttempts-1 {
					retryBackoff(ao.noBackoff)
				}
				continue
			}
			return 0, cpierrors.Wrap(createErr, fmt.Sprintf("create VMID %d", vmid))
		}
		return vmid, nil
	}

	return 0, cpierrors.Cloud(
		"AllocateWithRetry: failed to allocate VMID after %d attempts (last attempted VMID %d)",
		maxAttempts, lastVMID,
	)
}

// AllocateDiskWithRetry mirrors AllocateWithRetry for disk-range VMIDs. On
// each attempt it calls NextDiskVMID(ctx, c, node, storage) so the
// storage-volume scan re-runs every iteration and orphan volumes from prior
// failed attempts remain visible. Same backoff/jitter treatment as
// AllocateWithRetry.
func AllocateDiskWithRetry(
	ctx context.Context,
	c Client,
	node, storage string,
	create func(vmid int) error,
	isConflict func(err error) bool,
	maxAttempts int,
	opts ...AllocOption,
) (int, error) {
	if ctx == nil {
		return 0, cpierrors.Cloud("AllocateDiskWithRetry: ctx must not be nil")
	}
	if c == nil {
		return 0, cpierrors.Cloud("AllocateDiskWithRetry: client must not be nil")
	}
	if create == nil {
		return 0, cpierrors.Cloud("AllocateDiskWithRetry: create func must not be nil")
	}
	if maxAttempts <= 0 {
		maxAttempts = 3
	}

	ao := &allocOpts{}
	for _, opt := range opts {
		opt(ao)
	}

	var lastVMID int
	for attempt := 0; attempt < maxAttempts; attempt++ {
		vmid, err := NextDiskVMID(ctx, c, node, storage)
		if err != nil {
			return 0, err
		}
		lastVMID = vmid
		if createErr := create(vmid); createErr != nil {
			if isConflict != nil && isConflict(createErr) {
				if attempt < maxAttempts-1 {
					retryBackoff(ao.noBackoff)
				}
				continue
			}
			return 0, cpierrors.Wrap(createErr, fmt.Sprintf("create disk VMID %d", vmid))
		}
		return vmid, nil
	}

	return 0, cpierrors.Cloud(
		"AllocateDiskWithRetry: failed to allocate disk VMID after %d attempts (last attempted VMID %d)",
		maxAttempts, lastVMID,
	)
}
