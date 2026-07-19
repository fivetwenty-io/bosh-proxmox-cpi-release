// SDK-to-BOSH error mapping used across all CPI actions.
package pve

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"

	sdkerrors "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

// pvePushbackPhrases is the conservative set of lower-cased substrings that
// identify PVE server-side rate-limiting or worker-pool exhaustion. Each phrase
// is matched case-insensitively against the full error string. The set is kept
// intentionally narrow — false positives (non-retriable errors misclassified as
// pushback) are worse than false negatives (missing a retriable case).
var pvePushbackPhrases = []string{
	"too many requests",
	"worker busy",
	"worker pool",
	"unable to acquire lock",
	"lock-acquire timeout",
	"got timeout",
}

// WrapError maps an SDK or network error to the appropriate BOSH CPI error type:
//   - nil → nil
//   - SDK 404 (APIError.IsNotFound) → non-retriable CloudError
//   - SDK 5xx (errors.Is ErrServer) → retriable RetriableCloudError
//   - SDK 429 / pushback phrase → retriable RetriableCloudError (pushback subtype)
//   - SDK 4xx non-404 non-429 → non-retriable CloudError
//   - net.Error Timeout() → retriable RetriableCloudError
//   - SDK ConnectionError or TimeoutError → retriable RetriableCloudError
//   - everything else → non-retriable CloudError wrapping original message
func WrapError(err error) error {
	if err == nil {
		return nil
	}

	// SDK ConnectionError → retriable (transient network fault).
	var connErr *sdkerrors.ConnectionError
	if errors.As(err, &connErr) {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, "PVE connection error: "+connErr.Error())
	}

	// SDK TimeoutError → retriable.
	var timeoutErr *sdkerrors.TimeoutError
	if errors.As(err, &timeoutErr) {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, "PVE timeout: "+timeoutErr.Error())
	}

	// net.Error with Timeout() → retriable.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, "network timeout: "+err.Error())
	}

	// Cluster quorum loss: /etc/pve becomes read-only cluster-wide and every
	// mutating call fails, while read-only GETs keep succeeding. PVE surfaces
	// this as either a 5xx APIError or plain task-body text, so this check
	// runs before the generic APIError branch below — otherwise a quorum 5xx
	// would fall into the generic "PVE server error" message and lose the
	// operator-actionable hint. Retriable: quorum loss is a minutes-scale
	// condition (node loss below majority, corosync partition) that clears
	// once the cluster reforms a majority, so callers should route this onto
	// the storage-lock retry curve (2s→30s, 10 attempts) rather than the
	// shorter transport curve (1s→15s, 8 attempts) — see IsClusterNotQuorate's
	// use in RetryOnTransientOrLock and RetryOnStorageLock (retry.go).
	if IsClusterNotQuorate(err) {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			"cluster has lost quorum; mutations are blocked until quorum returns — check `pvecm status`: "+err.Error())
	}

	// SDK APIError: check HTTP code for 404 vs 5xx vs 429 vs other 4xx.
	var apiErr *sdkerrors.APIError
	if errors.As(err, &apiErr) {
		if apiErr.IsNotFound() {
			return cpierrors.Cloud("resource not found: %s", apiErr.Error())
		}
		// 5xx server error → retriable.
		if errors.Is(err, sdkerrors.ErrServer) {
			return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, "PVE server error: "+apiErr.Error())
		}
		// 429 Too Many Requests → retriable pushback. Checked inside the APIError
		// branch so the HTTPCode field is available without a second errors.As.
		if apiErr.HTTPCode == 429 {
			return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, "PVE pushback (429): "+apiErr.Error())
		}
		// 4xx non-404 non-429 → non-retriable.
		return cpierrors.Cloud("PVE API error: %s", apiErr.Error())
	}

	// Task-level transient signals carried as plain errors (no APIError /
	// ConnectionError shape because they originate inside a UPID task body
	// surfaced by AwaitTask). Mark these retriable so the director re-runs
	// the action with a fresh VMID / on a recovered storage layer.
	if IsStorageLockTimeout(err) {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, "PVE storage backend transient: "+err.Error())
	}
	if IsPmxcfsConfigMissing(err) {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, "PVE pmxcfs race (config gone mid-flight): "+err.Error())
	}
	// Plain-text pushback phrases (task-body or non-APIError surfaces).
	if IsPVEPushback(err) {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, "PVE pushback: "+err.Error())
	}

	// Fallback: generic wrap as non-retriable CloudError.
	return cpierrors.Wrap(err, "PVE error: "+err.Error())
}

// WrapNotFoundVM upgrades a 404-class error to VMNotFound for the given VM CID.
// Non-404 errors pass through WrapError unchanged.
func WrapNotFoundVM(err error, vmCID string) error {
	if err == nil {
		return nil
	}
	if IsNotFound(err) {
		return cpierrors.VMNotFound(vmCID)
	}
	return WrapError(err)
}

// WrapNotFoundDisk upgrades a 404-class error to DiskNotFound for the given disk CID.
// Non-404 errors pass through WrapError unchanged.
func WrapNotFoundDisk(err error, diskCID string) error {
	if err == nil {
		return nil
	}
	if IsNotFound(err) {
		return cpierrors.DiskNotFound(diskCID)
	}
	return WrapError(err)
}

// IsNotFound returns true when err (or any error in its chain) signals HTTP 404
// from the PVE SDK — i.e., errors.Is(err, sdkerrors.ErrNotFound) — or is already
// a BOSH VMNotFound / DiskNotFound error.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	// Check SDK sentinel.
	if errors.Is(err, sdkerrors.ErrNotFound) {
		return true
	}
	// Check SDK APIError.IsNotFound() directly (handles Code vs HTTPCode).
	var apiErr *sdkerrors.APIError
	if errors.As(err, &apiErr) && apiErr.IsNotFound() {
		return true
	}
	// Check BOSH not-found types.
	return cpierrors.IsNotFound(err)
}

// IsVolumeMissing reports whether err signals that a storage volume is absent.
// PVE's HTTP layer returns 404 for dir-type storages (covered by IsNotFound),
// but block-backed storages return 500 with a CLI-derived error string when
// the underlying object no longer exists. Detected shapes:
//
//   - lvmthin: GET /nodes/<n>/storage/<s>/content/<volid> on a deleted LV
//     returns 500 with "can't get size of '/dev/<vg>/<lv>': Failed to find
//     logical volume \"<vg>/<lv>\"". Either substring is sufficient.
//   - zfspool: similar pattern with "dataset does not exist" or
//     "no such pool or dataset".
//
// Callers that want existence semantics — has_disk, delete_disk idempotency,
// the NodeForExisting cluster scan — should fold this into a clean "not
// present" outcome rather than letting it surface as a retriable cloud error
// (which makes the operation appear to fail forever even though the volume
// is genuinely gone).
//
// nil → false.
func IsVolumeMissing(err error) bool {
	if err == nil {
		return false
	}
	if IsNotFound(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "failed to find logical volume"):
		return true
	case strings.Contains(msg, "can't get size of"):
		return true
	case strings.Contains(msg, "dataset does not exist"):
		return true
	case strings.Contains(msg, "no such pool or dataset"):
		return true
	}
	return false
}

// ExistsTolerant wraps client.Storage().Exists with extended not-found
// detection. The SDK only folds HTTP 404 into (false, nil); block-backed
// storages (lvmthin, zfspool) instead return 500 wrapping the perl CLI's
// "Failed to find logical volume" / "dataset does not exist" text. This
// helper recognizes both and reports (false, nil) so the cluster-scan and
// idempotent delete paths see uniform existence semantics regardless of
// storage backend.
func ExistsTolerant(ctx context.Context, client Client, node, storage, volume string) (bool, error) {
	exists, err := client.Storage().Exists(ctx, node, storage, volume)
	if err == nil {
		return exists, nil
	}
	if IsVolumeMissing(err) {
		return false, nil
	}
	return false, err
}

// IsVMIDConflict reports whether err signals that a VMID was already taken
// when the create request reached PVE.
//
// PVE returns this condition in two shapes depending on the endpoint:
//   - REST API spec: HTTP 409 from /cluster/nextid validators.
//   - Observed in the wild for /nodes/{node}/qemu:
//     HTTP 500 wrapping the perl die() text "VM N already exists on node …"
//     or "unable to create VM N - VM N already exists".
//
// The substring check is anchored to VMID-specific wording ("vm" adjacent
// to "already exists") to avoid false positives from block-storage messages
// such as Ceph "image already exists" or LVM "volume group already exists".
//
// nil → false.
func IsVMIDConflict(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *sdkerrors.APIError
	if errors.As(err, &apiErr) && apiErr.HTTPCode == 409 {
		return true
	}
	msg := strings.ToLower(err.Error())
	// Match PVE VMID-specific patterns: "vm N already exists" or
	// "vmid N already exists". A bare "already exists" without a vm/vmid
	// prefix is NOT matched to prevent false positives from Ceph/LVM messages.
	return strings.Contains(msg, "vm ") && strings.Contains(msg, " already exists") ||
		strings.Contains(msg, "vmid ") && strings.Contains(msg, " already exists")
}

// IsCloneSourceMissing reports whether err signals that the clone SOURCE
// template VM does not exist on the target node — e.g. a stemcell template was
// removed out-of-band on a shared cluster. PVE surfaces this during a clone
// POST as a 500-class "unable to find configuration file for VM <id>", which
// errors.Is(…, ErrServer) would otherwise classify as a transient transport
// fault. It is a PERMANENT condition for that template: retrying with a fresh
// VMID cannot help, so callers must treat it as non-retryable and surface the
// real cause instead of an "exhausted VMID allocation" message.
func IsCloneSourceMissing(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unable to find configuration file for vm")
}

// IsTransientTransport reports whether err signals a transient transport-layer
// fault between the CPI and the PVE API surface, distinct from a deliberate
// HTTP 4xx response. The canonical trigger is a pvedaemon worker cycling
// mid-request — under burst load each worker hits its built-in request quota
// and exits, dropping every in-flight TCP connection.
//
// Surfaces detected:
//   - Any 5xx APIError (errors.Is sdkerrors.ErrServer). HTTP 596 is the
//     specific shape pveproxy emits when it cannot reach pvedaemon on
//     localhost ("backend gone"); it is included in the 5xx class.
//   - SDK *ConnectionError — bare TCP refusal, RST, or DNS failure.
//   - SDK *TimeoutError — request deadline exceeded.
//   - "failed to parse login response" — POST /access/ticket returned an
//     empty body because the worker exited mid-response.
//   - "auto-login failed" — generic SDK wrapper covering ticket-fetch
//     failures, including the EOF case above.
//
// The condition is transient: a fresh pvedaemon worker replaces the dying
// one within a second, so a short backoff and retry usually wins.
//
// nil → false.
func IsTransientTransport(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, sdkerrors.ErrServer) {
		return true
	}
	var connErr *sdkerrors.ConnectionError
	if errors.As(err, &connErr) {
		return true
	}
	var timeoutErr *sdkerrors.TimeoutError
	if errors.As(err, &timeoutErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "failed to parse login response") {
		return true
	}
	if strings.Contains(msg, "auto-login failed") {
		return true
	}
	if strings.Contains(msg, "(code: 596)") || strings.Contains(msg, "http 596") {
		return true
	}
	return false
}

// IsHotUnplugBusy reports whether err is PVE's hot-unplug rejection for a
// disk the guest has not yet released:
//
//	scsi1: hotplug problem - error on hot-unplugging device 'virtioscsi1' - still busy in guest?
//
// PVE surfaces it inside a parameter-verification failure (HTTP 400), which
// the transport classifiers treat as permanent — but the condition is a
// settling window, not a verdict: QEMU can keep the drive busy for a few
// seconds after a snapshot or an I/O burst, and the same unplug succeeds on
// retry. A disk the guest genuinely holds (still mounted) keeps failing and
// surfaces after the bounded retry budget.
//
// nil → false.
func IsHotUnplugBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "hotplug problem") && strings.Contains(msg, "busy in guest")
}

// IsStorageLockTimeout reports whether err signals a transient PVE storage-
// backend stall. Two shapes are covered, both surfaced inside qmtask output:
//
//  1. per-storage lockfile timeout — many concurrent qmcreate / qm resize
//     tasks serialise behind /var/lock/pve-manager/pve-storage-<name>; the
//     loser dies with
//     "can't lock file '/var/lock/pve-manager/pve-storage-data' - got timeout".
//
//  2. LVM tooling command timeout — `qm resize` shells out to /sbin/lvs to
//     read the LV's current size before extending. Under heavy concurrent
//     activity on the same VG, lvs blocks on the LVM metadata daemon and
//     the wrapper kills it after its internal deadline, surfacing as
//     "command '/sbin/lvs --separator ... /dev/data/vm-N-disk-0' failed:
//     got timeout".
//
// Both are transient storage-backend contention and clear once the in-flight
// holder releases — the same seconds-scale backoff is appropriate for both,
// so they share the predicate and the retry curve.
//
// nil → false.
func IsStorageLockTimeout(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "can't lock file") && strings.Contains(msg, "got timeout") {
		return true
	}
	return IsLVMCommandTimeout(err)
}

// IsLVMCommandTimeout reports whether err is a task-level failure caused by
// an LVM userspace tool (lvs, lvcreate, lvremove, lvextend, vgs) timing out
// against the LVM metadata daemon. PVE invokes these directly during qm
// resize / qmcreate / qmdestroy on LVM-thin storage; under concurrent VG
// activity any one of them can stall and PVE's command wrapper kills it.
//
// The canonical surface is
//
//	task failed: command '/sbin/lvs ...' failed: got timeout
//
// nil → false.
func IsLVMCommandTimeout(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "failed: got timeout") {
		return false
	}
	// Anchor on the LVM tool path to avoid colliding with unrelated
	// "command X failed: got timeout" surfaces. /sbin/lv* and /sbin/vg*
	// cover the LVM2 user-space binaries PVE shells out to.
	return strings.Contains(msg, "/sbin/lv") || strings.Contains(msg, "/sbin/vg")
}

// IsClusterNotQuorate reports whether err signals that the PVE cluster has
// lost quorum. Below-majority node loss (or a corosync network partition)
// makes /etc/pve (the pmxcfs cluster filesystem) read-only cluster-wide:
// every mutating call fails while read-only GETs continue to succeed. PVE
// surfaces this as a 5xx APIError or as plain task-body text, matched
// case-insensitively for either phrase:
//
//   - "not quorate" — e.g. "error writing config, cfs-lock failed - not quorate"
//   - "no quorum"   — corosync/pmxcfs's own wording on some surfaces
//
// This is a MINUTES-scale condition, not a seconds-scale worker hiccup —
// unlike a plain 5xx, retries should use the storage-lock curve (2s→30s, 10
// attempts) rather than the shorter transport curve (1s→15s, 8 attempts).
// See WrapError (which injects an operator-actionable hint into the wrapped
// message) and RetryOnTransientOrLock / RetryOnStorageLock (which route
// retries onto the storage-lock curve) in this package.
//
// nil → false.
func IsClusterNotQuorate(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not quorate") || strings.Contains(msg, "no quorum")
}

// IsSnapshotBlocked reports whether err signals that a PVE disk operation was
// rejected because a VM snapshot holds a reference to the affected disk. PVE
// refuses detach and resize on LVM-thin/ZFS storage when a snapshot captures
// the disk state; the error surfaces as a task-body failure (for resize, via
// AwaitTask) or as a synchronous HTTP error (for detach via PUT /config).
//
// Two PVE message shapes are detected (case-insensitive):
//
//   - "is used in snapshot" — detach path:
//     "cannot delete disk 'scsiN', disk is used in snapshot '<name>'"
//   - "referenced in snapshot" — resize path (LVM-thin/ZFS):
//     "can't resize volume, volume is referenced in snapshot '<name>'"
//
// This is a defense-in-depth classifier. The primary guard is the pre-flight
// HasSnapshots check in each handler. IsSnapshotBlocked lets callers add
// remediation context when a guard is bypassed (race, config override, or
// storage backends that allow the op but later fail at the task level).
//
// nil → false.
func IsSnapshotBlocked(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "is used in snapshot") ||
		strings.Contains(msg, "referenced in snapshot")
}

// IsBaseVolumeInUse reports whether err signals that PVE refused to delete a
// template or VM because a linked clone still holds a reference to its base
// volume. PVE surfaces this condition in several shapes:
//
//   - "volume '...' is still in use by '...'" — DELETE /storage/content/<volid>
//     on an lvmthin/ZFS base that one or more linked clones depend on.
//   - "base volume ... in use" — zfspool variant.
//   - "cannot remove ... used by" — directory-backed variant.
//   - "still in use by" — generic task-body wrapper.
//
// All shapes share "in use" together with at least one of "volume", "base", or
// "clone". The predicate is best-effort: callers use it only to downgrade an
// orphan-prune failure to a skip/log rather than to drive hard business logic.
//
// nil → false.
func IsBaseVolumeInUse(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if !strings.Contains(s, "in use") {
		return false
	}
	return strings.Contains(s, "volume") ||
		strings.Contains(s, "base") ||
		strings.Contains(s, "clone")
}

// IsPmxcfsConfigMissing reports whether err is a task-level failure caused by
// PVE failing to read a VM's config file from /etc/pve (the pmxcfs cluster
// filesystem). Surface text:
//
//	task failed: Configuration file 'nodes/<node>/qemu-server/<vmid>.conf' does not exist
//
// This is observed mid-deploy when a concurrent operation (rollback of a
// sibling create, an orphan sweep, or a pmxcfs sync delay across nodes)
// removes the conf between an earlier successful qmcreate and the very next
// qm config / qm resize call against the same VMID. The VM may genuinely be
// gone — retrying the same call in-process is fruitless — but classifying
// this as retriable lets the BOSH director re-issue create_vm with a fresh
// VMID rather than failing the entire deploy.
//
// nil → false.
func IsPmxcfsConfigMissing(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// Two surfaces for the same condition:
	//   qm config / qm resize: "Configuration file '...' does not exist"
	//   qm stop / qm destroy:  "unable to find configuration file for VM <id> on node '<node>'"
	return (strings.Contains(msg, "Configuration file ") &&
		strings.Contains(msg, "does not exist")) ||
		strings.Contains(msg, "unable to find configuration file")
}

// vmConfigLockPattern matches PVE's guest-config lock rejection. PVE's
// PVE::AbstractConfig::check_lock dies with "VM is locked ($lock)\n" whenever
// a mutating call (stop, destroy, migrate, resize, ...) reaches a guest whose
// config carries an in-flight lock — clone, create, backup, migrate,
// snapshot, rollback, or suspended. API wrappers commonly prepend context
// ("unable to destroy VM 106: VM is locked (clone)") and some call sites
// repeat the vmid immediately before "is locked" ("VM 106 is locked
// (clone)"); the pattern is unanchored (so it matches regardless of prefix
// text) and tolerates an optional numeric vmid in that position.
//
// Distinct from the storage-lockfile phrase "can't lock file ... got timeout"
// (see IsStorageLockTimeout): that names a filesystem lockfile PATH, this
// names a guest-config lock TYPE in parentheses with no lockfile path present
// — the two patterns never match the same string.
var vmConfigLockPattern = regexp.MustCompile(`(?i)vm\s*(?:\d+\s*)?is locked\s*\(([^)]*)\)`)

// IsVMConfigLocked reports whether err signals that PVE rejected an operation
// because the target guest's config carries an in-flight lock (see
// vmConfigLockPattern). This is a guest-CONFIG lock (the "lock" attribute
// PVE writes into <vmid>.conf during clone/create/backup/migrate/snapshot/
// rollback), not the storage-backend lockfile contention IsStorageLockTimeout
// detects — the two conditions require different recovery (`qm unlock
// <vmid>` for this one; nothing operator-actionable for the storage case,
// which simply clears once the contending task finishes).
//
// nil → false.
func IsVMConfigLocked(err error) bool {
	if err == nil {
		return false
	}
	return vmConfigLockPattern.MatchString(err.Error())
}

// vmRunningDestroyPattern matches PVE's refusal to destroy a running guest.
// PVE::API2::Qemu::destroy_vm dies with "VM $vmid is running - destroy failed"
// (qemu-server) when the guest process is still alive; API wrappers prepend
// request context, so the pattern is unanchored. The vmid is optional for the
// same reason as vmConfigLockPattern.
var vmRunningDestroyPattern = regexp.MustCompile(`(?i)vm\s*(?:\d+\s*)?is running\s*-\s*destroy failed`)

// IsVMRunningDestroyFailure reports whether err signals that PVE rejected a
// destroy because the guest is still running. Seen when a stop was accepted
// while the VM was HA-managed: the stop task completes when the CRM files the
// request, not when the LRM halts the guest, so a destroy issued right after
// the task races the actual shutdown. Recovery is to wait for the guest to
// report "stopped" and reissue the destroy (skiplock does NOT bypass this —
// it only skips config-lock checks).
//
// nil → false.
func IsVMRunningDestroyFailure(err error) bool {
	if err == nil {
		return false
	}
	return vmRunningDestroyPattern.MatchString(err.Error())
}

// VMConfigLockType extracts the lock type named in a "VM is locked (<type>)"
// error — e.g. "clone", "create", "backup", "migrate" — for use in operator-
// facing diagnostics. Returns "" when err does not match IsVMConfigLocked or
// the parenthetical is empty.
//
// nil → "".
func VMConfigLockType(err error) string {
	if err == nil {
		return ""
	}
	m := vmConfigLockPattern.FindStringSubmatch(err.Error())
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// WrapVMConfigLocked upgrades a PVE guest-config-lock rejection (see
// IsVMConfigLocked) into a retriable error whose message names the lock type
// and the operator recovery command. PVE's HTTP API exposes no unlock
// endpoint — the only ways to clear a stuck guest-config lock are `qm unlock
// <vmid>` run on the node hosting the guest, or a skiplock=true retry, which
// PVE honors only for the root@pam superuser (see IsRootPamIdentity). This
// wrap is deliberately identity-agnostic: callers that CAN retry with
// skiplock should attempt that BEFORE calling this, reserving
// WrapVMConfigLocked for the final, unresolved failure surfaced to the BOSH
// Director — so the Director's error output actionably names the fix instead
// of repeating a generic 5xx message forever.
//
// vmid and node are caller-supplied context (not parsed from err, which may
// or may not embed them) so the recovery command is always concrete. Non-lock
// errors pass through WrapError unchanged. nil → nil.
func WrapVMConfigLocked(err error, vmid int, node string) error {
	if err == nil {
		return nil
	}
	if !IsVMConfigLocked(err) {
		return WrapError(err)
	}
	lockType := VMConfigLockType(err)
	if lockType == "" {
		lockType = "unknown"
	}
	return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
		fmt.Sprintf(
			"PVE VM %d on node %q is locked (%s); recover with `qm unlock %d` on node %q, then retry: %s",
			vmid, node, lockType, vmid, node, err.Error()))
}

// IsPVEPushback reports whether err signals PVE server-side rate-limiting or
// worker-pool exhaustion. Two signal surfaces are covered:
//
//  1. HTTP 429 Too Many Requests — pveproxy enforces a per-source request cap;
//     the SDK surfaces this as an *sdkerrors.APIError with HTTPCode 429.
//
//  2. Conservative phrase set matched case-insensitively against the full error
//     string. The phrases are kept intentionally narrow — the goal is to catch
//     well-known PVE pushback messages without false-positive-classifying
//     unrelated permanent errors as retriable.
//
// Callers that need the longer PushbackBackoff curve (5s/60s) rather than the
// StorageLockBackoff (2s/30s) should check IsPVEPushback first. Note that
// storage-lock timeout strings ("can't lock file … got timeout") overlap with
// the "got timeout" phrase so IsPVEPushback is a superset of IsStorageLockTimeout
// for plain-text errors — both return true for the same lock-timeout string.
//
// nil → false.
func IsPVEPushback(err error) bool {
	if err == nil {
		return false
	}
	// HTTP 429 via SDK APIError (HTTPCode field is set by ParseAPIError).
	var apiErr *sdkerrors.APIError
	if errors.As(err, &apiErr) && apiErr.HTTPCode == 429 {
		return true
	}
	// Plain-text phrase matching (task-body errors, non-APIError surfaces).
	msg := strings.ToLower(err.Error())
	for _, phrase := range pvePushbackPhrases {
		if strings.Contains(msg, phrase) {
			return true
		}
	}
	return false
}
