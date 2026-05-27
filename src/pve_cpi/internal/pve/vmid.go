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
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"

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
	// backoffFn, when set, is consulted by the retry helpers to compute
	// the sleep duration between attempts. attempt is 0-indexed (so the
	// pause that follows the first failure has attempt=0). The error
	// argument is the retryable error returned by the create callback so
	// the caller can apply a longer backoff for, e.g., storage lock
	// timeouts vs. VMID conflicts.
	backoffFn func(err error, attempt int) time.Duration
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

// WithBackoffFunc installs a custom backoff function that is consulted
// between retry attempts. attempt is 0-indexed. The returned duration is
// slept verbatim (return 0 to skip). WithNoBackoff takes precedence over
// any installed function.
func WithBackoffFunc(fn func(err error, attempt int) time.Duration) AllocOption {
	return func(o *allocOpts) {
		o.backoffFn = fn
	}
}

// retryBackoff sleeps for a duration chosen by ao's backoff configuration
// and returns the slept duration so callers can log it. The order of
// precedence is: noBackoff (returns 0) > backoffFn > default jitter.
//
// The sleep is context-aware: if ctx is cancelled or its deadline expires
// before the sleep completes, retryBackoff returns ctx.Err() immediately
// so callers stop retrying without waiting for the full sleep duration.
//
// Inputs:
//   - ctx: must not be nil; passed through from the outer VMID allocation call.
//   - ao: may be nil (treated as no override — default jitter applies).
//   - err: the error from the last failed attempt; forwarded to backoffFn.
//   - attempt: 0-indexed retry count.
//
// Failure modes:
//   - ctx cancelled/deadline exceeded before sleep completes → returns ctx.Err().
//   - noBackoff set → returns nil immediately (d == 0).
//   - normal completion → returns nil.
func retryBackoff(ctx context.Context, ao *allocOpts, err error, attempt int) error {
	if ao != nil && ao.noBackoff {
		return nil
	}
	var d time.Duration
	if ao != nil && ao.backoffFn != nil {
		d = ao.backoffFn(err, attempt)
	} else {
		// Default: uniform 50–250 ms.
		d = 50*time.Millisecond + time.Duration(mrand.Int64N(int64(200*time.Millisecond))) // #nosec G404 -- VMID collision-avoidance jitter; non-cryptographic
	}
	if d <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
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
//
// The cluster-resources call is wrapped in RetryOnTransient to absorb the
// pvedaemon-worker-recycle window: under burst load the worker holding our
// TCP connection may exit (request-quota or memory limit), surfacing as
// HTTP 596 or an auth-EOF on the next call. A fresh worker spawns within
// roughly a second, so a short retry usually wins.
func listClusterVMIDs(ctx context.Context, c Client) (map[int]struct{}, error) {
	typeStr := "vm"
	var resp *sdkcluster.ListResourcesResponse
	err := RetryOnTransient(ctx, nil, "vmid_list_cluster", 0, func() error {
		var inner error
		resp, inner = c.Cluster().ListResources(ctx, &sdkcluster.ListResourcesParams{Type: &typeStr})
		return inner
	})
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

// nextVMIDInRange picks a random start offset within [start, end] and scans
// the full range exactly once (wrapping at end back to start), returning the
// first VMID not present in used. Randomising the entry point scatters
// concurrent CPI processes across the VMID space, reducing cross-process
// collision probability to roughly 1/(end-start+1) per process pair while
// keeping AllocateWithRetry as the backstop for the rare collision that still
// occurs.
//
// Inputs and failure modes:
//   - used nil is treated as an empty set (all IDs free).
//   - end < start → returns *cpierrors.Error immediately (defensive guard;
//     callers are expected to pass a valid range, but we never panic).
//   - range fully exhausted → returns *cpierrors.Error with same message as
//     before so callers / tests that match the string remain unaffected.
func nextVMIDInRange(used map[int]struct{}, start, end int) (int, error) {
	if end < start {
		return 0, cpierrors.Cloud("no free VMID in range [%d, %d]: all %d IDs exhausted",
			start, end, 0)
	}
	width := end - start + 1
	randomOffset := mrand.IntN(width) // #nosec G404 -- VMID collision-avoidance offset; non-cryptographic
	for i := 0; i < width; i++ {
		candidate := start + (randomOffset+i)%width
		if _, taken := used[candidate]; !taken {
			return candidate, nil
		}
	}
	return 0, cpierrors.Cloud("no free VMID in range [%d, %d]: all %d IDs exhausted",
		start, end, end-start+1)
}

// NextVMID returns the lowest free VMID in the range specified by opts (default
// [VMIDRangeVMStart, VMIDRangeVMEnd]). The cluster VM list is fetched outside
// the process-level mutex so a slow PVE API call does not block other goroutines
// for its full round-trip duration. The mutex is held only around the in-memory
// scan+select, which is microsecond-range work. Cross-process races are handled
// at the caller layer via retry-on-conflict.
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

	// Fetch outside the mutex: the PVE API call can take seconds; holding the
	// lock here would serialize all goroutines behind a single 30-second timeout.
	used, err := listClusterVMIDs(ctx, c)
	if err != nil {
		return 0, err
	}

	// Lock only for the pure in-memory scan so two goroutines with identical
	// "used" snapshots cannot return the same VMID. The randomised start in
	// nextVMIDInRange further scatters concurrent allocations.
	globalVMIDMu.Lock()
	defer globalVMIDMu.Unlock()

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
//
// Both API calls (cluster list + storage content list) run outside the mutex
// for the same reason as NextVMID: they can block for seconds and must not
// head-of-line-block other goroutines.
func NextDiskVMID(ctx context.Context, c Client, node, storage string) (int, error) {
	if ctx == nil {
		return 0, cpierrors.Cloud("NextDiskVMID: ctx must not be nil")
	}
	if c == nil {
		return 0, cpierrors.Cloud("NextDiskVMID: client must not be nil")
	}

	// Fetch both data sources outside the mutex.
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

	globalVMIDMu.Lock()
	defer globalVMIDMu.Unlock()

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
	var resp *sdknodes.ListStorageContentResponse
	err := RetryOnTransient(ctx, nil, "vmid_list_storage", 0, func() error {
		var inner error
		resp, inner = nodesSvc.ListStorageContent(ctx, node, storage, nil)
		return inner
	})
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
					if sleepErr := retryBackoff(ctx, ao, createErr, attempt); sleepErr != nil {
						return 0, cpierrors.Wrap(sleepErr, "AllocateWithRetry: context cancelled during backoff")
					}
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
					if sleepErr := retryBackoff(ctx, ao, createErr, attempt); sleepErr != nil {
						return 0, cpierrors.Wrap(sleepErr, "AllocateDiskWithRetry: context cancelled during backoff")
					}
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
