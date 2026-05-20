// Package pve provides SDK-to-BOSH error mapping used across all CPI actions.
package pve

import (
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

// IsStorageLockTimeout reports whether err signals that a PVE per-storage
// lockfile (e.g. /var/lock/pve-manager/pve-storage-<name>) could not be
// acquired before its timeout. This happens when many concurrent qmcreate
// tasks import from the same storage and the import operation serialises
// behind the storage lock; the loser surfaces a task error like:
//
//	cannot import from 'local:import/...' - can't lock file
//	'/var/lock/pve-manager/pve-storage-data' - got timeout
//
// The condition is transient: retrying with a fresh VMID after a longer
// backoff succeeds once the lock holder finishes its import.
//
// nil → false.
func IsStorageLockTimeout(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "can't lock file") && strings.Contains(msg, "got timeout")
}
