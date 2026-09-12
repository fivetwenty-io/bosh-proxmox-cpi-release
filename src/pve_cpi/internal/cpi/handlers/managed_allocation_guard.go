package handlers

import (
	"context"
	"fmt"
	"sync"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

const (
	managedArgumentParams      = "params"
	managedArgumentStorageName = "storageName"
	managedArgumentPoolID      = "poolID"
	managedArgumentVnet        = "vnet"
	managedServiceQEMU         = "QEMU"
	managedServiceNodes        = "Nodes"
	managedServiceCluster      = "Cluster"
)

// ManagedAllocationMutation is in-memory call context, never journal payload.
// Args may contain credentials. Hooks must persist only allowlisted identities.
type ManagedAllocationMutation struct {
	Service, Method string
	Args            map[string]any
}

// ManagedAllocationHooks bind mutation admission, readback, and uncertainty to the journal.
type ManagedAllocationHooks struct {
	// Before revalidates and durably writes intent before the service is called.
	Before func(context.Context, ManagedAllocationMutation) (string, error)
	// After must await asynchronous tasks and prove the mutation by readback before
	// marking it observed. Use the original client, not the decorated write path.
	After func(context.Context, ManagedAllocationMutation, string, any) error
	// Failed persists uncertainty after either a service or After failure.
	Failed func(context.Context, ManagedAllocationMutation, string, error) error
}

// ManagedAllocationGuard serializes writes and blocks further mutations after uncertainty.
type ManagedAllocationGuard struct {
	mu       sync.Mutex
	original pve.Client
	hooks    ManagedAllocationHooks
	poisoned error
}

// NewManagedAllocationGuard requires all three durable evidence hooks.
func NewManagedAllocationGuard(client pve.Client, hooks ManagedAllocationHooks) (*ManagedAllocationGuard, error) {
	if client == nil || hooks.Before == nil || hooks.After == nil || hooks.Failed == nil {
		return nil, fmt.Errorf("managed allocation guard requires client and all evidence hooks")
	}
	return &ManagedAllocationGuard{original: client, hooks: hooks}, nil
}

// Client decorates mutation services while preserving read-only access.
func (g *ManagedAllocationGuard) Client() pve.Client {
	return &managedAllocationClient{Client: g.original, guard: g}
}

// Err reports the first unresolved failure that prevents further writes.
func (g *ManagedAllocationGuard) Err() error { g.mu.Lock(); defer g.mu.Unlock(); return g.poisoned }

// Poison prevents retries, fallback, and nested cleanup after an uncertain read
// outside an intercepted method (for example a later task wait or agent probe).
func (g *ManagedAllocationGuard) Poison(err error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.poisoned == nil {
		if err == nil {
			err = cpierrors.Cloud("managed allocation requires reconciliation")
		}
		g.poisoned = err
	}
	return g.poisoned
}
func (g *ManagedAllocationGuard) begin(ctx context.Context, m ManagedAllocationMutation) (string, error) {
	g.mu.Lock()
	defer func() {
		if recovered := recover(); recovered != nil {
			g.poisoned = cpierrors.Cloud("managed allocation admission panicked; reconciliation required")
			g.mu.Unlock()
			panic(recovered)
		}
	}()
	if g.poisoned != nil {
		err := g.poisoned
		g.mu.Unlock()
		return "", err
	}
	token, err := g.hooks.Before(ctx, m)
	if err == nil && token == "" {
		err = fmt.Errorf("managed mutation did not produce durable intent identity")
	}
	if err != nil {
		g.poisoned = cpierrors.Cloud("managed allocation blocked before %s.%s", m.Service, m.Method)
		g.mu.Unlock()
		return "", g.poisoned
	}
	return token, nil
}
func (g *ManagedAllocationGuard) finish(ctx context.Context, m ManagedAllocationMutation, token string, result any, err error) error {
	if err == nil {
		err = g.hooks.After(ctx, m, token, result)
	}
	if err != nil {
		g.poisoned = cpierrors.Cloud("managed allocation requires reconciliation after %s.%s", m.Service, m.Method)
		reported := g.hooks.Failed(ctx, m, token, err)
		if reported == nil {
			reported = cpierrors.Cloud("managed allocation requires reconciliation after %s.%s", m.Service, m.Method)
		}
		// Never propagate an upstream retriable error after submission.
		g.poisoned = cpierrors.Cloud("%s", reported.Error())
		return g.poisoned
	}
	return nil
}

type managedAllocationClient struct {
	pve.Client
	guard *ManagedAllocationGuard
}

func (g *ManagedAllocationGuard) end(ctx context.Context, m ManagedAllocationMutation, token string) {
	defer g.mu.Unlock()
	if recovered := recover(); recovered != nil {
		if g.poisoned == nil {
			g.poisoned = cpierrors.Cloud("managed allocation mutation panicked; reconciliation required")
			if err := g.finish(ctx, m, token, nil, fmt.Errorf("mutation panicked")); err != nil {
				g.poisoned = err
			}
		}
		panic(recovered)
	}
}

// Read-only audit capability survives the mutation decorator.
func (c *managedAllocationClient) StorageAuditVisibility(ctx context.Context) error {
	reader, ok := c.Client.(pve.StorageAuditVisibilityReader)
	if !ok {
		return fmt.Errorf("allocation audit visibility reader unavailable")
	}
	return reader.StorageAuditVisibility(ctx)
}
