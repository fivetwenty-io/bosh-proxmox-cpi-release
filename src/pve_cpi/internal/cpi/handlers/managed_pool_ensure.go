package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"unicode"
	"unicode/utf8"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// managedExistingPool proves a concurrent create was rejected before mutation.
// It does not establish CPI ownership of the pool or permission to delete it.
type managedExistingPool struct{ poolID string }

// EnsurePoolExists handles idempotence inside the guard. Direct CreatePool keeps
// exclusive semantics because pool-based locks require exclusive creation; it
// hands a refused sentinel create back unchanged (see managed_pool_lock.go).
func (t *managedPoolService) EnsurePoolExists(ctx context.Context, poolID, comment string) error {
	found, err := t.existingPool(ctx, poolID)
	if err != nil || found {
		return err
	}
	m := ManagedAllocationMutation{Service: managedDiskServicePool, Method: "CreatePool", Args: map[string]any{managedArgumentPoolID: poolID, "comment": comment}}
	token, err := t.guard.begin(ctx, m)
	if err != nil {
		return err
	}
	defer t.guard.end(ctx, m, token)
	err = t.PoolService.CreatePool(ctx, poolID, comment)
	var result any
	if exactPoolAlreadyExists(err, poolID) {
		_, found, readErr := t.GetPoolComment(ctx, poolID)
		switch {
		case readErr != nil:
			err = readErr
		case !found:
			err = fmt.Errorf("concurrent pool creation was not observed")
		default:
			err = nil
			result = managedExistingPool{poolID: poolID}
		}
	}
	return t.guard.finish(ctx, m, token, result, err)
}

func (t *managedPoolService) existingPool(ctx context.Context, poolID string) (bool, error) {
	t.guard.mu.Lock()
	defer t.guard.mu.Unlock()
	if t.guard.poisoned != nil {
		return false, t.guard.poisoned
	}
	_, found, err := t.GetPoolComment(ctx, poolID)
	return found, err
}

// PVE Pool.pm rejects this exact condition under its user.cfg lock before
// changing the pool. Generic conflicts, substrings and transport failures are
// insufficient, even if a later read happens to find the requested pool.
func exactPoolAlreadyExists(err error, poolID string) bool {
	verdict := "pool '" + poolID + "' already exists"
	return exactPoolVerdict(err, verdict, "create pool failed: "+verdict)
}

// exactPoolDoesNotExist is the delete-side counterpart. Pool.pm raises it under
// the same lock, before it deletes anything.
func exactPoolDoesNotExist(err error, poolID string) bool {
	verdict := "pool '" + poolID + "' does not exist"
	return exactPoolVerdict(err, verdict, "delete pool failed: "+verdict)
}

// exactPoolReadMissing matches the verdict a GET /pools/{poolid} returns for a
// missing pool. read_pool reuses the index handler, which raises the verdict
// outside lock_user_config, so it carries no prefix.
func exactPoolReadMissing(err error, poolID string) bool {
	return exactPoolVerdict(err, "pool '"+poolID+"' does not exist")
}

// exactPoolVerdict matches a Pool.pm verdict as a whole message. Pool.pm raises
// its create and delete verdicts inside lock_user_config, which re-raises them
// as "<errmsg>: <verdict>", so a live cluster answers HTTP 500 with the
// prefixed form in the body's message field. The bare verdict is accepted as
// well because it names the same condition on the same pool. One trailing
// whitespace character, which is the newline Perl's die carries, is tolerated.
// Anything else, including a second trailing character, extra text, or a
// field-error map, is not proof.
func exactPoolVerdict(err error, accepted ...string) bool {
	var apiErr *sdkerrors.APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPCode != http.StatusInternalServerError || len(apiErr.Errors) != 0 {
		return false
	}
	message := apiErr.Message
	if last, size := utf8.DecodeLastRuneInString(message); size > 0 && unicode.IsSpace(last) {
		message = message[:len(message)-size]
	}
	return slices.Contains(accepted, message)
}
