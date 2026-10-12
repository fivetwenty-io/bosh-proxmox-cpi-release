package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// sharedKeepFixture wires a heavy stemcell with one cache template holding
// only the caller's director ref, so delete_stemcell reaches the qcow2 step.
type sharedKeepFixture struct {
	client    *stemcellMockClient
	storage   *deleteStemcellMockStorage
	destroyed *[]string
}

// newSharedKeepFixture builds the client. shared selects how the "local"
// storage classifies: "shared", "local", or "unknown" (no cluster storage
// service, so shared-ness cannot be read). withTemplate false drives the
// no-template convergence branch instead.
func newSharedKeepFixture(shared string, withTemplate bool) sharedKeepFixture {
	var destroyed []string
	storageSvc := &deleteStemcellMockStorage{}
	nodesSvc := &stemcellMockNodes{
		deleteQemuFn: func(_ context.Context, node, vmid string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
			destroyed = append(destroyed, node+":"+vmid)
			resp := sdknodes.DeleteQemuResponse(`""`)
			return &resp, nil
		},
	}
	qemuSvc := &stemcellMockQEMU{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return directorRefsDescMap("dir-a"), nil
		},
	}
	clusterSvc := &stemcellMockCluster{}
	if withTemplate {
		clusterSvc.listResourcesFn = func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			items := sdkcluster.ListResourcesResponse{
				clusterTemplateItem(7001, vmNode, "stemcell-cache", cacheTemplateTags(testStemcellSHA8)),
			}
			return &items, nil
		}
	}
	client := &stemcellMockClient{
		qemuSvc: qemuSvc, nodesSvc: nodesSvc, tasksSvc: &stemcellMockTasks{},
		clusterSvc: clusterSvc, storageSvc: storageSvc,
	}
	switch shared {
	case "shared":
		client.clusterStorageSvc = lightStemcellClusterStorage("local", "nfs", true)
	case "local":
		client.clusterStorageSvc = lightStemcellClusterStorage("local", "dir", false)
	}
	return sharedKeepFixture{client: client, storage: storageSvc, destroyed: &destroyed}
}

func (f sharedKeepFixture) deleteHeavy(t *testing.T, overridden bool) {
	t.Helper()
	cfg := &config.CPIConfig{Node: vmNode, StemcellStorage: "local", VMStorage: "local", DiskStorage: "local"}
	deps := handlers.Deps{Config: cfg, PVE: f.client, Logger: log.NewNopLogger(), RequestOverridden: overridden}
	h := handlers.HandleDeleteStemcell(deps)
	args := []json.RawMessage{marshalArg(t, testHeavyCID())}
	if _, err := h.Handle(context.Background(), args, jsonrpc.Context{DirectorUUID: "dir-a"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDeleteStemcell_Heavy_SharedStorage_CpiConfigRequest_KeepsFile(t *testing.T) {
	t.Parallel()
	f := newSharedKeepFixture("shared", true)
	f.deleteHeavy(t, true)
	if len(*f.destroyed) != 1 {
		t.Errorf("the cache template must still be destroyed, destroyed = %v", *f.destroyed)
	}
	if f.storage.deleteVolumeIfExistsCalls != 0 {
		t.Errorf("a cpi_config request on shared storage must keep the heavy qcow2, got %d delete(s): %+v",
			f.storage.deleteVolumeIfExistsCalls, f.storage.calls)
	}
}

func TestDeleteStemcell_Heavy_SharedStorage_NoTemplate_CpiConfigRequest_KeepsFile(t *testing.T) {
	t.Parallel()
	f := newSharedKeepFixture("shared", false)
	f.deleteHeavy(t, true)
	if f.storage.deleteVolumeIfExistsCalls != 0 {
		t.Errorf("the no-template branch must keep the heavy qcow2 for a cpi_config request on shared storage, got %+v", f.storage.calls)
	}
}

func TestDeleteStemcell_Heavy_SharedStorage_NoCpiConfig_StillDeletesFile(t *testing.T) {
	t.Parallel()
	for _, withTemplate := range []bool{true, false} {
		t.Run(fmt.Sprintf("template=%v", withTemplate), func(t *testing.T) {
			t.Parallel()
			f := newSharedKeepFixture("shared", withTemplate)
			f.deleteHeavy(t, false)
			if f.storage.deleteVolumeIfExistsCalls != 1 {
				t.Errorf("a single-cluster request must keep today's delete, got %d delete(s)", f.storage.deleteVolumeIfExistsCalls)
			}
		})
	}
}

func TestDeleteStemcell_Heavy_LocalOrUnknownStorage_CpiConfigRequest_StillDeletesFile(t *testing.T) {
	t.Parallel()
	for _, shared := range []string{"local", "unknown"} {
		for _, withTemplate := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/template=%v", shared, withTemplate), func(t *testing.T) {
				t.Parallel()
				f := newSharedKeepFixture(shared, withTemplate)
				f.deleteHeavy(t, true)
				if f.storage.deleteVolumeIfExistsCalls < 1 {
					t.Errorf("storage that is not positively shared must keep today's delete, got none")
				}
				if f.storage.calls[0].volume != testVolumePath() {
					t.Errorf("unexpected volume %q", f.storage.calls[0].volume)
				}
			})
		}
	}
}

// TestDeleteStemcell_Heavy_ThroughRealOverride drives the whole path, from a
// request context carrying pve_host to the delete step, so the detection is
// proven to follow WithRequestOverrides and not a hand-set field.
func TestDeleteStemcell_Heavy_ThroughRealOverride(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                 string
		extra                map[string]any
		wantOnOverrideKeep   bool // override routed: template destroyed there, file kept
		wantJobClientDeletes int
	}{
		{"pve_host override keeps file", map[string]any{"pve_host": "az1.example"}, true, 0},
		{"no context keys deletes file", nil, false, 1},
		{"non-pve context keys delete file", map[string]any{"unrelated": "x"}, false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newSharedKeepFixture("shared", true)
			base := overrideTestBaseConfig()
			base.Node = vmNode
			base.StemcellStorage = "local"
			jobClient := &stemcellMockClient{
				qemuSvc: &stemcellMockQEMU{}, nodesSvc: &stemcellMockNodes{}, tasksSvc: &stemcellMockTasks{},
				clusterSvc: &stemcellMockCluster{}, storageSvc: &deleteStemcellMockStorage{},
			}
			deps := handlers.Deps{
				Config: base, PVE: jobClient, Logger: log.NewNopLogger(),
				Overrides: &handlers.RequestOverrideRuntime{
					ClientFactory: func(_ *config.CPIConfig, _ *log.Logger) (pve.Client, error) { return f.client, nil },
					Logger:        log.NewNopLogger(),
				},
			}
			h := handlers.HandleDeleteStemcell(deps)
			args := []json.RawMessage{marshalArg(t, testHeavyCID())}
			if _, err := h.Handle(context.Background(), args, jsonrpc.Context{DirectorUUID: "dir-a", Extra: tc.extra}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			jobStorage := jobClient.storageSvc.(*deleteStemcellMockStorage)
			if f.storage.deleteVolumeIfExistsCalls != 0 {
				t.Errorf("override cluster saw %d qcow2 delete(s), want 0", f.storage.deleteVolumeIfExistsCalls)
			}
			wantDestroyed := 0
			if tc.wantOnOverrideKeep {
				wantDestroyed = 1
			}
			if len(*f.destroyed) != wantDestroyed {
				t.Errorf("override cluster template destroys = %v, want %d (proves where the call routed)", *f.destroyed, wantDestroyed)
			}
			if jobStorage.deleteVolumeIfExistsCalls != tc.wantJobClientDeletes {
				t.Errorf("job client qcow2 deletes = %d, want %d", jobStorage.deleteVolumeIfExistsCalls, tc.wantJobClientDeletes)
			}
		})
	}
}

func TestWithRequestOverrides_RecordsWhetherAnOverrideApplied(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		extra map[string]any
		want  bool
	}{
		{"pve_host applied", map[string]any{"pve_host": "az1.example"}, true},
		{"no extra", nil, false},
		{"non-pve extra", map[string]any{"unrelated": "x"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tracker := &fakeTransportClientFactory{}
			deps := handlers.Deps{
				Config: overrideTestBaseConfig(), PVE: &mockPVEClient{}, Logger: log.NewNopLogger(),
				Overrides: &handlers.RequestOverrideRuntime{ClientFactory: tracker.Factory, Logger: log.NewNopLogger()},
			}
			got, err := deps.WithRequestOverrides(context.Background(), jsonrpc.Context{Extra: tc.extra})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.RequestOverridden != tc.want {
				t.Errorf("RequestOverridden = %v, want %v", got.RequestOverridden, tc.want)
			}
		})
	}
}
