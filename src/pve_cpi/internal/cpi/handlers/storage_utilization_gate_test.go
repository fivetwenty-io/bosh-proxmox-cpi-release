package handlers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"strconv"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	sdkclusterapi "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

// ---------------------------------------------------------------------------
// End-to-end pve.storage.max_utilization_pct gate tests, exercised through
// the public HandleCreateDisk / HandleResizeDisk / HandleSnapshotDisk
// entrypoints so the gate wiring inside each handler is itself under test
// (not only the shared checkMaxUtilizationGate/warnIfStorageAboveCeiling
// helpers already covered by storage_utilization_internal_test.go).
// ---------------------------------------------------------------------------

// suGateWarnMode is the shared "warn" literal for log level and
// storage.max_utilization_mode across this file's tests (see suWarnMode in
// storage_utilization_internal_test.go for the equivalent in package
// handlers; this is the handlers_test-side copy since Go does not share
// unexported identifiers across the internal/external test package split).
const suGateWarnMode = "warn"

// suGateTotalGiB is the fixed pool size (in GiB) every suGateNodesService
// fixture reports; only availGiB varies across call sites to model different
// utilization levels against this constant total.
const suGateTotalGiB = int64(100)

// suGateNodesService returns a nodes.Service whose ListStorage reports
// availGiB free out of the fixed suGateTotalGiB total, for the package-level
// storageName const ("local-lvm"), active and images-capable.
func suGateNodesService(t *testing.T, availGiB int64) nodes.Service {
	t.Helper()
	const gib = int64(1024 * 1024 * 1024)
	return &mockNodesService{
		listStorageFn: func(_ context.Context, _ string, params *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
			name := storageName
			if params != nil && params.Storage != nil {
				name = *params.Storage
			}
			raw, err := json.Marshal(map[string]any{
				"storage": name,
				"active":  1,
				"enabled": 1,
				"content": "images,rootdir",
				"avail":   availGiB * gib,
				"total":   suGateTotalGiB * gib,
			})
			if err != nil {
				t.Fatalf("marshal storage entry: %v", err)
			}
			resp := nodes.ListStorageResponse{json.RawMessage(raw)}
			return &resp, nil
		},
	}
}

// suGateLoggerDeps builds a Deps identical in shape to baseDepsForCreate, but
// with a Storage utilization gate, a wired nodesSvc, and a buffer-backed
// logger so Warn output can be asserted.
func suGateLoggerDeps(
	t *testing.T, storageSvc *mockStorageService, clusterVMIDs []int, nodesSvc nodes.Service, storageCfg *config.StorageConfig,
) (handlers.Deps, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger, err := log.NewLogger(suGateWarnMode, &buf)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	listFn := func(_ context.Context, _ *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
		resp := make(sdkclusterapi.ListResourcesResponse, 0, len(clusterVMIDs))
		for _, id := range clusterVMIDs {
			raw, _ := json.Marshal(struct {
				Vmid int64 `json:"vmid"`
			}{Vmid: int64(id)})
			resp = append(resp, raw)
		}
		return &resp, nil
	}
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:         testNode,
			DiskStorage:  storageName,
			VMDiskFormat: "qcow2",
			Storage:      storageCfg,
			// Opt out of the parked default; parker paths have dedicated tests.
			DetachedDiskStrategy: "free",
		},
		PVE: &mockPVEClient{
			storageSvc: storageSvc,
			clusterSvc: &mockClusterSvc{listResourcesFn: listFn},
			nodesSvc:   nodesSvc,
		},
		Logger: logger,
	}
	return deps, &buf
}

func TestHandleCreateDisk_UtilizationGate_EnforceBlocks(t *testing.T) {
	t.Parallel()
	createVolumeCalled := false
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, _ string, _ int, _ string, vmid int, _ string) (string, error) {
			createVolumeCalled = true
			return "", nil
		},
	}
	pct := 90
	nodesSvc := suGateNodesService(t, 15) // 85% used
	deps, _ := suGateLoggerDeps(t, storageSvc, []int{9000}, nodesSvc, &config.StorageConfig{MaxUtilizationPct: &pct})

	h := handlers.HandleCreateDisk(deps)
	// 10 GiB disk request: 85% + 10% -> 95% > 90% ceiling.
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(10240),
		marshal(map[string]string{}),
		json.RawMessage(`null`),
	}, jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected an error when the utilization ceiling is exceeded in enforce mode")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("expected a RETRIABLE error, got %T: %v", err, err)
	}
	if createVolumeCalled {
		t.Error("CreateVolume must not be called when the utilization gate blocks the request")
	}
}

func TestHandleCreateDisk_UtilizationGate_WarnProceeds(t *testing.T) {
	t.Parallel()
	createVolumeCalled := false
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, vmid int, _ string) (string, error) {
			createVolumeCalled = true
			return storage + ":vm-" + strconv.Itoa(vmid) + "-disk-0", nil
		},
	}
	pct := 90
	nodesSvc := suGateNodesService(t, 15) // 85% used, would breach 90% ceiling after +10GiB
	deps, buf := suGateLoggerDeps(t, storageSvc, []int{9000}, nodesSvc,
		&config.StorageConfig{MaxUtilizationPct: &pct, MaxUtilizationMode: suGateWarnMode})

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(10240),
		marshal(map[string]string{}),
		json.RawMessage(`null`),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("warn mode must not block create_disk, got: %v", err)
	}
	if !createVolumeCalled {
		t.Error("CreateVolume must be called in warn mode")
	}
	if !strings.Contains(buf.String(), "warn mode; proceeding") {
		t.Errorf("expected a warn-mode Warn to be logged, got %q", buf.String())
	}
}

func TestHandleCreateDisk_UtilizationGate_Disabled_NoImpact(t *testing.T) {
	t.Parallel()
	createVolumeCalled := false
	storageSvc := &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, _ string, vmid int, _ string) (string, error) {
			createVolumeCalled = true
			return storage + ":vm-" + strconv.Itoa(vmid) + "-disk-0", nil
		},
	}
	// A pool that would breach any reasonable ceiling, but the gate is unset
	// (nil Storage) — must have zero impact.
	nodesSvc := suGateNodesService(t, 1) // 99% used
	deps, buf := suGateLoggerDeps(t, storageSvc, []int{9000}, nodesSvc, nil)

	h := handlers.HandleCreateDisk(deps)
	_, err := h.Handle(context.Background(), []json.RawMessage{
		marshal(10240),
		marshal(map[string]string{}),
		json.RawMessage(`null`),
	}, jsonrpc.Context{})

	if err != nil {
		t.Fatalf("disabled gate must not block create_disk, got: %v", err)
	}
	if !createVolumeCalled {
		t.Error("CreateVolume must be called when the gate is disabled")
	}
	if strings.Contains(buf.String(), "utilization") {
		t.Errorf("disabled gate must log nothing utilization-related, got %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// resize_disk
// ---------------------------------------------------------------------------

// suResizeDeps builds Deps for resize_disk gate tests, mirroring resizeDeps
// (resize_disk_test.go) with an added Storage gate, wired nodesSvc, and a
// buffer-backed logger.
func suResizeDeps(
	t *testing.T, qemuSvc qemu.Service, clusterSvc sdkclusterapi.Service, nodesSvc nodes.Service, storageCfg *config.StorageConfig,
) (handlers.Deps, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger, err := log.NewLogger(suGateWarnMode, &buf)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:    testNode,
			Storage: storageCfg,
		},
		PVE: &mockPVEClient{
			qemuSvc:    qemuSvc,
			clusterSvc: clusterSvc,
			nodesSvc:   nodesSvc,
		},
		Logger: logger,
	}
	return deps, &buf
}

func TestHandleResizeDisk_UtilizationGate_EnforceBlocks(t *testing.T) {
	t.Parallel()
	resizeCalled := false
	qemuSvc := resizeQEMUWithDisk(diskSlot, diskCID+",size=10G", func(context.Context, string, int, string, int) (string, error) {
		resizeCalled = true
		return "", nil
	})
	pct := 90
	nodesSvc := suGateNodesService(t, 15) // 85% used
	deps, _ := suResizeDeps(t, qemuSvc, resizeClusterWith(100), nodesSvc, &config.StorageConfig{MaxUtilizationPct: &pct})

	h := handlers.HandleResizeDisk(deps)
	// Grow from 10 GiB to 15 GiB: a 5 GiB delta. 85% + 5% -> 90% == ceiling,
	// so bump the request to push strictly over: 10G -> 25G (15 GiB delta,
	// 85%+15%=100% > 90%).
	_, err := h.Handle(context.Background(), marshalArgs(mustEncodeDiskCID(t, diskCID, nil), 25600), jsonrpc.Context{})

	if err == nil {
		t.Fatal("expected an error when the utilization ceiling is exceeded in enforce mode")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("expected a RETRIABLE error, got %T: %v", err, err)
	}
	if resizeCalled {
		t.Error("ResizeDisk must not be called when the utilization gate blocks the request")
	}
}

func TestHandleResizeDisk_UtilizationGate_WarnProceeds(t *testing.T) {
	t.Parallel()
	resizeCalled := false
	qemuSvc := resizeQEMUWithDisk(diskSlot, diskCID+",size=10G", func(context.Context, string, int, string, int) (string, error) {
		resizeCalled = true
		return "", nil
	})
	pct := 90
	nodesSvc := suGateNodesService(t, 15) // 85% used
	deps, buf := suResizeDeps(t, qemuSvc, resizeClusterWith(100), nodesSvc,
		&config.StorageConfig{MaxUtilizationPct: &pct, MaxUtilizationMode: suGateWarnMode})

	h := handlers.HandleResizeDisk(deps)
	_, err := h.Handle(context.Background(), marshalArgs(mustEncodeDiskCID(t, diskCID, nil), 25600), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("warn mode must not block resize_disk, got: %v", err)
	}
	if !resizeCalled {
		t.Error("ResizeDisk must be called in warn mode")
	}
	if !strings.Contains(buf.String(), "warn mode; proceeding") {
		t.Errorf("expected a warn-mode Warn to be logged, got %q", buf.String())
	}
}

// ---------------------------------------------------------------------------
// snapshot_disk: always Warn-only, regardless of max_utilization_mode
// ---------------------------------------------------------------------------

func TestHandleSnapshotDisk_UtilizationGate_EnforceMode_StillOnlyWarnsAndProceeds(t *testing.T) {
	t.Parallel()
	const volid = "local-lvm:vm-9001-disk-0"
	snapshotCalled := false
	qemuSvc := &snapQEMUService{
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if vmid == 100 {
				return map[string]any{diskSlot: volid}, nil
			}
			return map[string]any{}, nil
		},
		snapshotFn: func(context.Context, string, int, string, map[string]any) (string, error) {
			snapshotCalled = true
			return "", nil
		},
	}
	pct := 80
	// Already at 85%, above the 80% ceiling — with mode "enforce", which
	// snapshot_disk must ignore entirely (it is always Warn-only).
	nodesSvc := suGateNodesService(t, 15)
	var buf bytes.Buffer
	logger, err := log.NewLogger(suGateWarnMode, &buf)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	deps := handlers.Deps{
		Config: &config.CPIConfig{
			Node:    testNode,
			Storage: &config.StorageConfig{MaxUtilizationPct: &pct, MaxUtilizationMode: "enforce"},
		},
		PVE: &mockPVEClient{
			qemuSvc: qemuSvc,
			clusterSvc: &snapClusterService{listFn: func(context.Context, *sdkclusterapi.ListResourcesParams) (*sdkclusterapi.ListResourcesResponse, error) {
				return clusterRespWith(100, testNode), nil
			}},
			nodesSvc: nodesSvc,
		},
		Logger: logger,
	}

	h := handlers.HandleSnapshotDisk(deps)
	_, err = h.Handle(context.Background(), marshalArgs(mustEncodeDiskCID(t, volid, nil), nil), jsonrpc.Context{})

	if err != nil {
		t.Fatalf("snapshot_disk must never be blocked by the utilization gate, got: %v", err)
	}
	if !snapshotCalled {
		t.Error("Snapshot must be called — the gate is warn-only and must not block")
	}
	if !strings.Contains(buf.String(), "already above the utilization ceiling") {
		t.Errorf("expected an above-ceiling Warn to be logged, got %q", buf.String())
	}
}
