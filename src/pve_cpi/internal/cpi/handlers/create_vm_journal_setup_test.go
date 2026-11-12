package handlers_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// TestCreateVMNamesTheJournalSetupCause sets the allocation journal directory
// with no namespace, and then with a namespace that was never enrolled, and
// runs create_vm. Each refuses with a non-retriable CloudError that names the
// missing piece, and neither falls back to the legacy create, because the
// journal directory may hold records for VMs that path would allocate again.
func TestCreateVMNamesTheJournalSetupCause(t *testing.T) {
	for _, tc := range []struct {
		name, namespace string
		want            []string
	}{
		{"journal directory without a namespace", "", []string{
			"storage_allocation_journal_dir is set but storage_placement_namespace is not",
			"docs/storage-journal-provisioning.md",
		}},
		{"namespace never enrolled", "never-enrolled", []string{
			`storage_placement_namespace "never-enrolled" is not enrolled in the allocation journal at `,
			"'cpi storage-journal audit-enrollment' and then 'cpi storage-journal initialize'",
			"docs/storage-journal-operations.md#enroll-a-new-namespace",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, n, c, a := &vmMockQEMU{}, &vmMockNodes{}, &vmMockCluster{}, &vmMockAgent{}
			deps := buildVMDeps(q, n, c, a)
			directory := t.TempDir()
			if err := os.Chmod(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			deps.Config.StorageAllocationJournalDir = directory
			deps.Config.StoragePlacementNamespace = tc.namespace
			args := mkArgs("agent-journal-setup", testStemcellCID, map[string]any{"cores": 1, "memory": 512},
				defaultNetMap(), []string{}, map[string]any{})
			_, err := handlers.HandleCreateVM(deps).Handle(context.Background(), args, mkCtx("journal-setup"))
			if !cpierrors.IsType(err, cpierrors.TypeCloud) || cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
				t.Fatalf("create_vm error = %v, want a non-retriable CloudError", err)
			}
			for _, fragment := range tc.want {
				if !strings.Contains(err.Error(), fragment) {
					t.Fatalf("create_vm error %q does not contain %q", err.Error(), fragment)
				}
			}
			if len(q.createCalls) != 0 || len(q.startCalls) != 0 {
				t.Fatalf("create_vm fell back to the legacy create: %d creates, %d starts",
					len(q.createCalls), len(q.startCalls))
			}
		})
	}
}
