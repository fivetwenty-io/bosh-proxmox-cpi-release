package pve

import (
	"context"
	"encoding/json"
	"fmt"
	mrand "math/rand/v2"
	"regexp"
	"sync"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// Default VMID ranges. The three classes occupy disjoint, contiguous bands:
//
//	VMs       [100, 8999]     general VM allocation (create_vm)
//	disks     [9000, 29999]   synthetic VMIDs for persistent-disk containers
//	templates [30000, 30999]  frozen stemcell template VMs (create_stemcell)
//
// Disks are sized at roughly 2x the VM ceiling so a foundation never exhausts
// persistent-disk identifiers before VMs. Templates need only a small band (the
// live count of stemcell name/version tuples is tens, not thousands). The VM
// range and template range are operator-overridable; the disk range is fixed in
// code today (see the configurability plan).
const (
	VMIDRangeVMStart       = 100
	VMIDRangeVMEnd         = 8999
	VMIDRangeDiskStart     = 9000
	VMIDRangeDiskEnd       = 29999
	VMIDRangeTemplateStart = 30000 // template VMs (create_stemcell); operator-overridable
	VMIDRangeTemplateEnd   = 30999
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
	// storageScanNode and storageScanStorage, when both non-empty, make
	// NextVMID union a storage-content scan (listStorageVMIDs) into its
	// used-set, exactly as NextDiskVMID already does for the disk range.
	// Set via WithStorageScan. Left empty ("", "") the option is a no-op.
	storageScanNode    string
	storageScanStorage string
	// extraStorageScans holds additional (node, storage) pairs unioned into
	// the same used-set as storageScanNode/storageScanStorage. Set via
	// WithExtraStorageScan, which may be supplied more than once to scan
	// several distinct pools in one allocation call (e.g. vm_storage AND a
	// distinct iso_storage). Empty by default — zero extra API calls and
	// byte-identical behavior for every caller that does not use it.
	extraStorageScans []storageScanTarget
}

// storageScanTarget is one (node, storage) pair queued by WithExtraStorageScan.
type storageScanTarget struct {
	node    string
	storage string
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

// WithStorageScan makes NextVMID also union VMIDs extracted from volume
// names ("vm-<vmid>-disk-N" and "base-<vmid>-disk-N", the latter covering
// frozen stemcell templates) found on node/storage into its used-set —
// the same storage-content scan NextDiskVMID already performs for the disk
// range (see listStorageVMIDs).
//
// This closes a gap that matters once two or more BOSH-Proxmox AZs
// (separate PVE clusters, each with its own cluster-resources view) share
// one storage backend (same storage ID, same NFS/dir export): the
// cluster-resources list NextVMID otherwise relies on only ever reflects
// VMs known to THIS cluster, so a VMID already holding files under
// images/<vmid>/ on the shared storage — because the OTHER cluster owns
// that VM or template — is invisible without this scan. Allocating it
// again would co-mingle the new VM's disk files with the existing owner's
// and risks cross-cluster disk deletion on cleanup.
//
// Both node and storage must be non-empty for the scan to activate. Either
// left empty (the zero value) makes this option a complete no-op — NextVMID
// behavior is then byte-identical to a call with no WithStorageScan at all,
// which keeps every existing caller (and every test that doesn't opt in)
// unaffected.
func WithStorageScan(node, storage string) AllocOption {
	return func(o *allocOpts) {
		o.storageScanNode = node
		o.storageScanStorage = storage
	}
}

// WithExtraStorageScan queues one additional (node, storage) pair to be
// unioned into NextVMID's used-set alongside the primary WithStorageScan
// pair (if any). Callers may supply it more than once to scan several
// distinct pools in a single allocation — e.g. vm_storage via WithStorageScan
// plus a distinct iso_storage via WithExtraStorageScan, closing the gap
// documented at WithStorageScan for pools WithStorageScan itself does not
// cover.
//
// Either argument empty is a no-op: nothing is queued and behavior is
// unaffected for that call. Duplicate (node, storage) pairs — including one
// identical to the primary WithStorageScan pair — are queued and scanned
// again; listStorageVMIDs is idempotent so this only costs a redundant API
// call, never a correctness issue.
func WithExtraStorageScan(node, storage string) AllocOption {
	return func(o *allocOpts) {
		if node == "" || storage == "" {
			return
		}
		o.extraStorageScans = append(o.extraStorageScans, storageScanTarget{node: node, storage: storage})
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
	switch {
	case ao != nil && ao.backoffFn != nil:
		d = ao.backoffFn(err, attempt)
	default:
		// Default: uniform 50–250 ms.
		d = 50*time.Millisecond + time.Duration(mrand.Int64N(int64(200*time.Millisecond))) // #nosec G404 -- VMID collision-avoidance jitter; non-cryptographic
	}
	// Test override via context wins over the default curve so handler tests
	// can keep VMID-conflict-retry suites deterministic without threading an
	// AllocOption all the way through the call stack. An explicit backoffFn
	// installed by production callers takes precedence over the override.
	if ao == nil || ao.backoffFn == nil {
		if override := backoffFromCtx(ctx); override != nil {
			d = override(attempt)
		}
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
	Vmid *sdkclient.PVEInt `json:"vmid"`
}

// listClusterVMIDs returns the set of all VMID integers currently registered
// in the cluster (across all nodes), as the UNION of two sources:
//
//   - GET /cluster/resources?type=vm: covers QEMU and LXC guests alike, but
//     the index trails node-local state by minutes, so a guest registered
//     seconds ago can be missing.
//   - ListGuestsAuthoritative: the per-node QEMU listings, which have no lag,
//     so a VMID a concurrent create just won is already in the used-set. The
//     CPI only ever creates QEMU guests, which means every peer-created VMID
//     the index can lag on is covered by this leg; LXC guests (always
//     operator-created, so never inside the lag window of a CPI peer) stay
//     covered by the index leg.
//
// Over-approximation is safe for allocation: the worst a stale index row
// costs is skipping a free ID. Both legs are required; either failing fails
// the allocation with a classified *cpierrors.Error.
//
// The cluster-resources call is wrapped in RetryOnTransient to absorb the
// pvedaemon-worker-recycle window: under burst load the worker holding our
// TCP connection may exit (request-quota or memory limit), surfacing as
// HTTP 596 or an auth-EOF on the next call. A fresh worker spawns within
// roughly a second, so a short retry usually wins. The authoritative leg
// carries its own per-node retries.
func listClusterVMIDs(ctx context.Context, c Client) (map[int]struct{}, error) {
	typeStr := "vm"
	var resp *sdkcluster.ListResourcesResponse
	err := RetryOnTransient(ctx, nil, "vmid_list_cluster", 0, func() error {
		var inner error
		resp, inner = c.Cluster().ListResources(ctx, &sdkcluster.ListResourcesParams{Type: &typeStr})
		return inner
	})
	if err != nil {
		return nil, cpierrors.Wrap(WrapError(err), "vmid: list cluster resources")
	}
	if resp == nil {
		// Retriable: a pvedaemon coming back up answers with an empty body, and
		// this listing gates every VMID allocation, parkers included.
		return nil, cpierrors.Retriable("vmid: nil response from cluster resources")
	}

	used := make(map[int]struct{}, len(*resp))
	for _, raw := range *resp {
		var entry vmidEntry
		if jsonErr := json.Unmarshal(raw, &entry); jsonErr != nil {
			// Skip malformed entries; a single bad item should not abort allocation.
			continue
		}
		if entry.Vmid != nil {
			used[int(entry.Vmid.Int())] = struct{}{}
		}
	}

	// Tolerant form: an offline member must not block every allocation
	// cluster-wide. Its guests stay covered by the index leg above (an
	// offline node creates no new guests, so the index has long since
	// caught up on them), keeping the used-set an over-approximation.
	guests, _, err := ListGuestsAuthoritativeTolerant(ctx, c, nil)
	if err != nil {
		return nil, cpierrors.Wrap(err, "vmid: authoritative guest enumeration")
	}
	for _, g := range guests {
		used[g.VMID] = struct{}{}
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
// When WithStorageScan(node, storage) is supplied (both non-empty), the
// storage's volume content is ALSO scanned and unioned into the used-set —
// see WithStorageScan for why this matters on storage shared across PVE
// clusters. Fetched outside the mutex for the same reason as the cluster
// list. Omitted (or either argument empty), behavior is unchanged from
// before this option existed. WithExtraStorageScan queues additional
// (node, storage) pairs scanned the same way, for callers that must cover
// more than one pool in a single allocation.
//
// Inputs and failure modes:
//   - ctx nil → returns *cpierrors.Error before any SDK call.
//   - c nil → returns *cpierrors.Error before any SDK call.
//   - SDK failure (cluster list) → returns *cpierrors.Error wrapping the SDK error.
//   - Storage-scan failure (when WithStorageScan is set) → returns
//     *cpierrors.Error wrapping the SDK error; the allocation fails rather
//     than proceeding blind to shared-storage content.
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

	if ao.storageScanNode != "" && ao.storageScanStorage != "" {
		storageUsed, sErr := listStorageVMIDs(ctx, c, ao.storageScanNode, ao.storageScanStorage)
		if sErr != nil {
			return 0, sErr
		}
		for id := range storageUsed {
			used[id] = struct{}{}
		}
	}
	for _, extra := range ao.extraStorageScans {
		storageUsed, sErr := listStorageVMIDs(ctx, c, extra.node, extra.storage)
		if sErr != nil {
			return 0, sErr
		}
		for id := range storageUsed {
			used[id] = struct{}{}
		}
	}

	// Lock only for the pure in-memory scan so two goroutines with identical
	// "used" snapshots cannot return the same VMID. The randomised start in
	// nextVMIDInRange further scatters concurrent allocations.
	globalVMIDMu.Lock()
	defer globalVMIDMu.Unlock()

	return nextVMIDInRange(used, ao.rangeStart, ao.rangeEnd)
}

// NextDiskVMID returns a free VMID in the disk range (default
// [VMIDRangeDiskStart, VMIDRangeDiskEnd]; override with WithRange). Disk VMIDs
// are synthetic identifiers used for unattached persistent volumes managed as
// QEMU VMs (for snapshotting support).
//
// node and storage may be empty: when set, the function ALSO unions VMIDs
// extracted from volume names ("vm-<vmid>-disk-N" or "base-<vmid>-disk-N",
// the latter matching frozen stemcell templates) on that storage into the
// "used" set. This is critical for synthetic VMIDs because the cluster VM
// list does NOT include orphaned persistent disks — without the storage
// scan, a stale "vm-9000-disk-0" left behind on storage would be invisible
// to NextDiskVMID and the next call could hand out the same VMID again,
// colliding on lvcreate.
//
// Both API calls (cluster list + storage content list) run outside the mutex
// for the same reason as NextVMID: they can block for seconds and must not
// head-of-line-block other goroutines.
func NextDiskVMID(ctx context.Context, c Client, node, storage string, opts ...AllocOption) (int, error) {
	if ctx == nil {
		return 0, cpierrors.Cloud("NextDiskVMID: ctx must not be nil")
	}
	if c == nil {
		return 0, cpierrors.Cloud("NextDiskVMID: client must not be nil")
	}

	ao := &allocOpts{
		rangeStart: VMIDRangeDiskStart,
		rangeEnd:   VMIDRangeDiskEnd,
	}
	for _, opt := range opts {
		opt(ao)
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

	return nextVMIDInRange(used, ao.rangeStart, ao.rangeEnd)
}

// volumeVMIDRegexp matches PVE volume names of the form "vm-NNN-disk-N"
// (a regular VM/disk volume) or "base-NNN-disk-N" (the frozen-template
// naming PVE gives a VM's disks once it becomes a template — see
// create_stemcell's MakeTemplate), extracting the VMID from either form.
// Anchored on each side so we don't accidentally match substrings.
var volumeVMIDRegexp = regexp.MustCompile(`(?:^|[/:])(?:vm|base)-(\d+)-disk-\d+(?:\.\w+)?$`)

// listStorageVMIDs returns the set of VMIDs that have at least one volume
// named "vm-{vmid}-disk-{N}" or "base-{vmid}-disk-{N}" on the given storage.
// The base- form is how PVE renames a VM's disk files once MakeTemplate
// freezes it, so this scan sees frozen stemcell templates as well as
// ordinary VM/disk volumes. Non-VM-disk volumes (ISOs, backups, snippets)
// are skipped because they don't follow either naming convention.
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
		// WrapError first, as the cluster listing above and the create below both
		// do: RetryOnTransient hands back the raw SDK error, and cpierrors.Wrap
		// of an untyped error is permanent. A storage plugin that times out past
		// the retry budget would then fail a park -- and with it a detach_disk --
		// for good, rather than being re-driven.
		return nil, cpierrors.Wrap(WrapError(err),
			fmt.Sprintf("vmid: list storage %q content on node %q", storage, node))
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
// Idempotency-collision model — regenerate-identity: a VMID conflict means the
// numeric identity is already taken by another VM. The correct response is to
// allocate a brand-new VMID, never to retry the same one. This is the
// "regenerate-identity" model. It contrasts with clouds whose idempotency token
// (AWS ClientToken, Azure x-ms-client-request-id) means "this request is in
// flight" — those clouds must retry the same token; regenerating a fresh token
// would create a duplicate resource. PVE has no in-flight reservation: a VMID
// that conflicts is simply occupied, so regeneration is the only safe path.
// TestAllocateWithRetry_RegeneratesDistinctVMID asserts this invariant.
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
	var lastConflictErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		vmid, err := NextVMID(ctx, c, opts...)
		if err != nil {
			return 0, err
		}
		lastVMID = vmid
		if createErr := create(vmid); createErr != nil {
			if isConflict != nil && isConflict(createErr) {
				lastConflictErr = createErr
				if attempt < maxAttempts-1 {
					if sleepErr := retryBackoff(ctx, ao, createErr, attempt); sleepErr != nil {
						return 0, cpierrors.Wrap(sleepErr, "AllocateWithRetry: context cancelled during backoff")
					}
				}
				continue
			}
			// WrapError first: a raw SDK 5xx through cpierrors.Wrap becomes a
			// non-retriable CloudError, turning a transient pvedaemon recycle
			// after the retry budget into a permanent create_vm/create_disk
			// failure the Director will not re-drive.
			return 0, cpierrors.Wrap(WrapErrorKeepingClass(createErr), fmt.Sprintf("create VMID %d", vmid))
		}
		return vmid, nil
	}

	// Exhaustion keeps the last conflict as the cause so both the reason and
	// its retriability class survive: an every-attempt conflict is contention
	// (another allocator racing this band), which a Director re-drive with
	// fresh VMIDs resolves, so discarding it here minted a permanent failure
	// out of a transient condition.
	if lastConflictErr != nil {
		return 0, cpierrors.Wrap(WrapErrorKeepingClass(lastConflictErr), fmt.Sprintf(
			"AllocateWithRetry: exhausted VMID allocation after %d attempts (last attempted VMID %d)",
			maxAttempts, lastVMID))
	}
	return 0, cpierrors.Cloud(
		"AllocateWithRetry: exhausted VMID allocation after %d attempts (last attempted VMID %d)",
		maxAttempts, lastVMID,
	)
}

// AllocateDiskWithRetry mirrors AllocateWithRetry for disk-range VMIDs. On
// each attempt it calls NextDiskVMID(ctx, c, node, storage, opts...) so the
// storage-volume scan re-runs every iteration and orphan volumes from prior
// failed attempts remain visible. opts are forwarded to NextDiskVMID, so
// WithRange overrides the default disk range. Same backoff/jitter treatment as
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
	var lastConflictErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		vmid, err := NextDiskVMID(ctx, c, node, storage, opts...)
		if err != nil {
			return 0, err
		}
		lastVMID = vmid
		if createErr := create(vmid); createErr != nil {
			if isConflict != nil && isConflict(createErr) {
				lastConflictErr = createErr
				if attempt < maxAttempts-1 {
					if sleepErr := retryBackoff(ctx, ao, createErr, attempt); sleepErr != nil {
						return 0, cpierrors.Wrap(sleepErr, "AllocateDiskWithRetry: context cancelled during backoff")
					}
				}
				continue
			}
			// WrapErrorKeepingClass first, exactly like the VM twin above: a
			// raw SDK 5xx through a bare cpierrors.Wrap became a non-retriable
			// CloudError, turning a transient pvedaemon recycle into a
			// permanent create_disk failure the Director will not re-drive.
			return 0, cpierrors.Wrap(WrapErrorKeepingClass(createErr), fmt.Sprintf("create disk VMID %d", vmid))
		}
		return vmid, nil
	}

	// Keep the last conflict as the cause on exhaustion (see AllocateWithRetry).
	if lastConflictErr != nil {
		return 0, cpierrors.Wrap(WrapErrorKeepingClass(lastConflictErr), fmt.Sprintf(
			"AllocateDiskWithRetry: exhausted disk VMID allocation after %d attempts (last attempted VMID %d)",
			maxAttempts, lastVMID))
	}
	return 0, cpierrors.Cloud(
		"AllocateDiskWithRetry: exhausted disk VMID allocation after %d attempts (last attempted VMID %d)",
		maxAttempts, lastVMID,
	)
}
