// SDK-to-BOSH error mapping used across all CPI actions.
package pve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"syscall"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

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

// WrapConfigReadError classifies a VM-config read failure for the paths that
// scan configs to find who holds a volume: the cluster-wide holder scan, the
// parker listing, and the holder resolve.
//
// It exists because those paths used to force every non-404 into a retriable
// error, which turns a missing VM.Audit grant into a Director that re-drives a
// detach forever against a permission only a human can add. WrapError makes the
// real split -- except that its fallback is permanent, and these scans see
// transient transport shapes (a cycling pveproxy, a 5xx with no APIError
// wrapper) that must stay retriable. IsTransientTransport is the wider of the
// two tests, so it goes first.
func WrapConfigReadError(err error) error {
	if err == nil {
		return nil
	}
	if IsTransientTransport(err) {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, "PVE transient transport fault: "+err.Error())
	}
	// Any 5xx on a config read is the server failing, not a verdict about the
	// request: a cycling pvedaemon worker answers this way and comes back within
	// seconds. IsTransientTransport catches the wrapped shapes; this catches a
	// bare APIError carrying only the code. The permanent 500-with-text shapes
	// are excluded for the same reason IsTransientTransport excludes them.
	if code, ok := apiHTTPCode(err); ok && code >= 500 && !IsVolumeFormatUnknown(err) {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, "PVE server error: "+err.Error())
	}
	return WrapError(err)
}

// WrapMutationError classifies a failure from a mutating parker call: an
// attach, a detach, a protection write, a task await.
//
// It differs from WrapError in its default. WrapError ends with a permanent
// wrap, which is right for a caller that can enumerate what it might see; these
// calls cannot. PVE reports plenty of transient conditions as prose the
// classifiers do not recognize, and a park that fails permanently on one of them
// fails the whole detach_disk with no retry, leaving the disk free-floating --
// the state parking exists to avoid. So the default here is retriable, and only
// the shapes that are provably a verdict about the request stay permanent: a
// 4xx that is not 429 (a denied grant, a malformed request), and a not-found.
func WrapMutationError(err error) error {
	if err == nil {
		return nil
	}
	if IsNotFound(err) {
		return WrapError(err)
	}
	// pmxcfs "Configuration file ... does not exist" arrives as a 500 with prose,
	// so neither test above catches it -- and it is a verdict, not a wobble: the
	// guest this call names is not there. Without this, every write against a
	// parker somebody deleted mid-window is retried until the Director gives up.
	if IsPmxcfsConfigMissing(err) {
		return cpierrors.Cloud("PVE reports the guest config is gone: %s", err.Error())
	}
	if code, ok := apiHTTPCode(err); ok && code >= 400 && code < 500 && code != 429 {
		return WrapError(err)
	}
	if IsVolumeFormatUnknown(err) {
		return WrapError(err)
	}
	return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, "PVE call failed: "+err.Error())
}

// apiHTTPCode extracts the HTTP status code from an SDK error, whichever
// concrete type carries it. The SDK's ParseAPIError does not always return
// *APIError: 403 dispatches to *PermissionError, 400 to *ParameterError, and
// 401 to *AuthenticationError, each embedding APIError by VALUE -- so an
// errors.As against *APIError misses exactly the codes that are a verdict
// about the request. Every classifier that branches on a 4xx must go through
// this, not a bare errors.As.
func apiHTTPCode(err error) (int, bool) {
	var apiErr *sdkerrors.APIError
	if errors.As(err, &apiErr) {
		return apiErr.HTTPCode, true
	}
	var permErr *sdkerrors.PermissionError
	if errors.As(err, &permErr) {
		return permErr.HTTPCode, true
	}
	var paramErr *sdkerrors.ParameterError
	if errors.As(err, &paramErr) {
		return paramErr.HTTPCode, true
	}
	var authErr *sdkerrors.AuthenticationError
	if errors.As(err, &authErr) {
		return authErr.HTTPCode, true
	}
	return 0, false
}

// WrapError maps an SDK or network error to the appropriate BOSH CPI error type:
//   - nil → nil
//   - SDK 404 (APIError.IsNotFound) → non-retriable CloudError
//   - SDK 5xx (errors.Is ErrServer) → retriable RetriableCloudError
//   - SDK 429 / pushback phrase → retriable RetriableCloudError (pushback subtype)
//   - SDK 4xx non-404 non-429 → non-retriable CloudError
//   - net.Error Timeout() → retriable RetriableCloudError
//   - SDK ConnectionError or TimeoutError → retriable RetriableCloudError
//   - mid-request connection drop (IsTransportConnectionDrop) → retriable
//     RetriableCloudError
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

	// Mid-request connection drop (EOF, reset, broken pipe after the
	// connection was established) → retriable. The SDK's typed ConnectionError
	// covers only connections that never came up, so these would otherwise
	// fall through to the permanent fallback.
	if IsTransportConnectionDrop(err) {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			"PVE connection dropped mid-request: "+err.Error())
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
	// use in RetryOnTransientOrLock (retry.go).
	if IsClusterNotQuorate(err) {
		return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud,
			"cluster has lost quorum; mutations are blocked until quorum returns — check `pvecm status`: "+err.Error())
	}

	// Permanent 500-with-text: PVE returns a request-shaped rejection as a 500
	// body, which the generic APIError branch below would map to a retriable
	// "PVE server error". Checked first so the director gets ok_to_retry=false
	// and an operator-actionable message naming the volid. See
	// IsVolumeFormatUnknown.
	if IsVolumeFormatUnknown(err) {
		return cpierrors.Cloud(
			"PVE cannot determine a disk format for this volume ID — the volume ID is malformed "+
				"or names a path its storage plugin cannot resolve; this is permanent, not a transient "+
				"server fault: %s", err.Error())
	}

	// SDK API status classification: 404 vs 5xx vs 429 vs other 4xx. Resolved
	// through apiHTTPCode, not a bare errors.As against *APIError, because the
	// SDK returns 400 as *ParameterError, 401 as *AuthenticationError, and 403
	// as *PermissionError, each embedding APIError by VALUE; the bare check
	// missed exactly those codes, letting their bodies fall through to the
	// text classifiers below, where a pushback phrase in a 4xx body silently
	// flipped a request verdict to retriable. Every resolved status gets its
	// verdict here, ahead of any text matching.
	if code, ok := apiHTTPCode(err); ok {
		if IsNotFound(err) {
			return cpierrors.Cloud("resource not found: %s", err.Error())
		}
		// 5xx server error → retriable. The sentinel check keeps parity with
		// shapes that carry Code but not HTTPCode.
		if errors.Is(err, sdkerrors.ErrServer) || code >= 500 {
			return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, "PVE server error: "+err.Error())
		}
		// 429 Too Many Requests → retriable pushback.
		if code == 429 {
			return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, "PVE pushback (429): "+err.Error())
		}
		// Any other 4xx (or an unresolved code on a typed API error) is a
		// verdict about the request → non-retriable, whatever the body says.
		return cpierrors.Cloud("PVE API error: %s", err.Error())
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

// IsCloneToNonSharedStorage reports whether err is PVE's rejection of a
// cross-node clone whose DESTINATION storage is node-local:
//
//	can't clone to non-shared storage 'local-lvm-data'
//
// PVE requires BOTH sides of a cross-node clone to be shared: the template's
// own storage (the SDK's documented Target constraint) and the destination
// storage the new disks are written to. The rejection can surface with an
// SDK classification IsTransientTransport would match, but it is a PERMANENT
// configuration condition: retrying with a fresh VMID cannot help, so
// callers must treat it as non-retryable and surface the real cause instead
// of an "exhausted VMID allocation" message.
func IsCloneToNonSharedStorage(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "can't clone to non-shared storage")
}

// IsTransportConnectionDrop reports whether err signals a connection that the
// server closed after it was established, mid-request or mid-response: the Go
// transport surfaces these as io.EOF or io.ErrUnexpectedEOF inside the
// *url.Error, or as write-side ECONNRESET / EPIPE syscall errors. The live
// trigger is pveproxy under burst load (a CF deploy uploading many configdrive
// ISOs in parallel) recycling or shedding connections; a stale keep-alive
// connection the server closed between requests produces the same shape,
// because Go cannot auto-retry a streamed POST body.
//
// Most of these never reach the SDK's typed ConnectionError (that type
// historically modeled a connection that could not be established at all;
// SDK v3.9.1 additionally maps the drop shapes below to it, and the typed
// branch in WrapError handles that form), so without this predicate a
// mid-request drop falls through every transient classifier and surfaces as
// a permanent CloudError. Typed errors.Is checks first: the SDK preserves the
// chain with %w end to end, and textual matching on "eof" is far too loose.
// The single exception is net/http's unexported errServerClosedIdle sentinel
// ("http: server closed idle connection", the other arm of the keep-alive
// race whose io.EOF arm is covered above): it is a bare errors.New with no
// chain, so it is matched per unwrap link by full-string equality, never by
// substring. The exception list is named and bounded: this one sentinel, and
// nothing else, is matched textually here.
//
// Retrying a dropped mutation can replay a request the server already
// processed; that trade is identical to the timeout case (already classified
// transient), and the mutation call sites absorb it the same way they absorb
// a replayed timeout: clone retries VMID conflicts with per-candidate
// cleanup, the configdrive upload clears the target name before re-uploading,
// and volume creates sweep a partially committed volume after failure.
//
// nil → false.
func IsTransportConnectionDrop(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	// The client-side arm of the same race: the transport pool handed out a
	// connection that was already closed.
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	// The server-side keep-alive arm, whose stdlib sentinel carries no chain:
	// exact match per unwrap link (see the doc comment above).
	for e := err; e != nil; e = errors.Unwrap(e) {
		if e.Error() == serverClosedIdleMessage {
			return true
		}
	}
	return false
}

// serverClosedIdleMessage is the message of net/http's unexported
// errServerClosedIdle sentinel. See IsTransportConnectionDrop for the
// matching discipline.
const serverClosedIdleMessage = "http: server closed idle connection"

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
//   - Mid-request connection drops (IsTransportConnectionDrop): EOF, reset,
//     or broken pipe after the connection was established.
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
	// Permanent 500-with-text shapes are excluded before the blanket 5xx rule
	// below: PVE answers a request-shaped rejection with a 500 body rather than
	// a 4xx, so "is a 5xx" alone cannot distinguish a cycling worker from a
	// verdict that will never change. See IsVolumeFormatUnknown.
	if IsVolumeFormatUnknown(err) {
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
	if IsTransportConnectionDrop(err) {
		return true
	}
	// A failure that carries a resolved non-429 4xx status is a verdict about
	// the request (a rejected credential, a denied grant), not a transport
	// fault: the textual rescues below must not resurrect it. SDK v3.9.1 and
	// later preserve the status chain through login failures, so a 401 login
	// resolves here and classifies permanent even though its message contains
	// the no-ticket sentinel text.
	if code, ok := apiHTTPCode(err); ok && code >= 400 && code < 500 && code != 429 {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "failed to parse login response") {
		return true
	}
	if strings.Contains(msg, "auto-login failed") {
		return true
	}
	// Safety net for SDK versions predating v3.9.1, whose ticket-login failure
	// surfaced as this bare sentinel with the real cause (typically a 5xx from
	// a cycling pveproxy) discarded. Retriable because a ticket login that a
	// live cluster refuses to answer is overwhelmingly a transient server
	// fault: a genuinely rejected credential answers 401 and carries a
	// different message. v3.9.1 and later preserve the status chain, so the
	// typed branches above classify first. API-token deployments never hit
	// this path.
	if strings.Contains(msg, "authentication failed: no ticket received") {
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
	// The cluster-wide cfs lock on a shared storage (pmxcfs, not the local
	// file lock above): concurrent qmclone/qmcreate/qmdestroy tasks against
	// one shared pool (e.g. RBD during a mass VM creation) contend on it and
	// PVE fails the task with "cfs-lock 'storage-<pool>' error: got lock
	// request timeout". Pure contention -- retriable. Other cfs-lock errors
	// (e.g. "no quorum!") deliberately do not match.
	if strings.Contains(msg, "cfs-lock") && strings.Contains(msg, "got lock request timeout") {
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
// message) and RetryOnTransientOrLock (which routes retries onto the
// storage-lock curve) in this package.
//
// nil → false.
func IsClusterNotQuorate(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not quorate") || strings.Contains(msg, "no quorum")
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

// IsPoolNotEmpty reports whether err signals that PVE refused to delete a
// resource pool because it still has member VMs/containers. Live shape
// (PVE 9.2.4, always HTTP 500 + text, never 409/404):
//
//	pool 'bosh' is not empty (contains VM 99098)
//
// Matched with a "pool" adjacency guard (both substrings must be present) so
// an unrelated "is not empty" message from another PVE subsystem is not
// misclassified. delete_vm's empty-pool reaper treats this as a benign race
// (a VM landed in the pool between the emptiness check and the delete) and
// simply skips the reap rather than failing the destroy.
//
// nil → false.
func IsPoolNotEmpty(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "pool") && strings.Contains(msg, "is not empty")
}

// IsPoolNotFound reports whether err signals that a referenced resource pool
// does not exist. Live shape (PVE 9.2.4, HTTP 500 + text, never 404):
//
//	delete pool failed: pool 'x' does not exist
//
// The same shape comes back from a plain read (GET /pools/{poolid}) of a pool
// that has not been created yet:
//
//	pool 'bosh-templates' does not exist
//
// This is deliberately distinct from the generic 404-based IsNotFound: PVE
// pool errors are always surfaced as 500 with a text body, so IsNotFound
// never matches them. Callers needing an already-gone signal for a pool
// (the delete_vm empty-pool reaper's tolerate-already-deleted branch, and
// sdkPoolService.GetPoolComment's absent-pool mapping) must use this
// classifier instead. Matched with a "pool" adjacency guard so an unrelated
// "does not exist" message (e.g. "storage does not exist") is not
// misclassified.
//
// nil → false.
func IsPoolNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "pool") && strings.Contains(msg, "does not exist")
}

// IsVolumeFormatUnknown reports whether err is PVE's refusal to inspect a
// volume whose volid it cannot parse into a known disk format. Live shape
// (PVE 9.2, HTTP 500 + text, never 4xx):
//
//	volume_size_info on 'nfs-images:9999/vm-9999-disk-0.qcow2' failed - no format
//
// PVE's storage layer raises this from volume_size_info when the volid names a
// directory-style path on file storage that its plugin cannot resolve to a
// format. It is a permanent, request-shaped verdict about the volid the caller
// supplied — the same request fails identically forever — but it arrives as a
// 500, so the blanket 5xx rule in IsTransientTransport classified it transient
// and every retry helper burned its full budget (observed live: 8 attempts,
// ~29.9s on a has_disk probe) before surfacing the identical error. Both
// IsTransientTransport and WrapError check this first so the error surfaces
// immediately and non-retriably.
//
// Matched with a "volume_size_info" adjacency guard (both substrings must be
// present) so an unrelated 500 mentioning a format — a genuinely transient
// server fault whose text happens to include the word — is not misclassified
// as permanent. Reachable only with a malformed disk CID; a CPI-issued disk
// CID always names a well-formed volid.
//
// nil → false.
func IsVolumeFormatUnknown(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "volume_size_info") && strings.Contains(msg, "no format")
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
