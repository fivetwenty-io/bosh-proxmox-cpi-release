package pve_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// imageScanNodes adds the node storage listing to stubNodesService and counts
// how often each storage's content is read.
type imageScanNodes struct {
	stubNodesService
	listStorageFn func(ctx context.Context, node string, params *sdknodes.ListStorageParams) (*sdknodes.ListStorageResponse, error)

	mu        sync.Mutex
	listed    map[string]int
	lastParam *sdknodes.ListStorageParams
}

func (n *imageScanNodes) ListStorage(ctx context.Context, node string, params *sdknodes.ListStorageParams) (*sdknodes.ListStorageResponse, error) {
	n.mu.Lock()
	n.lastParam = params
	n.mu.Unlock()
	return n.listStorageFn(ctx, node, params)
}

func (n *imageScanNodes) ListStorageContent(ctx context.Context, node, storage string, params *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
	n.mu.Lock()
	if n.listed == nil {
		n.listed = map[string]int{}
	}
	n.listed[storage]++
	n.mu.Unlock()
	return n.stubNodesService.ListStorageContent(ctx, node, storage, params)
}

func nodeStorageRows(rows ...map[string]any) *sdknodes.ListStorageResponse {
	resp := make(sdknodes.ListStorageResponse, 0, len(rows))
	for _, row := range rows {
		raw, _ := json.Marshal(row)
		resp = append(resp, raw)
	}
	return &resp
}

func imagesRow(name string) map[string]any {
	return map[string]any{"storage": name, "content": "images,rootdir", "enabled": 1, "active": 1}
}

func newImageScanClient(nodesSvc *imageScanNodes) *mockClient {
	return &mockClient{
		tasksSvc: &mockTasksService{},
		clusterSvc: &stubClusterService{listResourcesFn: func(context.Context, *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return buildResources(), nil
		}},
		nodesSvc: nodesSvc,
	}
}

// TestNextVMID_NodeImageStorageScan_SkipsVMIDNamedOnAnyImagesStorage pins the
// allocator half of the fix. A VMID that names a volume on any images storage
// the node sees is skipped, not only one that names a volume on vm_storage.
func TestNextVMID_NodeImageStorageScan_SkipsVMIDNamedOnAnyImagesStorage(t *testing.T) {
	t.Parallel()
	nodesSvc := &imageScanNodes{
		listStorageFn: func(context.Context, string, *sdknodes.ListStorageParams) (*sdknodes.ListStorageResponse, error) {
			return nodeStorageRows(imagesRow("local-lvm"), imagesRow("data")), nil
		},
	}
	nodesSvc.listStorageContentFn = func(_ context.Context, _, storage string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
		if storage == "data" {
			return buildStorageContent("data:vm-9005-disk-0"), nil
		}
		return buildStorageContent(), nil
	}
	client := newImageScanClient(nodesSvc)

	// The random start offset would pick 9005 about half the time.
	for range 20 {
		got, err := pve.NextVMID(context.Background(), client,
			pve.WithRange(9005, 9006), pve.WithNodeImageStorageScan("pve1"))
		if err != nil {
			t.Fatalf("NextVMID: %v", err)
		}
		if got != 9006 {
			t.Fatalf("NextVMID = %d, want 9006: data:vm-9005-disk-0 names 9005", got)
		}
	}
	params := nodesSvc.lastParam
	if params == nil || params.Content == nil || *params.Content != "images" || params.Enabled == nil || !*params.Enabled {
		t.Errorf("the node storage list must ask for enabled images storages; got %+v", params)
	}
}

// TestNextVMID_NodeImageStorageScan_ReadsOwnerFromAnyVolumeName pins PVE's
// owner rule: any vm-<N>-* or base-<N>-* name, in flat or path form, belongs
// to VMID N, not only the vm-<N>-disk-<M> shape.
func TestNextVMID_NodeImageStorageScan_ReadsOwnerFromAnyVolumeName(t *testing.T) {
	t.Parallel()
	nodesSvc := &imageScanNodes{
		listStorageFn: func(context.Context, string, *sdknodes.ListStorageParams) (*sdknodes.ListStorageResponse, error) {
			return nodeStorageRows(imagesRow("data")), nil
		},
	}
	nodesSvc.listStorageContentFn = func(context.Context, string, string, *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
		resp := make(sdknodes.ListStorageContentResponse, 0, 5)
		for volid, content := range map[string]string{
			"data:vm-9005-cloudinit":               "images",
			"data:base-9006-disk-0":                "images",
			"data:9007/vm-9007-ephemeral-0.qcow2":  "images",
			"data:base-9006-disk-0/vm-9008-disk-0": "images",
			// An ISO has no owner in PVE, whatever its file is called.
			"data:iso/vm-9009-disk-0-notes.iso": "iso",
		} {
			raw, _ := json.Marshal(map[string]string{"volid": volid, "content": content})
			resp = append(resp, raw)
		}
		return &resp, nil
	}
	client := newImageScanClient(nodesSvc)

	for range 20 {
		got, err := pve.NextVMID(context.Background(), client,
			pve.WithRange(9005, 9009), pve.WithNodeImageStorageScan("pve1"))
		if err != nil {
			t.Fatalf("NextVMID: %v", err)
		}
		if got != 9009 {
			t.Fatalf("NextVMID = %d, want 9009", got)
		}
	}
}

// TestNextVMID_NodeImageStorageScan_FailsClosed pins that the allocation fails
// when either read the scan needs fails, rather than allocating blind.
func TestNextVMID_NodeImageStorageScan_FailsClosed(t *testing.T) {
	t.Parallel()
	cases := map[string]*imageScanNodes{
		"storage list error": {
			listStorageFn: func(context.Context, string, *sdknodes.ListStorageParams) (*sdknodes.ListStorageResponse, error) {
				return nil, errors.New("storage list unavailable")
			},
		},
		"storage row unreadable": {
			listStorageFn: func(context.Context, string, *sdknodes.ListStorageParams) (*sdknodes.ListStorageResponse, error) {
				resp := sdknodes.ListStorageResponse{json.RawMessage(`{"storage": 7}`)}
				return &resp, nil
			},
		},
		"storage list nil": {
			listStorageFn: func(context.Context, string, *sdknodes.ListStorageParams) (*sdknodes.ListStorageResponse, error) {
				return nil, nil
			},
		},
		"content list error": {
			stubNodesService: stubNodesService{
				listStorageContentFn: func(_ context.Context, _, storage string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
					if storage == "data" {
						return nil, errors.New("content list unavailable")
					}
					return buildStorageContent(), nil
				},
			},
			listStorageFn: func(context.Context, string, *sdknodes.ListStorageParams) (*sdknodes.ListStorageResponse, error) {
				return nodeStorageRows(imagesRow("local-lvm"), imagesRow("data")), nil
			},
		},
	}
	for name, nodesSvc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := pve.NextVMID(context.Background(), newImageScanClient(nodesSvc),
				pve.WithRange(9005, 9006), pve.WithNodeImageStorageScan("pve1"))
			if err == nil {
				t.Fatalf("NextVMID = %d with a failed listing; want an error", got)
			}
		})
	}
}

// TestNextVMID_NodeImageStorageScan_ListsEachStorageOnce pins that a storage
// named by WithStorageScan, WithExtraStorageScan, and the node listing is read
// once, and that rows the node marks disabled, inactive, or not for images
// are skipped.
func TestNextVMID_NodeImageStorageScan_ListsEachStorageOnce(t *testing.T) {
	t.Parallel()
	nodesSvc := &imageScanNodes{
		listStorageFn: func(context.Context, string, *sdknodes.ListStorageParams) (*sdknodes.ListStorageResponse, error) {
			return nodeStorageRows(
				imagesRow("local-lvm"),
				imagesRow("shared-nfs"),
				map[string]any{"storage": "isos", "content": "iso,vztmpl", "enabled": 1, "active": 1},
				map[string]any{"storage": "off", "content": "images", "enabled": 0, "active": 0},
				map[string]any{"storage": "absent-here", "content": "images", "enabled": 1, "active": 0},
			), nil
		},
	}
	client := newImageScanClient(nodesSvc)

	if _, err := pve.NextVMID(context.Background(), client,
		pve.WithRange(9005, 9006),
		pve.WithStorageScan("pve1", "local-lvm"),
		pve.WithExtraStorageScan("pve1", "shared-nfs"),
		pve.WithNodeImageStorageScan("pve1"),
	); err != nil {
		t.Fatalf("NextVMID: %v", err)
	}
	want := map[string]int{"local-lvm": 1, "shared-nfs": 1}
	nodesSvc.mu.Lock()
	defer nodesSvc.mu.Unlock()
	if len(nodesSvc.listed) != len(want) {
		t.Errorf("listed storages = %v, want %v", nodesSvc.listed, want)
	}
	for storage, n := range want {
		if nodesSvc.listed[storage] != n {
			t.Errorf("storage %q listed %d times, want %d (all: %v)", storage, nodesSvc.listed[storage], n, nodesSvc.listed)
		}
	}
}

// TestNextVMID_NodeImageStorageScan_EmptyNodeIsNoOp pins that the option is
// inert without a node, so callers that cannot name one keep their behavior.
func TestNextVMID_NodeImageStorageScan_EmptyNodeIsNoOp(t *testing.T) {
	t.Parallel()
	nodesSvc := &imageScanNodes{
		listStorageFn: func(context.Context, string, *sdknodes.ListStorageParams) (*sdknodes.ListStorageResponse, error) {
			t.Error("the node storage list must not be read without a node")
			return nil, errors.New("unexpected")
		},
	}
	if _, err := pve.NextVMID(context.Background(), newImageScanClient(nodesSvc),
		pve.WithRange(9005, 9006), pve.WithNodeImageStorageScan("")); err != nil {
		t.Fatalf("NextVMID: %v", err)
	}
}

// TestNextVMID_NodeImageStorageScan_InactiveSkipOnlyOnExplicitZero pins the
// skip for storages the node cannot list. A row the node marks active 0 or
// enabled 0 is skipped. A row with no active field is listed, so when its
// content cannot be read the allocation fails rather than skipping it.
func TestNextVMID_NodeImageStorageScan_InactiveSkipOnlyOnExplicitZero(t *testing.T) {
	t.Parallel()
	rows := func(context.Context, string, *sdknodes.ListStorageParams) (*sdknodes.ListStorageResponse, error) {
		return nodeStorageRows(
			map[string]any{"storage": "down", "content": "images", "enabled": 1, "active": 0},
			map[string]any{"storage": "off", "content": "images", "enabled": 0, "active": 1},
			map[string]any{"storage": "unmarked", "content": "images"},
		), nil
	}
	contentFailsFor := func(failing string) func(context.Context, string, string, *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
		return func(_ context.Context, _, storage string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
			if storage == failing {
				return nil, errors.New("storage is not online")
			}
			return buildStorageContent(), nil
		}
	}

	t.Run("explicit zero rows are skipped", func(t *testing.T) {
		t.Parallel()
		nodesSvc := &imageScanNodes{listStorageFn: rows}
		// A read of either skipped storage would fail the allocation.
		nodesSvc.listStorageContentFn = func(_ context.Context, _, storage string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
			if storage == "down" || storage == "off" {
				return nil, errors.New("storage is not online")
			}
			return buildStorageContent(), nil
		}
		if _, err := pve.NextVMID(context.Background(), newImageScanClient(nodesSvc),
			pve.WithRange(9005, 9006), pve.WithNodeImageStorageScan("pve1")); err != nil {
			t.Fatalf("NextVMID: %v", err)
		}
		nodesSvc.mu.Lock()
		defer nodesSvc.mu.Unlock()
		if nodesSvc.listed["down"] != 0 || nodesSvc.listed["off"] != 0 || nodesSvc.listed["unmarked"] != 1 {
			t.Errorf("listed = %v, want only unmarked, once", nodesSvc.listed)
		}
	})

	t.Run("a row with no active field is listed and fails closed", func(t *testing.T) {
		t.Parallel()
		nodesSvc := &imageScanNodes{listStorageFn: rows}
		nodesSvc.listStorageContentFn = contentFailsFor("unmarked")
		got, err := pve.NextVMID(context.Background(), newImageScanClient(nodesSvc),
			pve.WithRange(9005, 9006), pve.WithNodeImageStorageScan("pve1"))
		if err == nil {
			t.Fatalf("NextVMID = %d although the unmarked storage could not be listed", got)
		}
	})
}
