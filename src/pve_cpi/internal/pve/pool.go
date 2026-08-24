// Resource pool provisioning helpers shared by create_vm and create_stemcell:
// idempotent create-if-missing plus a standard provenance comment marking
// pools the CPI created (vs. an operator's pre-existing pool), so the
// delete_vm reaper can tell the two apart before ever deleting a pool.
package pve

import (
	"context"
	"errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// PoolProvenanceComment is the comment the CPI writes on every resource pool
// it creates. delete_vm's opt-in empty-pool reaper (see
// reapEmptyPoolIfManaged in internal/cpi/handlers/delete_vm.go) checks a
// pool's comment for this prefix before ever deleting it, so an operator's
// pre-existing pool is never touched.
const PoolProvenanceComment = "managed by bosh-pve-cpi"

// PoolProvenance builds the comment recorded on a CPI-created pool. When
// director is non-empty, it is appended in a parenthetical so multiple BOSH
// directors sharing one PVE cluster can be told apart in the PVE UI/API; an
// empty director yields the bare PoolProvenanceComment.
func PoolProvenance(director string) string {
	if director == "" {
		return PoolProvenanceComment
	}
	return PoolProvenanceComment + " (director " + director + ")"
}

// EnsurePoolExists creates the PVE resource pool poolID with comment if it
// does not already exist, tolerating a concurrent/prior creation of the same
// pool. It is the single create-if-missing entry point used by both the
// create_vm resolved-pool path and the create_stemcell template-pool path.
//
// Inputs: ctx must be non-nil; c must be non-nil and its Pools() must return
// a non-nil PoolService (several call/test paths construct a Client without
// a pool service — see the nil-Pools guard below); poolID must be non-empty.
// comment may be empty (sent to PVE as-is by PoolService.CreatePool). logger
// may be nil (retry attempts then go unlogged).
//
// Every pool mutation cluster-wide serializes on PVE's cfs_lock_file
// ('user_cfg'), so concurrent creates (parallel deploys, a stemcell upload
// racing a VM create) can time out on pure lock contention. The create rides
// RetryOnTransientOrLock rather than surfacing the first lock timeout;
// "already exists" answers, from this call or a concurrent winner, stop the
// retry immediately because the pool existing is the desired end state.
//
// Failure modes:
//   - ctx == nil, c == nil, or poolID == "": non-retriable CloudError, no PVE
//     call attempted.
//   - c.Pools() == nil: non-retriable CloudError naming the missing service,
//     no PVE call attempted (defends test/wiring gaps rather than panicking
//     on a nil-interface method call).
//   - CreatePool succeeds (err == nil): pool now exists, return nil.
//   - CreatePool fails with the live PVE "pool already exists" 500+text shape
//     (isPoolAlreadyExists, cluster_lock.go): treated as success — the pool
//     existing is the desired end state, whether this call or a concurrent
//     one created it.
//   - Any other CreatePool error (auth failure, malformed poolID, quota,
//     transient transport fault after the retry budget, ...): wrapped via
//     WrapError and returned so callers see the correct retriable/
//     non-retriable classification.
//
// The retry rides the full storage-lock attempt budget even on the
// create_vm request path, deliberately: every pool mutation cluster-wide
// serializes on the single pmxcfs user_cfg lock, so a burst deploy contends
// here by design, and giving up after the smaller cleanup-sweep budget
// would fail creates that one more backoff window would have completed. The
// opt-in operation_timeout envelope remains the operator's ceiling.
func EnsurePoolExists(ctx context.Context, c Client, poolID, comment string, logger *log.Logger) error {
	if ctx == nil {
		return cpierrors.Cloud("EnsurePoolExists: ctx must not be nil")
	}
	if c == nil {
		return cpierrors.Cloud("EnsurePoolExists: PVE client must not be nil")
	}
	if poolID == "" {
		return cpierrors.Cloud("EnsurePoolExists: poolID must not be empty")
	}
	pools := c.Pools()
	if pools == nil {
		return cpierrors.Cloud("EnsurePoolExists: PVE client has no pool service")
	}

	err := RetryOnTransientOrLock(ctx, logger, "pool.ensure_exists", 0, func() error {
		createErr := pools.CreatePool(ctx, poolID, comment)
		if createErr != nil && isPoolAlreadyExists(createErr) {
			// Goal state reached (concurrent winner or earlier attempt whose
			// response was dropped); do not spend retry budget on it.
			return nil
		}
		return createErr
	})
	if err != nil {
		return cpierrors.Wrap(WrapError(err), "EnsurePoolExists: create pool "+poolID)
	}
	return nil
}

// IsPoolPermissionDenied reports whether err signals that PVE denied read
// access to a resource pool -- HTTP 401 (invalid credentials) or 403 (valid
// credentials, missing grant, e.g. Pool.Audit on /pool/<name>) -- as opposed
// to a transient network/server fault or the pool simply not existing yet
// (PoolService.GetPoolComment already maps a not-found pool to
// found=false with a nil error, so that case never reaches this classifier).
//
// Used by the CPI startup preflight (cmd/cpi/main.go) to fail fast with an
// actionable grant-naming message on a genuine permission problem, while
// treating every other GetPoolComment error as transient (Warn-only, does
// not block boot) -- a startup-time PVE API hiccup must not be
// indistinguishable from a misconfigured token.
func IsPoolPermissionDenied(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, sdkerrors.ErrUnauthorized) || errors.Is(err, sdkerrors.ErrForbidden) {
		return true
	}
	var apiErr *sdkerrors.APIError
	if errors.As(err, &apiErr) {
		return apiErr.IsUnauthorized() || apiErr.IsForbidden()
	}
	var permErr *sdkerrors.PermissionError
	if errors.As(err, &permErr) {
		return true
	}
	var authErr *sdkerrors.AuthenticationError
	return errors.As(err, &authErr)
}
