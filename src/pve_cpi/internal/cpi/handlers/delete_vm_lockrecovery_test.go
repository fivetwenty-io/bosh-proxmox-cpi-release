// Package handlers_test -- delete_vm's synchronous destroy path: root@pam-only
// skiplock recovery when PVE rejects DeleteQemu because the guest config
// carries an in-flight lock (a killed worker or node reboot mid-clone/
// mid-create leaves lock: clone|create behind).
package handlers_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
)

const lockedCloneDestroyMsg = "500 unable to destroy VM 100: VM is locked (clone)\n"

// noLockGuestConfig backs the qemu.Config calls the guard steps
// (guardUnusedVolumes, detachForeignActiveDisks, detachRetainedEphemeralDisk)
// make before the destroy call: an empty config means no unusedN entries, no
// foreign-VMID disks, and no retain-ephemeral tag, so every guard is a no-op
// and the test isolates the destroy skiplock behavior.
func noLockGuestConfig(_ context.Context, _ string, _ int) (map[string]any, error) {
	return map[string]any{}, nil
}

func TestHandleDeleteVM_LockedDestroy_RootPam_RetriesWithSkiplockAndSucceeds(t *testing.T) {
	t.Parallel()

	var deleteCalls []*nodes.DeleteQemuParams
	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil // synchronous stop, no lock
		},
		configFn: noLockGuestConfig,
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, params *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			deleteCalls = append(deleteCalls, params)
			if len(deleteCalls) == 1 {
				return nil, errors.New(lockedCloneDestroyMsg)
			}
			// Skiplock retry succeeds, synchronous completion (empty response).
			return &nodes.DeleteQemuResponse{}, nil
		},
	}
	tasksSvc := &mockTasksService{}
	agentSvc := &mockAgentService{}

	deps := testDepsFoundVM(100, qemuSvc, nodesSvc, tasksSvc, agentSvc)
	deps.Config.APIToken = ""
	deps.Config.User = "root"
	deps.Config.Realm = "pam"
	deps.Config.Password = "secret"

	h := handlers.HandleDeleteVM(deps)
	result, err := h.Handle(context.Background(), marshalArgs("100"), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("root@pam skiplock retry should succeed, got error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %v", result)
	}
	if len(deleteCalls) != 2 {
		t.Fatalf("expected 2 DeleteQemu calls (initial + skiplock retry), got %d", len(deleteCalls))
	}
	first, second := deleteCalls[0], deleteCalls[1]
	if first.Skiplock != nil && *first.Skiplock {
		t.Error("first DeleteQemu call must not carry skiplock=true")
	}
	if second.Skiplock == nil || !*second.Skiplock {
		t.Error("retry DeleteQemu call must carry skiplock=true")
	}
}

// TestHandleDeleteVM_LockedDestroy_RootPamOwnedToken_ReturnsActionableRetriableError
// verifies the corrected behavior: an API token OWNED by root@pam still does
// not qualify for the skiplock retry -- PVE rejects skiplock for any token
// identity regardless of the owning user, so the original lock error
// surfaces unretried, identically to any other non-qualifying identity.
func TestHandleDeleteVM_LockedDestroy_RootPamOwnedToken_ReturnsActionableRetriableError(t *testing.T) {
	t.Parallel()

	var deleteCalls []*nodes.DeleteQemuParams
	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: noLockGuestConfig,
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, params *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			deleteCalls = append(deleteCalls, params)
			return nil, errors.New(lockedCloneDestroyMsg)
		},
	}
	tasksSvc := &mockTasksService{}
	agentSvc := &mockAgentService{}

	deps := testDepsFoundVM(100, qemuSvc, nodesSvc, tasksSvc, agentSvc)
	deps.Config.APIToken = "root@pam!bosh-cpi=uuid"

	h := handlers.HandleDeleteVM(deps)
	_, err := h.Handle(context.Background(), marshalArgs("100"), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected an error when the guest config stays locked under a root@pam-owned token identity")
	}
	if len(deleteCalls) != 1 {
		t.Fatalf("root@pam-owned token: expected exactly 1 DeleteQemu call (no skiplock retry), got %d", len(deleteCalls))
	}
	if deleteCalls[0].Skiplock != nil && *deleteCalls[0].Skiplock {
		t.Error("the single DeleteQemu call must not carry skiplock=true for a root@pam-owned token identity")
	}

	var ce *cpierrors.Error
	if !errors.As(err, &ce) {
		t.Fatalf("expected a *cpierrors.Error, got %T: %v", err, err)
	}
	if !ce.OkToRetry() {
		t.Errorf("locked-VM error must be retriable (director should re-drive after `qm unlock`), got: %v", err)
	}
}

func TestHandleDeleteVM_LockedDestroy_NonRootPam_ReturnsActionableRetriableError(t *testing.T) {
	t.Parallel()

	var deleteCalls []*nodes.DeleteQemuParams
	qemuSvc := &mockQEMUService{
		stopFn: func(_ context.Context, _ string, _ int) (string, error) {
			return "", nil
		},
		configFn: noLockGuestConfig,
	}
	nodesSvc := &mockNodesService{
		deleteQemuFn: func(_ context.Context, _ string, _ string, params *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			deleteCalls = append(deleteCalls, params)
			return nil, errors.New(lockedCloneDestroyMsg)
		},
	}
	tasksSvc := &mockTasksService{}
	agentSvc := &mockAgentService{}

	// testDepsFoundVM's default identity (APIToken "test-token", no "!"
	// separator) is already non-root@pam; set an explicit non-root token to
	// make the scenario unambiguous in the test itself.
	deps := testDepsFoundVM(100, qemuSvc, nodesSvc, tasksSvc, agentSvc)
	deps.Config.APIToken = "bosh@pve!bosh-token=uuid"

	h := handlers.HandleDeleteVM(deps)
	_, err := h.Handle(context.Background(), marshalArgs("100"), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected an error when the guest config stays locked under a non-root@pam identity")
	}
	if len(deleteCalls) != 1 {
		t.Fatalf("non-root@pam identity: expected exactly 1 DeleteQemu call (no skiplock retry), got %d", len(deleteCalls))
	}
	if deleteCalls[0].Skiplock != nil && *deleteCalls[0].Skiplock {
		t.Error("the single DeleteQemu call must not carry skiplock=true for a non-root@pam identity")
	}

	var ce *cpierrors.Error
	if !errors.As(err, &ce) {
		t.Fatalf("expected a *cpierrors.Error, got %T: %v", err, err)
	}
	if !ce.OkToRetry() {
		t.Errorf("locked-VM error must be retriable (director should re-drive after `qm unlock`), got: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"qm unlock", "100", "clone"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message %q missing expected substring %q", msg, want)
		}
	}
}
