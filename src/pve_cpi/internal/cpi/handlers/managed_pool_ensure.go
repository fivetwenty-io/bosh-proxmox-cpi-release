package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// managedExistingPool proves a concurrent create was rejected before mutation.
// It does not establish CPI ownership of the pool or permission to delete it.
type managedExistingPool struct{ poolID string }

// EnsurePoolExists handles idempotence inside the guard. Direct CreatePool is
// deliberately unchanged because pool-based locks require exclusive creation.
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
	var apiErr *sdkerrors.APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPCode != http.StatusInternalServerError || len(apiErr.Errors) != 0 {
		return false
	}
	return strings.TrimSuffix(apiErr.Message, "\n") == "pool '"+poolID+"' already exists"
}
