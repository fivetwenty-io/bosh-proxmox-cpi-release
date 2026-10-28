package handlers_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// An ephemeral pool on iscsi or iscsidirect storage cannot allocate disk
// images, so create_vm refuses it with a non-retriable error that names the
// pool and its type before it imports, clones, or configures a VM, and before
// it asks the pool for a volume.
func TestHandleCreateVM_EphemeralOnISCSIRefusedBeforeTheVMExists(t *testing.T) {
	t.Parallel()
	for _, typ := range []string{"iscsi", "iscsidirect"} {
		t.Run(typ, func(t *testing.T) {
			t.Parallel()
			var volumes, clones, configs atomic.Int32
			stor := &mockStorageService{
				createVolumeFn: func(context.Context, string, string, int, string, int, string) (string, error) {
					volumes.Add(1)
					return "", errors.New("unexpected volume")
				},
			}
			q := &vmMockQEMU{
				cloneFn: func(context.Context, string, int, map[string]any) (string, error) {
					clones.Add(1)
					return "", errors.New("unexpected clone")
				},
				configFn: func(context.Context, string, int) (map[string]any, error) {
					configs.Add(1)
					return map[string]any{}, nil
				},
			}
			deps := buildVMDepsWithStorage(q, &vmMockNodes{}, &vmMockCluster{}, &vmMockAgent{}, stor)
			deps.PVE.(*mockPVEClient).clusterStorageSvc = &multiClusterStorage{entries: []map[string]any{
				{"storage": storageName, "type": "lvmthin"},
				{"storage": "san-lun", "type": typ},
			}}
			args := mkArgs("agent-eph-"+typ, testStemcellCID,
				map[string]any{"cores": 1, "memory": 512, "ephemeral_disk_size_mb": 4096, "ephemeral_storage_pool": "san-lun"},
				defaultNetMap(), []string{}, map[string]any{})

			_, err := handlers.HandleCreateVM(deps).Handle(context.Background(), args, mkCtx("eph-"+typ))
			var cloud *cpierrors.Error
			if !errors.As(err, &cloud) || cloud.OkToRetry() {
				t.Fatalf("create_vm error = %v; want a non-retriable cloud error", err)
			}
			for _, want := range []string{`"san-lun"`, `type "` + typ + `"`, "cannot allocate disk images", "before the VM was created"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %s", err, want)
				}
			}
			q.mu.Lock()
			n := len(q.createCalls)
			q.mu.Unlock()
			if n != 0 || clones.Load() != 0 || configs.Load() != 0 || volumes.Load() != 0 {
				t.Fatalf("PVE saw %d creates, %d clones, %d config reads, and %d volume requests; want none",
					n, clones.Load(), configs.Load(), volumes.Load())
			}
		})
	}
}
