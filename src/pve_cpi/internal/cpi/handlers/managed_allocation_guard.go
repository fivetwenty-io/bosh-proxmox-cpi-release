package handlers

import (
	"context"
	"errors"
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
	// A refusal that PVE made before changing anything reaches After too, with
	// the refusal proof as its result (see settleRefused).
	After func(context.Context, ManagedAllocationMutation, string, any) error
	// Failed persists uncertainty after either a service or After failure.
	Failed func(context.Context, ManagedAllocationMutation, string, error) error
	// SettleProtectionWrites opts a guard into finishing a failed
	// protection-only write without locking itself (see
	// classifyProtectionWriteFailure). A write PVE refused is passed to After
	// as a managedProtectionWriteRefusal and observed; a write cut off before
	// PVE answered is left planned for the settler. Only a guard whose Before
	// records protection-only parameters on the step may set it, because the
	// settler reads those parameters to find the step later.
	SettleProtectionWrites bool
	// SettleFailedWrite, when set, gets a write that PVE answered with an
	// error before the guard treats the failure as uncertain. It returns true
	// only when it read back that the write changed nothing but what it
	// settled in the journal, such as a slot delete that PVE recorded as
	// pending before the unplug failed busy. The guard then stays usable and
	// hands the failure back unchanged, so a caller can still classify it and
	// its follow-up writes, such as the revert, still pass.
	SettleFailedWrite func(context.Context, ManagedAllocationMutation, string, error) bool
}

// ManagedAllocationGuard serializes writes and blocks further mutations after uncertainty.
//
// One invariant holds this together and nothing enforces it mechanically, so it
// is written down here. A guarded UpdateQemuConfig that carries a Description is
// checked byte-for-byte against the frozen allocation marker, in
// managedVMTarget.validate and again in the evidence comparison. That check is
// only survivable while the description still reads exactly as the allocation
// wrote it. Once createManagedVMRoot returns, create_vm annotates the
// description with pool membership and stemcell provenance, so from that point
// on no write under the guard may carry a Description. The annotating write
// goes through unguardedPVE for precisely this reason, pveConfigKeyDescription
// is blocklisted in pve_config so an operator cannot reintroduce one, and the
// remaining description writers all sit on attach_disk, detach_disk,
// set_*_metadata, or delete_vm paths that run outside an allocation. A new
// description-bearing write added under the guard after root creation would
// fail create_vm and poison the allocation, and it would do it silently.
type ManagedAllocationGuard struct {
	mu       sync.Mutex
	original pve.Client
	hooks    ManagedAllocationHooks
	poisoned error
	// lockRefusals holds, per sentinel, the claim that last refused a guarded
	// create and the refusal PVE returned for it. See repeatLockRefusal.
	lockRefusals map[string]lockRefusal
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
		return "", &managedMutationNotAttempted{err: err}
	}
	token, err := g.hooks.Before(ctx, m)
	if err == nil && token == "" {
		err = fmt.Errorf("managed mutation did not produce durable intent identity")
	}
	if errors.Is(err, errManagedRequestEnded) || errors.Is(err, errManagedDescriptionDigestStale) ||
		errors.Is(err, errManagedTailReadFailed) || errors.Is(err, errManagedAdmissionReadFailed) {
		// The request's context ended before or during the checks for this
		// mutation, a description-only write carried a digest the guard's own
		// read no longer matches, or a read the guard makes to admit the
		// mutation returned an error, and the refusal came before any intent
		// was written or PVE was called. Nothing is uncertain, so the guard
		// stays usable, the way a cut-off protection write leaves it.
		g.mu.Unlock()
		return "", &managedMutationNotAttempted{err: err}
	}
	if errors.Is(err, errManagedRestoreChecksIncomplete) {
		// The checks that admit a parker protection restore could not
		// finish, and Before recorded the restore as a planned step instead
		// of sending it. Nothing was sent, and the record already tells the
		// next call to read the parker back, so the guard stays usable. The
		// refusal goes back unchanged, so the restore can say it was not sent
		// rather than that the operation was already uncertain.
		g.mu.Unlock()
		return "", err
	}
	if err != nil {
		g.poisoned = cpierrors.Cloud("managed allocation blocked before %s.%s", m.Service, m.Method)
		g.mu.Unlock()
		return "", &managedMutationNotAttempted{err: g.poisoned}
	}
	return token, nil
}

// managedMutationNotAttempted is the error begin returns when it refuses a
// mutation before the service call, so the write never reached PVE. Its text
// and CPI type are the refusal's own; it also matches
// pve.ErrMutationNotAttempted, so a caller that reports on the write, such as
// the parker protection restore, can say it was not attempted instead of
// blaming PVE or the network.
type managedMutationNotAttempted struct{ err error }

func (e *managedMutationNotAttempted) Error() string { return e.err.Error() }

func (e *managedMutationNotAttempted) Unwrap() []error {
	return []error{e.err, pve.ErrMutationNotAttempted}
}
func (g *ManagedAllocationGuard) finish(ctx context.Context, m ManagedAllocationMutation, token string, result any, err error) error {
	if err != nil && g.hooks.SettleProtectionWrites {
		switch classifyProtectionWriteFailure(ctx, m, err) {
		case protectionWriteCutOff:
			// The step stays planned and the guard stays usable; the caller
			// gets the failure unchanged.
			return err
		case protectionWriteRefused:
			if g.hooks.After(ctx, m, token, managedProtectionWriteRefusal{}) == nil {
				return err
			}
			// A refusal that cannot be observed is uncertain like any other
			// failure, and falls through to the lock below.
		case protectionWriteOther:
		}
	}
	if err != nil && g.hooks.SettleFailedWrite != nil && g.hooks.SettleFailedWrite(ctx, m, token, err) {
		return err
	}
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

// settleRefused closes the intent of a mutation that PVE provably refused
// before changing anything. After receives the refusal proof as its result and
// settles the planned step, and the refusal goes back to the caller unchanged
// so that it can still classify it. If After cannot settle the step, the
// refusal is treated like any other failure.
func (g *ManagedAllocationGuard) settleRefused(ctx context.Context, m ManagedAllocationMutation, token string, proof any, refusal error) error {
	if err := g.hooks.After(ctx, m, token, proof); err != nil {
		return g.finish(ctx, m, token, nil, refusal)
	}
	return refusal
}

type managedAllocationClient struct {
	pve.Client
	guard *ManagedAllocationGuard
}

// guardWrappedClient is implemented by every client decorator that a managed
// allocation guard installs. Each one reports the client it wraps, which is how
// unguardedPVE walks back out to the client the CPI built.
type guardWrappedClient interface {
	unguardedClient() pve.Client
}

func (c *managedAllocationClient) unguardedClient() pve.Client { return c.Client }

// unguardedPVE returns the client underneath every managed allocation guard
// decorator wrapping c, and returns c itself when no guard wraps it.
//
// The parker pool sweep is the caller. A guard's admission hook refuses every
// pool outside bosh-lock-, and one refusal poisons the whole allocation, so a
// cosmetic pool call made on a guarded client would fail the detach or the
// attach whose park has already landed. The park funnels cannot tell which
// client they hold, because the managed lifecycle paths shadow their own deps
// with a guarded copy before they call one, so the unwrapping happens here
// rather than at each funnel.
//
// The walk is bounded so that a decorator which ever returned a client wrapping
// itself cannot spin. Two decorators exist today and the bound leaves room for
// more.
func unguardedPVE(c pve.Client) pve.Client {
	for range 8 {
		wrapper, ok := c.(guardWrappedClient)
		if !ok {
			return c
		}
		inner := wrapper.unguardedClient()
		if inner == nil || inner == c {
			return c
		}
		c = inner
	}
	return c
}

// holderDeletionAnswerer is implemented by a client decorator whose guard
// decides which holder VMs an operation may delete.
type holderDeletionAnswerer interface {
	deletesHolder(vmid int) bool
}

// guardDeletesHolder reports whether a delete of the VM with this VMID, sent
// through c, would get past the guard that wraps c. It walks the same
// decorator chain as unguardedPVE and returns the first answer a decorator
// gives. A client that no guard wraps has nothing to refuse the delete, so the
// answer is yes.
//
// The answer covers only who created the VM. Whether the VM is empty stays
// with the caller's own check, which pve.DestroyEmptyMover makes before it
// writes anything.
func guardDeletesHolder(c pve.Client, vmid int) bool {
	for range 8 {
		if answerer, ok := c.(holderDeletionAnswerer); ok {
			return answerer.deletesHolder(vmid)
		}
		wrapper, ok := c.(guardWrappedClient)
		if !ok {
			return true
		}
		inner := wrapper.unguardedClient()
		if inner == nil || inner == c {
			return true
		}
		c = inner
	}
	return true
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
		return pve.ErrAuditVisibilityReaderUnavailable
	}
	return reader.StorageAuditVisibility(ctx)
}
