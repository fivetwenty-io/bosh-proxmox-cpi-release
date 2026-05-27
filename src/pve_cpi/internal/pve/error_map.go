// SDK-to-BOSH error mapping used across all CPI actions.
package pve

import (
	"context"
	"errors"
	"net"
	"strings"

	sdkerrors "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
)

// WrapError maps an SDK or network error to the appropriate BOSH CPI error type:
//   - nil → nil
//   - SDK 404 (APIError.IsNotFound) → non-retriable CloudError
//   - SDK 5xx (errors.Is ErrServer) → retriable RetriableCloudError
//   - SDK 4xx non-404 → non-retriable CloudError
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

	// SDK APIError: check HTTP code for 404 vs 5xx vs other 4xx.
	var apiErr *sdkerrors.APIError
	if errors.As(err, &apiErr) {
		if apiErr.IsNotFound() {
			return cpierrors.Cloud("resource not found: %s", apiErr.Error())
		}
		// 5xx server error → retriable.
		if errors.Is(err, sdkerrors.ErrServer) {
			return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, "PVE server error: "+apiErr.Error())
		}
		// 4xx non-404 → non-retriable.
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

// IsVMIDConflict reports whether err signals that a VMID (or block-storage
// volume name) was already taken when the create request reached PVE.
//
// PVE returns this condition in two shapes depending on the endpoint:
//   - REST API spec: HTTP 409 from /cluster/nextid validators.
//   - Observed in the wild for /nodes/{node}/qemu and storage allocators:
//     HTTP 500 wrapping the perl die() text "VM N already exists on node …"
//     or "unable to create VM N - VM N already exists".
//
// Substring matching on "already exists" (case-insensitive) is the canonical
// detector; the 409 check is kept as a forward-compatible hint.
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
	return strings.Contains(strings.ToLower(err.Error()), "already exists")
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
