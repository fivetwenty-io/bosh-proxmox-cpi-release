package handlers_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// listedVMMockNodes is vmMockNodes with a cluster that lists one online node,
// so a create gets past the node listing and reaches the journal's open.
type listedVMMockNodes struct{ *vmMockNodes }

func (listedVMMockNodes) ListNodes(context.Context) (*sdknodes.ListNodesResponse, error) {
	listed := sdknodes.ListNodesResponse{json.RawMessage(`{"node":"pve","status":"online"}`)}
	return &listed, nil
}

// journalSetupVMDeps is buildVMDeps with a cluster that lists its node.
func journalSetupVMDeps(q *vmMockQEMU) handlers.Deps {
	n := &vmMockNodes{}
	deps := buildVMDeps(q, n, &vmMockCluster{}, &vmMockAgent{})
	deps.PVE.(*mockPVEClient).nodesSvc = listedVMMockNodes{n}
	return deps
}

// assertCreateRefusal checks that create_vm refused with a non-retriable
// CloudError that carries every fragment, and that it created nothing.
func assertCreateRefusal(t *testing.T, err error, q *vmMockQEMU, want []string) {
	t.Helper()
	if !cpierrors.IsType(err, cpierrors.TypeCloud) || cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("create_vm error = %v, want a non-retriable CloudError", err)
	}
	for _, fragment := range want {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("create_vm error %q does not contain %q", err.Error(), fragment)
		}
	}
	if len(q.createCalls) != 0 || len(q.startCalls) != 0 {
		t.Fatalf("create_vm fell back to the legacy create: %d creates, %d starts",
			len(q.createCalls), len(q.startCalls))
	}
}

// TestCreateVMNamesTheJournalSetupCause sets the allocation journal directory
// with no namespace, then a namespace that was never enrolled, and then a
// directory that was never provisioned, and runs create_vm on a cluster whose
// node lists, so the create reaches the journal. Each refuses with a
// non-retriable CloudError that names the missing piece, and none falls back
// to the legacy create, because the journal directory may hold records for VMs
// that path would allocate again.
func TestCreateVMNamesTheJournalSetupCause(t *testing.T) {
	for _, tc := range []struct {
		name, namespace string
		unprovisioned   bool
		want            []string
	}{
		{"journal directory without a namespace", "", false, []string{
			"pve.storage_allocation_journal_dir is set but no namespace is",
			"pve_storage_placement_namespace in the cpi-config entry",
			"docs/storage-journal-operations.md#enroll-a-new-namespace",
		}},
		{"namespace never enrolled", "never-enrolled", false, []string{
			`namespace "never-enrolled" is not enrolled in the allocation journal at `,
			"'cpi storage-journal audit-enrollment' and then 'cpi storage-journal initialize'",
			"docs/storage-journal-operations.md#enroll-a-new-namespace",
		}},
		{"journal directory never provisioned", "director", true, []string{
			"/allocations does not exist or this process cannot see it",
			"'cpi provision-journal' as docs/storage-journal-provisioning.md",
			"director.cpi_additional_volumes",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &vmMockQEMU{}
			deps := journalSetupVMDeps(q)
			directory := t.TempDir()
			if err := os.Chmod(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			if tc.unprovisioned {
				directory = filepath.Join(directory, "allocations")
			}
			deps.Config.StorageAllocationJournalDir = directory
			deps.Config.StoragePlacementNamespace = tc.namespace
			args := mkArgs("agent-journal-setup", testStemcellCID, map[string]any{"cores": 1, "memory": 512},
				defaultNetMap(), []string{}, map[string]any{})
			_, err := handlers.HandleCreateVM(deps).Handle(context.Background(), args, mkCtx("journal-setup"))
			assertCreateRefusal(t, err, q, tc.want)
		})
	}
}

// TestSetManagedCreateVMNamesTheMissingJournal binds the root disk to a storage
// set and sets neither half of the allocation journal. The existing-allocation
// lookup does not run, so the set-managed create reaches the journal's open
// with nothing else in front of it, and it refuses with the text that names
// both properties rather than the bare validation error.
func TestSetManagedCreateVMNamesTheMissingJournal(t *testing.T) {
	q := &vmMockQEMU{}
	deps := journalSetupVMDeps(q)
	deps.Config.RootStorageSet = "R"
	deps.Config.StorageSets = map[string]config.StorageSet{"R": {
		Names:    []string{storageName, "other"},
		Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1},
	}}
	args := mkArgs("agent-set-managed", testStemcellCID, map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, map[string]any{})
	_, err := handlers.HandleCreateVM(deps).Handle(context.Background(), args, mkCtx("set-managed"))
	assertCreateRefusal(t, err, q, []string{
		"set-enabled allocation needs the allocation journal, which needs pve.storage_placement_namespace, " +
			"pve.storage_allocation_journal_dir, and an enrolled namespace",
		"docs/storage-journal-provisioning.md",
	})
}
