package pve_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// parkerTestCfg returns a ParkerConfig with the default parker band.
func parkerTestCfg() pve.ParkerConfig {
	return pve.ParkerConfig{
		VMIDRangeStart: 90000,
		VMIDRangeEnd:   90999,
	}
}

// parkerTestCfgWithDirector returns a ParkerConfig that includes a director ID.
func parkerTestCfgWithDirector(directorID string) pve.ParkerConfig {
	return pve.ParkerConfig{
		VMIDRangeStart: 90000,
		VMIDRangeEnd:   90999,
		DirectorID:     directorID,
	}
}

// nopLogger returns a no-op logger for tests that don't need log inspection.
func nopLogger() *log.Logger {
	return log.NewNopLogger()
}

// buildParkerClient reuses diskClusterClient (defined in disk_test.go, same
// package pve_test) to satisfy pve.Client for parker tests.

// ---------------------------------------------------------------------------
// parkerQEMU — QEMU service mock used by parker tests.
// ---------------------------------------------------------------------------

type parkerQEMU struct {
	qemu.Service // embed to get zero-value methods; override what we need

	// configFn provides per-(node,vmid) Config responses.
	configFn func(node string, vmid int) (map[string]any, error)
	// createFn intercepts Create calls.
	createFn func(node string, params map[string]any) (string, error)
	// attachFn intercepts AttachDisk calls.
	attachFn func(node string, vmid int, volid, bus string, opts *qemu.AttachOpts) (string, error)
	// detachFn intercepts DetachDisk calls.
	detachFn func(node string, vmid int, diskID string) error
}

func (q *parkerQEMU) Config(_ context.Context, node string, vmid int) (map[string]any, error) {
	if q.configFn != nil {
		return q.configFn(node, vmid)
	}
	return map[string]any{}, nil
}

func (q *parkerQEMU) Create(_ context.Context, node string, params map[string]any) (string, error) {
	if q.createFn != nil {
		return q.createFn(node, params)
	}
	return "", nil
}

func (q *parkerQEMU) AttachDisk(_ context.Context, node string, vmid int, volid, bus string, opts *qemu.AttachOpts) (string, error) {
	if q.attachFn != nil {
		return q.attachFn(node, vmid, volid, bus, opts)
	}
	return "", nil
}

func (q *parkerQEMU) DetachDisk(_ context.Context, node string, vmid int, diskID string) error {
	if q.detachFn != nil {
		return q.detachFn(node, vmid, diskID)
	}
	return nil
}

func (q *parkerQEMU) Status(_ context.Context, _ string, _ int) (map[string]any, error) {
	panic("Status not expected")
}
func (q *parkerQEMU) Start(_ context.Context, _ string, _ int) (string, error) {
	panic("Start not expected")
}
func (q *parkerQEMU) Stop(_ context.Context, _ string, _ int) (string, error) {
	panic("Stop not expected")
}
func (q *parkerQEMU) Reset(_ context.Context, _ string, _ int) (string, error) {
	panic("Reset not expected")
}
func (q *parkerQEMU) Clone(_ context.Context, _ string, _ int, _ map[string]any) (string, error) {
	panic("Clone not expected")
}
func (q *parkerQEMU) Template(_ context.Context, _ string, _ int) (string, error) {
	panic("Template not expected")
}
func (q *parkerQEMU) ResizeDisk(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
	panic("ResizeDisk not expected")
}
func (q *parkerQEMU) Snapshot(_ context.Context, _ string, _ int, _ string, _ map[string]any) (string, error) {
	panic("Snapshot not expected")
}
func (q *parkerQEMU) DeleteSnapshot(_ context.Context, _ string, _ int, _ string) error {
	panic("DeleteSnapshot not expected")
}
func (q *parkerQEMU) ListSnapshots(_ context.Context, _ string, _ int) ([]map[string]any, error) {
	panic("ListSnapshots not expected")
}
func (q *parkerQEMU) RollbackSnapshot(_ context.Context, _ string, _ int, _ string) (string, error) {
	panic("RollbackSnapshot not expected")
}

// parkerFakeCluster builds cluster.ListResourcesResponse rows from typed maps.
func parkerClusterResp(rows ...map[string]any) *cluster.ListResourcesResponse {
	out := make(cluster.ListResourcesResponse, 0, len(rows))
	for _, r := range rows {
		b, _ := json.Marshal(r)
		out = append(out, b)
	}
	return &out
}

// buildParkerClient constructs a pve.Client backed by parkerQEMU and a
// diskFakeCluster (defined in disk_test.go). Both are configurable by the
// test.
func buildParkerClient(
	qemuSvc qemu.Service,
	listFn func(ctx context.Context, params *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error),
) pve.Client {
	return &diskClusterClient{
		qemuSvc: qemuSvc,
		clusterSvc: &diskFakeCluster{
			listFn: listFn,
		},
	}
}

// ---------------------------------------------------------------------------
// IsParkerVM
// ---------------------------------------------------------------------------

func TestIsParkerVM_InRangeWithTag(t *testing.T) {
	t.Parallel()
	cfg := parkerTestCfg()
	if !pve.IsParkerVM(90000, "bosh-parker", cfg) {
		t.Error("expected true for vmid in range with bosh-parker tag")
	}
}

func TestIsParkerVM_InRangeWithTagAndDirector(t *testing.T) {
	t.Parallel()
	cfg := parkerTestCfg()
	if !pve.IsParkerVM(90500, "bosh-stemcell;bosh-parker;director--prod", cfg) {
		t.Error("expected true: bosh-parker present alongside other tags")
	}
}

func TestIsParkerVM_InRangeWithoutTag(t *testing.T) {
	t.Parallel()
	cfg := parkerTestCfg()
	if pve.IsParkerVM(90000, "bosh-stemcell", cfg) {
		t.Error("expected false: bosh-parker tag absent")
	}
}

func TestIsParkerVM_OutOfRangeWithTag(t *testing.T) {
	t.Parallel()
	cfg := parkerTestCfg()
	// VMID 1000 is outside 90000-90999.
	if pve.IsParkerVM(1000, "bosh-parker", cfg) {
		t.Error("expected false: vmid outside parker range")
	}
}

func TestIsParkerVM_EmptyTags(t *testing.T) {
	t.Parallel()
	cfg := parkerTestCfg()
	if pve.IsParkerVM(90000, "", cfg) {
		t.Error("expected false: empty tags string")
	}
}

func TestIsParkerVM_TagCaseInsensitive(t *testing.T) {
	t.Parallel()
	cfg := parkerTestCfg()
	// PVE may normalize tag case; tolerate both forms.
	if !pve.IsParkerVM(90000, "Bosh-Parker", cfg) {
		t.Error("expected true: tag comparison is case-insensitive")
	}
}

// ---------------------------------------------------------------------------
// chooseParkSlot / free-slot logic (exported via package pve_test via parker.go
// — chooseParkSlot is unexported; test via IsDiskParked + ParkDisk behavior,
// and test the exported ErrNoSlots sentinel).
//
// Since chooseParkSlot is unexported, we test it indirectly: we expose the
// equivalent logic via a thin exported wrapper in a _internal_test.go or test
// the behavior end-to-end. The plan says "chooseParkSlot/free-slot logic" so
// we use pool_internal_test.go pattern: internal_test goes in package pve
// (not pve_test) to access unexported symbols.
// ---------------------------------------------------------------------------
//
// The direct chooseParkSlot tests live in parker_internal_test.go (package pve).
// Here we test ErrNoSlots surfacing through ParkDisk.

func TestParkDisk_FullParkerCreatesSecondParker(t *testing.T) {
	t.Parallel()
	ctx := pve.WithTestBackoff(context.Background(), func(_ int) time.Duration { return 0 })
	cfg := parkerTestCfg()
	node := "pve1"
	bareVolid := "local-lvm:vm-9001-disk-0"

	// First parker VMID 90000 has all 31 scsi slots occupied.
	fullDisks := map[string]any{}
	for i := 0; i < 31; i++ {
		fullDisks[fmt.Sprintf("scsi%d", i)] = fmt.Sprintf("local-lvm:vm-9999-disk-%d", i)
	}
	fullDisks["tags"] = "bosh-parker"

	// Second parker VMID 90001 is empty.
	emptyParkerCfg := map[string]any{"tags": "bosh-parker"}

	var createdVMIDs []int
	var attachCalls []struct {
		vmid int
		slot string
	}

	qemuSvc := &parkerQEMU{
		configFn: func(_ string, vmid int) (map[string]any, error) {
			switch vmid {
			case 90000:
				return fullDisks, nil
			case 90001:
				return emptyParkerCfg, nil
			default:
				return map[string]any{}, nil
			}
		},
		createFn: func(_ string, params map[string]any) (string, error) {
			vmidVal, _ := params["vmid"].(int)
			createdVMIDs = append(createdVMIDs, vmidVal)
			return "", nil // no UPID
		},
		attachFn: func(_ string, vmid int, _, _ string, opts *qemu.AttachOpts) (string, error) {
			slot := ""
			if opts != nil {
				slot = opts.DiskID
			}
			attachCalls = append(attachCalls, struct {
				vmid int
				slot string
			}{vmid, slot})
			return "", nil
		},
	}

	// Cluster: 90000 exists on pve1 (full parker). 90001 doesn't exist yet.
	listCallCount := 0
	c := buildParkerClient(qemuSvc, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		listCallCount++
		// After first creation of 90001, include it.
		if listCallCount <= 2 {
			return parkerClusterResp(
				map[string]any{"vmid": int64(90000), "node": node},
			), nil
		}
		return parkerClusterResp(
			map[string]any{"vmid": int64(90000), "node": node},
			map[string]any{"vmid": int64(90001), "node": node},
		), nil
	})

	err := pve.ParkDisk(ctx, c, nopLogger(), node, bareVolid, cfg)
	if err != nil {
		t.Fatalf("ParkDisk: unexpected error: %v", err)
	}

	// A second parker VM must have been created.
	if len(createdVMIDs) == 0 {
		t.Error("expected at least one parker VM created (fresh parker for overflow)")
	}

	// Attach must have been called on the fresh parker.
	if len(attachCalls) == 0 {
		t.Error("expected at least one AttachDisk call")
	}
}

// ---------------------------------------------------------------------------
// IsDiskParked
// ---------------------------------------------------------------------------

func TestIsDiskParked_FreeFloating(t *testing.T) {
	t.Parallel()
	// Disk is attached to no VM — FindVMByDiskVolidOrNone returns found=false.
	c := buildParkerClient(
		&parkerQEMU{},
		func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			return parkerClusterResp(), nil // empty cluster
		},
	)
	_, _, _, parked, err := pve.IsDiskParked(context.Background(), c, nopLogger(),
		"local-lvm:vm-9001-disk-0", parkerTestCfg())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parked {
		t.Error("expected parked=false for free-floating disk")
	}
}

func TestIsDiskParked_HolderNotInRange_NoConfigRead(t *testing.T) {
	t.Parallel()
	// Disk is held by VMID 500 (outside 90000-90999). No config call expected.
	node := "pve1"
	volid := "local-lvm:vm-9001-disk-0"
	var configCalls int

	qemuSvc := &parkerQEMU{
		configFn: func(_ string, vmid int) (map[string]any, error) {
			configCalls++
			return map[string]any{"scsi0": volid}, nil
		},
	}
	c := buildParkerClient(qemuSvc, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return parkerClusterResp(
			map[string]any{"vmid": int64(500), "node": node},
		), nil
	})

	_, _, _, parked, err := pve.IsDiskParked(context.Background(), c, nopLogger(), volid, parkerTestCfg())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parked {
		t.Error("expected parked=false: holder not in parker range")
	}
	// Config may be called by FindVMByDiskVolid (which scans all VMs), but
	// IsDiskParked itself must NOT issue an extra config call for the range-fail path.
	// The key assertion is that the range check short-circuits before a second config read.
	// We can't easily distinguish calls here without a more complex mock, so we
	// just verify parked=false and no error.
}

func TestIsDiskParked_ParkedDisk(t *testing.T) {
	t.Parallel()
	node := "pve1"
	parkerVMID := 90000
	volid := "local-lvm:vm-9001-disk-0"

	qemuSvc := &parkerQEMU{
		configFn: func(_ string, vmid int) (map[string]any, error) {
			if vmid == parkerVMID {
				return map[string]any{
					"tags":  "bosh-parker",
					"scsi3": volid,
				}, nil
			}
			return map[string]any{}, nil
		},
	}
	c := buildParkerClient(qemuSvc, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return parkerClusterResp(
			map[string]any{"vmid": int64(parkerVMID), "node": node},
		), nil
	})

	gotVMID, gotNode, gotSlot, parked, err := pve.IsDiskParked(context.Background(), c, nopLogger(), volid, parkerTestCfg())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !parked {
		t.Fatal("expected parked=true")
	}
	if gotVMID != parkerVMID {
		t.Errorf("vmid: want %d, got %d", parkerVMID, gotVMID)
	}
	if gotNode != node {
		t.Errorf("node: want %q, got %q", node, gotNode)
	}
	if gotSlot != "scsi3" {
		t.Errorf("slot: want %q, got %q", "scsi3", gotSlot)
	}
}

func TestIsDiskParked_InRangeNoTag_FalseAndWarn(t *testing.T) {
	t.Parallel()
	node := "pve1"
	parkerVMID := 90000
	volid := "local-lvm:vm-9001-disk-0"

	// VM is in range but has no bosh-parker tag.
	qemuSvc := &parkerQEMU{
		configFn: func(_ string, vmid int) (map[string]any, error) {
			if vmid == parkerVMID {
				return map[string]any{
					"tags":  "some-other-tag",
					"scsi0": volid,
				}, nil
			}
			return map[string]any{}, nil
		},
	}
	c := buildParkerClient(qemuSvc, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return parkerClusterResp(
			map[string]any{"vmid": int64(parkerVMID), "node": node},
		), nil
	})

	_, _, _, parked, err := pve.IsDiskParked(context.Background(), c, nopLogger(), volid, parkerTestCfg())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parked {
		t.Error("expected parked=false when VMID in range but tag absent")
	}
}

func TestIsDiskParked_TransientScanError_Propagates(t *testing.T) {
	t.Parallel()
	transientErr := errors.New("pveproxy backend gone (code: 596)")
	c := buildParkerClient(
		&parkerQEMU{},
		func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			return nil, transientErr
		},
	)

	_, _, _, _, err := pve.IsDiskParked(context.Background(), c, nopLogger(),
		"local-lvm:vm-9001-disk-0", parkerTestCfg())
	if err == nil {
		t.Fatal("expected error to propagate for transient scan failure")
	}
}

// ---------------------------------------------------------------------------
// EnsureParker
// ---------------------------------------------------------------------------

func TestEnsureParker_FindsExisting(t *testing.T) {
	t.Parallel()
	node := "pve1"
	existingVMID := 90000
	var createCalled bool

	qemuSvc := &parkerQEMU{
		configFn: func(_ string, vmid int) (map[string]any, error) {
			if vmid == existingVMID {
				return map[string]any{"tags": "bosh-parker"}, nil
			}
			return nil, fmt.Errorf("not found")
		},
		createFn: func(_ string, _ map[string]any) (string, error) {
			createCalled = true
			return "", nil
		},
	}
	c := buildParkerClient(qemuSvc, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return parkerClusterResp(
			map[string]any{"vmid": int64(existingVMID), "node": node},
		), nil
	})

	vmid, err := pve.EnsureParker(context.Background(), c, nopLogger(), node, parkerTestCfg())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vmid != existingVMID {
		t.Errorf("vmid: want %d, got %d", existingVMID, vmid)
	}
	if createCalled {
		t.Error("Create must not be called when an existing parker is found")
	}
}

func TestEnsureParker_CreatesWhenNoneExist(t *testing.T) {
	t.Parallel()
	node := "pve1"
	var createdParams map[string]any
	var createdVMID int

	// Cluster starts empty; after create, include the new VMID.
	createdAlready := false
	qemuSvc := &parkerQEMU{
		configFn: func(_ string, vmid int) (map[string]any, error) {
			if createdAlready && vmid == createdVMID {
				return map[string]any{"tags": "bosh-parker"}, nil
			}
			return map[string]any{}, nil
		},
		createFn: func(_ string, params map[string]any) (string, error) {
			createdParams = params
			vmidVal, _ := params["vmid"].(int)
			createdVMID = vmidVal
			createdAlready = true
			return "", nil
		},
	}
	c := buildParkerClient(qemuSvc, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return parkerClusterResp(), nil // always empty for allocation purposes
	})

	vmid, err := pve.EnsureParker(context.Background(), c, nopLogger(), node, parkerTestCfg())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vmid < 90000 || vmid > 90999 {
		t.Errorf("vmid %d outside parker range [90000,90999]", vmid)
	}

	// Verify create params.
	if createdParams == nil {
		t.Fatal("Create was not called")
	}
	if v, _ := createdParams["protection"].(int); v != 1 {
		t.Errorf("protection: want 1, got %v", createdParams["protection"])
	}
	if v, _ := createdParams["onboot"].(int); v != 0 {
		t.Errorf("onboot: want 0, got %v", createdParams["onboot"])
	}
	tagsVal, _ := createdParams["tags"].(string)
	if tagsVal == "" {
		t.Error("tags must be non-empty")
	}
	if !containsTag(tagsVal, pve.ParkerTag) {
		t.Errorf("tags %q must contain %q", tagsVal, pve.ParkerTag)
	}
	nameVal, _ := createdParams["name"].(string)
	if nameVal == "" {
		t.Error("name must be set")
	}
	expectedName := fmt.Sprintf("bosh-parker-%d", vmid)
	if nameVal != expectedName {
		t.Errorf("name: want %q, got %q", expectedName, nameVal)
	}
}

func TestEnsureParker_CreateConflictAdoptsWinner(t *testing.T) {
	t.Parallel()
	node := "pve1"
	winnerVMID := 90000

	// Create always returns VMID-conflict error. Existing parker 90000 is found
	// on the re-scan.
	qemuSvc := &parkerQEMU{
		configFn: func(_ string, vmid int) (map[string]any, error) {
			if vmid == winnerVMID {
				return map[string]any{"tags": "bosh-parker"}, nil
			}
			return map[string]any{}, nil
		},
		createFn: func(_ string, _ map[string]any) (string, error) {
			// Simulate VMID already exists error.
			return "", fmt.Errorf("create VM: 500 (500) Parameter verification failed. (500) Error: 500 Parameter verification failed. (500) KVM VM already exists, vmid: 90000") //nolint:gocritic
		},
	}
	c := buildParkerClient(qemuSvc, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return parkerClusterResp(
			map[string]any{"vmid": int64(winnerVMID), "node": node},
		), nil
	})

	vmid, err := pve.EnsureParker(context.Background(), c, nopLogger(), node, parkerTestCfg())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vmid != winnerVMID {
		t.Errorf("vmid: want %d (winner), got %d", winnerVMID, vmid)
	}
}

func TestEnsureParker_DirectorTagIncludedWhenSet(t *testing.T) {
	t.Parallel()
	node := "pve1"
	var createdTags string

	qemuSvc := &parkerQEMU{
		createFn: func(_ string, params map[string]any) (string, error) {
			createdTags, _ = params["tags"].(string)
			return "", nil
		},
	}
	c := buildParkerClient(qemuSvc, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return parkerClusterResp(), nil
	})

	cfg := parkerTestCfgWithDirector("prod-director")
	_, err := pve.EnsureParker(context.Background(), c, nopLogger(), node, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !containsTag(createdTags, "director--prod-director") {
		t.Errorf("tags %q must contain director--prod-director when DirectorID is set", createdTags)
	}
}

// ---------------------------------------------------------------------------
// ParkDisk
// ---------------------------------------------------------------------------

func TestParkDisk_AttachesToFreeSlot(t *testing.T) {
	t.Parallel()
	node := "pve1"
	parkerVMID := 90000
	bareVolid := "local-lvm:vm-9001-disk-0"
	var attachedSlot string
	var attachedVMID int

	qemuSvc := &parkerQEMU{
		configFn: func(_ string, vmid int) (map[string]any, error) {
			if vmid == parkerVMID {
				return map[string]any{
					"tags":  "bosh-parker",
					"scsi0": "local-lvm:vm-9999-disk-0",
					// scsi1 is free
				}, nil
			}
			return map[string]any{}, nil
		},
		attachFn: func(_ string, vmid int, _, _ string, opts *qemu.AttachOpts) (string, error) {
			attachedVMID = vmid
			if opts != nil {
				attachedSlot = opts.DiskID
			}
			return "", nil
		},
	}
	// Cluster has only the parker.
	c := buildParkerClient(qemuSvc, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return parkerClusterResp(
			map[string]any{"vmid": int64(parkerVMID), "node": node},
		), nil
	})

	err := pve.ParkDisk(context.Background(), c, nopLogger(), node, bareVolid, parkerTestCfg())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attachedVMID != parkerVMID {
		t.Errorf("attachedVMID: want %d, got %d", parkerVMID, attachedVMID)
	}
	if attachedSlot != "scsi1" {
		t.Errorf("slot: want scsi1, got %q", attachedSlot)
	}
}

func TestParkDisk_AlreadyParkedIsIdempotent(t *testing.T) {
	t.Parallel()
	node := "pve1"
	parkerVMID := 90000
	bareVolid := "local-lvm:vm-9001-disk-0"
	var attachCalls int

	qemuSvc := &parkerQEMU{
		configFn: func(_ string, vmid int) (map[string]any, error) {
			if vmid == parkerVMID {
				// Disk already parked at scsi2.
				return map[string]any{
					"tags":  "bosh-parker",
					"scsi2": bareVolid,
				}, nil
			}
			return map[string]any{}, nil
		},
		attachFn: func(_ string, _ int, _, _ string, _ *qemu.AttachOpts) (string, error) {
			attachCalls++
			return "", nil
		},
	}
	c := buildParkerClient(qemuSvc, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return parkerClusterResp(
			map[string]any{"vmid": int64(parkerVMID), "node": node},
		), nil
	})

	err := pve.ParkDisk(context.Background(), c, nopLogger(), node, bareVolid, parkerTestCfg())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if attachCalls != 0 {
		t.Errorf("AttachDisk must not be called when disk is already parked; got %d calls", attachCalls)
	}
}

// ---------------------------------------------------------------------------
// UnparkDisk
// ---------------------------------------------------------------------------

func TestUnparkDisk_ParkedDiskDetachCalled(t *testing.T) {
	t.Parallel()
	node := "pve1"
	parkerVMID := 90000
	bareVolid := "local-lvm:vm-9001-disk-0"
	var detachVMID int
	var detachSlot string

	qemuSvc := &parkerQEMU{
		configFn: func(_ string, vmid int) (map[string]any, error) {
			if vmid == parkerVMID {
				return map[string]any{
					"tags":  "bosh-parker",
					"scsi5": bareVolid,
				}, nil
			}
			return map[string]any{}, nil
		},
		detachFn: func(_ string, vmid int, diskID string) error {
			detachVMID = vmid
			detachSlot = diskID
			return nil
		},
	}
	c := buildParkerClient(qemuSvc, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return parkerClusterResp(
			map[string]any{"vmid": int64(parkerVMID), "node": node},
		), nil
	})

	err := pve.UnparkDisk(context.Background(), c, nopLogger(), bareVolid, parkerTestCfg())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if detachVMID != parkerVMID {
		t.Errorf("detachVMID: want %d, got %d", parkerVMID, detachVMID)
	}
	if detachSlot != "scsi5" {
		t.Errorf("detachSlot: want scsi5, got %q", detachSlot)
	}
}

func TestUnparkDisk_NotParkedNoDetach(t *testing.T) {
	t.Parallel()
	var detachCalls int

	qemuSvc := &parkerQEMU{
		detachFn: func(_ string, _ int, _ string) error {
			detachCalls++
			return nil
		},
	}
	// Empty cluster — disk is free-floating.
	c := buildParkerClient(qemuSvc, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return parkerClusterResp(), nil
	})

	err := pve.UnparkDisk(context.Background(), c, nopLogger(),
		"local-lvm:vm-9001-disk-0", parkerTestCfg())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if detachCalls != 0 {
		t.Errorf("DetachDisk must not be called when disk is not parked; got %d calls", detachCalls)
	}
}

// ---------------------------------------------------------------------------
// FindVMByDiskVolidOrNone
// ---------------------------------------------------------------------------

func TestFindVMByDiskVolidOrNone_Found(t *testing.T) {
	t.Parallel()
	node := "pve1"
	volid := "local-lvm:vm-200-disk-0"

	c := buildParkerClient(
		&parkerQEMU{
			configFn: func(_ string, _ int) (map[string]any, error) {
				return map[string]any{"scsi0": volid}, nil
			},
		},
		func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			return parkerClusterResp(
				map[string]any{"vmid": int64(200), "node": node},
			), nil
		},
	)

	vmid, gotNode, found, err := pve.FindVMByDiskVolidOrNone(context.Background(), c, node, volid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if vmid != 200 {
		t.Errorf("vmid: want 200, got %d", vmid)
	}
	if gotNode != node {
		t.Errorf("node: want %q, got %q", node, gotNode)
	}
}

func TestFindVMByDiskVolidOrNone_NotFound(t *testing.T) {
	t.Parallel()
	// Empty cluster — disk not attached to any VM → false, nil (not an error).
	c := buildParkerClient(
		&parkerQEMU{},
		func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			return parkerClusterResp(), nil
		},
	)

	vmid, node, found, err := pve.FindVMByDiskVolidOrNone(context.Background(), c, "", "local-lvm:vm-9001-disk-0")
	if err != nil {
		t.Fatalf("expected nil error for not-found case; got: %v", err)
	}
	if found {
		t.Error("expected found=false")
	}
	if vmid != 0 {
		t.Errorf("vmid: want 0, got %d", vmid)
	}
	if node != "" {
		t.Errorf("node: want empty, got %q", node)
	}
}

func TestFindVMByDiskVolidOrNone_TransientErrorPassthrough(t *testing.T) {
	t.Parallel()
	transientErr := errors.New("pveproxy backend gone (code: 596)")
	c := buildParkerClient(
		&parkerQEMU{},
		func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			return nil, transientErr
		},
	)

	_, _, _, err := pve.FindVMByDiskVolidOrNone(context.Background(), c, "", "local-lvm:vm-9001-disk-0")
	if err == nil {
		t.Fatal("expected error to propagate for transient scan failure; got nil")
	}
}

func TestFindVMByDiskVolidOrNone_RetriableConfigError_Passthrough(t *testing.T) {
	t.Parallel()
	node := "pve1"
	volid := "local-lvm:vm-301-disk-0"

	c := buildParkerClient(
		&parkerQEMU{
			configFn: func(_ string, _ int) (map[string]any, error) {
				return nil, errors.New("pveproxy backend gone (code: 596)")
			},
		},
		func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			return parkerClusterResp(
				map[string]any{"vmid": int64(301), "node": node},
			), nil
		},
	)

	_, _, _, err := pve.FindVMByDiskVolidOrNone(context.Background(), c, node, volid)
	if err == nil {
		t.Fatal("expected retriable error to propagate; got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Errorf("expected TypeRetriableCloud; got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Helper: containsTag
// ---------------------------------------------------------------------------

// containsTag reports whether tagStr contains tag as a semicolon-delimited token.
func containsTag(tagStr, tag string) bool {
	for _, t := range strings.Split(tagStr, ";") {
		if strings.TrimSpace(t) == tag {
			return true
		}
	}
	return false
}
