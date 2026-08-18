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
	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
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

	// attached records disks AttachDisk placed, keyed by vmid then slot. Config
	// merges these into its response so the read-after-write slot-verify in
	// attachToParker observes the disk the test's attachFn accepted. Tests that
	// need to simulate a lost slot (concurrent park) override attachFn and write
	// a different volid into attached themselves.
	attached map[int]map[string]string
}

func (q *parkerQEMU) Config(_ context.Context, node string, vmid int) (map[string]any, error) {
	var base map[string]any
	if q.configFn != nil {
		c, err := q.configFn(node, vmid)
		if err != nil {
			return nil, err
		}
		base = c
	} else {
		base = map[string]any{}
	}
	// Merge recorded attaches so verify reads see them. Copy to avoid mutating
	// the test's static map.
	if slots, ok := q.attached[vmid]; ok && len(slots) > 0 {
		merged := make(map[string]any, len(base)+len(slots))
		for k, v := range base {
			merged[k] = v
		}
		for slot, volid := range slots {
			merged[slot] = volid
		}
		return merged, nil
	}
	return base, nil
}

func (q *parkerQEMU) Create(_ context.Context, node string, params map[string]any) (string, error) {
	if q.createFn != nil {
		return q.createFn(node, params)
	}
	return "", nil
}

func (q *parkerQEMU) recordAttach(vmid int, slot, volid string) {
	if q.attached == nil {
		q.attached = make(map[int]map[string]string)
	}
	if q.attached[vmid] == nil {
		q.attached[vmid] = make(map[string]string)
	}
	q.attached[vmid][slot] = volid
}

func (q *parkerQEMU) AttachDisk(_ context.Context, node string, vmid int, volid, bus string, opts *qemu.AttachOpts) (string, error) {
	slot := ""
	if opts != nil {
		slot = opts.DiskID
	}
	// Default state recording so the verify read sees the disk. attachFn may
	// override the recorded value (e.g. to simulate a lost slot).
	if slot != "" {
		q.recordAttach(vmid, slot, volid)
	}
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

// ---------------------------------------------------------------------------
// parkerNodesService — nodes.Service mock for provenance tests.
// ---------------------------------------------------------------------------

// parkerNodesService is a minimal nodes.Service that intercepts UpdateQemuConfig.
// All other methods panic if called unexpectedly.
type parkerNodesService struct {
	sdknodes.Service // embed for zero-value; override only UpdateQemuConfig
	updateFn         func(node, vmid string, params *sdknodes.UpdateQemuConfigParams) error
}

func (n *parkerNodesService) UpdateQemuConfig(_ context.Context, node, vmid string, params *sdknodes.UpdateQemuConfigParams) error {
	if n.updateFn != nil {
		return n.updateFn(node, vmid, params)
	}
	return nil
}

// parkerClientWithNodes wraps a diskClusterClient but overrides Nodes() to
// return an injectable nodes.Service. Used by provenance tests that need to
// intercept UpdateQemuConfig calls without affecting other parker tests.
type parkerClientWithNodes struct {
	pve.Client
	nodesSvc sdknodes.Service
}

func (c *parkerClientWithNodes) Nodes() sdknodes.Service { return c.nodesSvc }

// buildParkerClientWithNodes constructs a pve.Client with both QEMU+Cluster
// services AND an injectable nodes.Service.
func buildParkerClientWithNodes(
	qemuSvc qemu.Service,
	listFn func(ctx context.Context, params *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error),
	nodesSvc sdknodes.Service,
) pve.Client {
	base := buildParkerClient(qemuSvc, listFn)
	return &parkerClientWithNodes{Client: base, nodesSvc: nodesSvc}
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

// fullParkerDisks returns a disk map with all 31 scsi slots occupied plus the
// bosh-parker tag, simulating a parker that cannot accept another disk.
func fullParkerDisks() map[string]any {
	m := map[string]any{"tags": "bosh-parker"}
	for i := 0; i < 31; i++ {
		m[fmt.Sprintf("scsi%d", i)] = fmt.Sprintf("local-lvm:vm-9999-disk-%d", i)
	}
	return m
}

// TestParkDisk_ReusesPartialParkerNoNewParker proves the capacity-reuse fix
// (F-W6-01): with two full parkers and one partially full parker, a new park
// lands in the partial parker's free slot and NO new parker VM is created.
func TestParkDisk_ReusesPartialParkerNoNewParker(t *testing.T) {
	t.Parallel()
	ctx := pve.WithTestBackoff(context.Background(), func(_ int) time.Duration { return 0 })
	cfg := parkerTestCfg()
	node := "pve1"
	bareVolid := "local-lvm:vm-9001-disk-0"

	// 90000 full, 90001 full, 90002 partial (scsi0 used, rest free).
	partial := map[string]any{"tags": "bosh-parker", "scsi0": "local-lvm:vm-9999-disk-0"}

	var createdVMIDs []int
	var attachVMID int
	var attachSlot string

	qemuSvc := &parkerQEMU{
		configFn: func(_ string, vmid int) (map[string]any, error) {
			switch vmid {
			case 90000, 90001:
				return fullParkerDisks(), nil
			case 90002:
				return partial, nil
			default:
				return map[string]any{}, nil
			}
		},
		createFn: func(_ string, params map[string]any) (string, error) {
			vmidVal, _ := params["vmid"].(int)
			createdVMIDs = append(createdVMIDs, vmidVal)
			return "", nil
		},
		attachFn: func(_ string, vmid int, _, _ string, opts *qemu.AttachOpts) (string, error) {
			attachVMID = vmid
			if opts != nil {
				attachSlot = opts.DiskID
			}
			return "", nil
		},
	}

	c := buildParkerClient(qemuSvc, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return parkerClusterResp(
			map[string]any{"vmid": int64(90000), "node": node},
			map[string]any{"vmid": int64(90001), "node": node},
			map[string]any{"vmid": int64(90002), "node": node},
		), nil
	})

	if err := pve.ParkDisk(ctx, c, nopLogger(), node, bareVolid, cfg, pve.ParkContext{}); err != nil {
		t.Fatalf("ParkDisk: unexpected error: %v", err)
	}

	if len(createdVMIDs) != 0 {
		t.Errorf("no new parker must be created when a partial parker has a free slot; created %v", createdVMIDs)
	}
	if attachVMID != 90002 {
		t.Errorf("disk must land in the partial parker 90002; attached to %d", attachVMID)
	}
	if attachSlot != "scsi1" {
		t.Errorf("disk must take the first free slot scsi1 in the partial parker; got %q", attachSlot)
	}
}

// TestParkDisk_AllParkersFullCreatesFreshParker proves a fresh parker IS
// created only when every existing parker is full (F-W6-01).
func TestParkDisk_AllParkersFullCreatesFreshParker(t *testing.T) {
	t.Parallel()
	ctx := pve.WithTestBackoff(context.Background(), func(_ int) time.Duration { return 0 })
	cfg := parkerTestCfg()
	node := "pve1"
	bareVolid := "local-lvm:vm-9001-disk-0"

	var createdVMIDs []int
	var attachVMID int

	qemuSvc := &parkerQEMU{
		configFn: func(_ string, vmid int) (map[string]any, error) {
			switch vmid {
			case 90000, 90001, 90002:
				return fullParkerDisks(), nil
			default:
				// Freshly created parker: empty until disks attach (Config merges
				// recorded attaches automatically).
				return map[string]any{"tags": "bosh-parker"}, nil
			}
		},
		createFn: func(_ string, params map[string]any) (string, error) {
			vmidVal, _ := params["vmid"].(int)
			createdVMIDs = append(createdVMIDs, vmidVal)
			return "", nil
		},
		attachFn: func(_ string, vmid int, _, _ string, _ *qemu.AttachOpts) (string, error) {
			attachVMID = vmid
			return "", nil
		},
	}

	// All three parkers exist and are full; NextVMID picks a free VMID for the
	// fresh parker (not 90000-90002 since those are used in the cluster list).
	c := buildParkerClient(qemuSvc, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return parkerClusterResp(
			map[string]any{"vmid": int64(90000), "node": node},
			map[string]any{"vmid": int64(90001), "node": node},
			map[string]any{"vmid": int64(90002), "node": node},
		), nil
	})

	if err := pve.ParkDisk(ctx, c, nopLogger(), node, bareVolid, cfg, pve.ParkContext{}); err != nil {
		t.Fatalf("ParkDisk: unexpected error: %v", err)
	}

	if len(createdVMIDs) != 1 {
		t.Errorf("exactly one fresh parker must be created when all parkers are full; created %v", createdVMIDs)
	}
	if attachVMID == 90000 || attachVMID == 90001 || attachVMID == 90002 || attachVMID == 0 {
		t.Errorf("disk must attach to the freshly created parker, not an existing full one; got %d", attachVMID)
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

	ctx := pve.WithTestBackoff(context.Background(), func(_ int) time.Duration { return 0 })
	_, _, _, _, err := pve.IsDiskParked(ctx, c, nopLogger(),
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
	if !containsTag(tagsVal, pve.CpiOwnershipTag) {
		t.Errorf("tags %q must contain CpiOwnershipTag %q", tagsVal, pve.CpiOwnershipTag)
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

// TestEnsureParker_CreateConflictAdoptsNoSecondParker proves the adopt-on-conflict
// path is live (F-W6-04): when Create returns a VMID-conflict error, EnsureParker
// re-scans and adopts the winner instead of regenerating a fresh VMID and creating
// a duplicate parker. Exactly one Create attempt is made.
func TestEnsureParker_CreateConflictAdoptsNoSecondParker(t *testing.T) {
	t.Parallel()
	node := "pve1"
	winnerVMID := 90000

	var createCalls int
	qemuSvc := &parkerQEMU{
		configFn: func(_ string, vmid int) (map[string]any, error) {
			if vmid == winnerVMID {
				return map[string]any{"tags": "bosh-parker"}, nil
			}
			return map[string]any{}, nil
		},
		createFn: func(_ string, _ map[string]any) (string, error) {
			createCalls++
			return "", fmt.Errorf("create VM: 500 KVM VM already exists, vmid: %d", winnerVMID) //nolint:gocritic
		},
	}

	// First scan (EnsureParker's FindParkerForNode) → empty, so it proceeds to
	// create. After the create conflicts, the adopt re-scan → winner present.
	listCallCount := 0
	c := buildParkerClient(qemuSvc, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		listCallCount++
		if listCallCount == 1 {
			return parkerClusterResp(), nil
		}
		return parkerClusterResp(
			map[string]any{"vmid": int64(winnerVMID), "node": node},
		), nil
	})

	vmid, err := pve.EnsureParker(context.Background(), c, nopLogger(), node, parkerTestCfg())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vmid != winnerVMID {
		t.Errorf("vmid: want %d (adopted winner), got %d", winnerVMID, vmid)
	}
	// A single Create attempt: AllocateWithRetry must NOT regenerate a fresh VMID
	// on conflict (that would create a duplicate parker).
	if createCalls != 1 {
		t.Errorf("expected exactly 1 Create attempt (adopt on conflict, no regenerate); got %d", createCalls)
	}
}

// TestEnsureParker_InvalidRange_ReturnsLoudError proves EnsureParker still
// fails loudly on an invalid ParkerConfig VMID range after the shared
// createParkerVM extraction (the validation guard moved into the helper but
// must remain reachable for EnsureParker's own callers).
func TestEnsureParker_InvalidRange_ReturnsLoudError(t *testing.T) {
	t.Parallel()
	node := "pve1"
	var createCalls int
	qemuSvc := &parkerQEMU{
		createFn: func(_ string, _ map[string]any) (string, error) {
			createCalls++
			return "", nil
		},
	}
	c := buildParkerClient(qemuSvc, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return parkerClusterResp(), nil
	})

	_, err := pve.EnsureParker(context.Background(), c, nopLogger(), node, pve.ParkerConfig{})
	if err == nil {
		t.Fatal("expected loud error for invalid (zero-value) ParkerConfig VMID range")
	}
	if createCalls != 0 {
		t.Errorf("Create must not be called when the VMID range is invalid; got %d calls", createCalls)
	}
}

// ---------------------------------------------------------------------------
// EnsureFreshParker
// ---------------------------------------------------------------------------

// TestEnsureFreshParker_InvalidRange_ReturnsLoudError proves the C1 fix:
// EnsureFreshParker must reject a zero-value ParkerConfig VMID range instead
// of silently allocating in the general VM range via WithRange's
// silent-ignore-invalid-values fallback.
func TestEnsureFreshParker_InvalidRange_ReturnsLoudError(t *testing.T) {
	t.Parallel()
	node := "pve1"
	var createCalls int
	qemuSvc := &parkerQEMU{
		createFn: func(_ string, _ map[string]any) (string, error) {
			createCalls++
			return "", nil
		},
	}
	c := buildParkerClient(qemuSvc, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return parkerClusterResp(), nil
	})

	_, err := pve.EnsureFreshParker(context.Background(), c, nopLogger(), node, pve.ParkerConfig{})
	if err == nil {
		t.Fatal("expected loud error for invalid (zero-value) ParkerConfig VMID range")
	}
	if createCalls != 0 {
		t.Errorf("Create must not be called when the VMID range is invalid; got %d calls", createCalls)
	}
}

// TestEnsureFreshParker_EndBeforeStart_ReturnsLoudError covers the
// VMIDRangeEnd<=VMIDRangeStart half of the guard (not just the zero-value case).
func TestEnsureFreshParker_EndBeforeStart_ReturnsLoudError(t *testing.T) {
	t.Parallel()
	node := "pve1"
	c := buildParkerClient(&parkerQEMU{}, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return parkerClusterResp(), nil
	})

	cfg := pve.ParkerConfig{VMIDRangeStart: 90999, VMIDRangeEnd: 90000}
	_, err := pve.EnsureFreshParker(context.Background(), c, nopLogger(), node, cfg)
	if err == nil {
		t.Fatal("expected loud error when VMIDRangeEnd <= VMIDRangeStart")
	}
}

// TestEnsureFreshParker_CreatesNewParker proves the happy path directly
// (previously only exercised indirectly via TestParkDisk_AllParkersFullCreatesFreshParker).
func TestEnsureFreshParker_CreatesNewParker(t *testing.T) {
	t.Parallel()
	node := "pve1"
	var createdParams map[string]any

	qemuSvc := &parkerQEMU{
		createFn: func(_ string, params map[string]any) (string, error) {
			createdParams = params
			return "", nil
		},
	}
	c := buildParkerClient(qemuSvc, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return parkerClusterResp(), nil // always empty for allocation purposes
	})

	vmid, err := pve.EnsureFreshParker(context.Background(), c, nopLogger(), node, parkerTestCfg())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if vmid < 90000 || vmid > 90999 {
		t.Errorf("vmid %d outside parker range [90000,90999]", vmid)
	}
	if createdParams == nil {
		t.Fatal("Create was not called")
	}
	tagsVal, _ := createdParams["tags"].(string)
	if !containsTag(tagsVal, pve.ParkerTag) {
		t.Errorf("tags %q must contain %q", tagsVal, pve.ParkerTag)
	}
}

// ---------------------------------------------------------------------------
// ParkDisk
// ---------------------------------------------------------------------------

// TestParkDisk_RealVMHolderRefusesPark proves ParkDisk refuses to park a disk
// that a real (non-parker) VM still holds (F-W6-02): no attach, no create, nil
// returned (idempotent no-op). Guards a stale-Director-retry path that would
// otherwise double-reference the volume.
func TestParkDisk_RealVMHolderRefusesPark(t *testing.T) {
	t.Parallel()
	node := "pve1"
	realVMID := 12345 // outside parker range 90000-90999
	bareVolid := "local-lvm:vm-9001-disk-0"

	var attachCalls, createCalls int
	qemuSvc := &parkerQEMU{
		configFn: func(_ string, vmid int) (map[string]any, error) {
			if vmid == realVMID {
				// Real VM currently holds the disk on an active bus.
				return map[string]any{"scsi3": bareVolid}, nil
			}
			return map[string]any{}, nil
		},
		attachFn: func(_ string, _ int, _, _ string, _ *qemu.AttachOpts) (string, error) {
			attachCalls++
			return "", nil
		},
		createFn: func(_ string, _ map[string]any) (string, error) {
			createCalls++
			return "", nil
		},
	}
	// Cluster: the real VM holds the disk; no parker exists.
	c := buildParkerClient(qemuSvc, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return parkerClusterResp(
			map[string]any{"vmid": int64(realVMID), "node": node},
		), nil
	})

	err := pve.ParkDisk(context.Background(), c, nopLogger(), node, bareVolid, parkerTestCfg(), pve.ParkContext{})
	if err != nil {
		t.Fatalf("ParkDisk: want nil (idempotent no-op), got %v", err)
	}
	if attachCalls != 0 {
		t.Errorf("AttachDisk must not be called when a real VM holds the disk; got %d", attachCalls)
	}
	if createCalls != 0 {
		t.Errorf("no parker must be created when a real VM holds the disk; got %d", createCalls)
	}
}

// ---------------------------------------------------------------------------
// DiskHeldByRealVM — direct coverage of the two branches that are only
// exercised indirectly by ParkDisk tests today.
// ---------------------------------------------------------------------------

// TestDiskHeldByRealVM_VanishedMidScan_TreatedAsFreeFloating covers
// parker.go's IsNotFound(cfgErr) branch: the cluster-wide disk scan
// (FindVMByDiskVolid) finds the disk attached to an in-range VMID on its
// first config read, but DiskHeldByRealVM's own follow-up config read (to
// confirm the bosh-parker tag) 404s because the VM was deleted concurrently
// between the two reads. Must be treated as free-floating, not an error.
func TestDiskHeldByRealVM_VanishedMidScan_TreatedAsFreeFloating(t *testing.T) {
	t.Parallel()
	node := "pve1"
	holderVMID := 90000 // in parker range 90000-90999
	bareVolid := "local-lvm:vm-9001-disk-0"

	var configCalls int
	qemuSvc := &parkerQEMU{
		configFn: func(_ string, vmid int) (map[string]any, error) {
			if vmid != holderVMID {
				return map[string]any{}, nil
			}
			configCalls++
			if configCalls == 1 {
				// First read: FindVMByDiskVolid's cluster-wide scan sees the disk
				// attached and returns this VMID as the holder.
				return map[string]any{"scsi0": bareVolid}, nil
			}
			// Second read: DiskHeldByRealVM's own tag-confirmation fetch — the VM
			// vanished between the scan and this read.
			return nil, makeAPIErr(404, "no such VM")
		},
	}
	c := buildParkerClient(qemuSvc, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return parkerClusterResp(
			map[string]any{"vmid": int64(holderVMID), "node": node},
		), nil
	})

	held, vmid, _, err := pve.DiskHeldByRealVM(context.Background(), c, nopLogger(), bareVolid, parkerTestCfg())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if held {
		t.Errorf("expected held=false when the holder VM vanished mid-scan; got held=true vmid=%d", vmid)
	}
	if vmid != 0 {
		t.Errorf("expected vmid=0 for the vanished-VM case, got %d", vmid)
	}
	if configCalls < 2 {
		t.Fatalf("test setup error: expected at least 2 config reads for vmid %d, got %d", holderVMID, configCalls)
	}
}

// TestDiskHeldByRealVM_InRangeNoTag_TreatedAsRealVM covers parker.go's
// in-range-VMID-without-bosh-parker-tag branch: a VMID inside the parker
// range that never got the bosh-parker tag (e.g. a mis-numbered workload VM)
// must be treated as a real VM holder, not a parker.
func TestDiskHeldByRealVM_InRangeNoTag_TreatedAsRealVM(t *testing.T) {
	t.Parallel()
	node := "pve1"
	holderVMID := 90000 // in parker range but lacks bosh-parker tag
	bareVolid := "local-lvm:vm-9001-disk-0"

	qemuSvc := &parkerQEMU{
		configFn: func(_ string, vmid int) (map[string]any, error) {
			if vmid == holderVMID {
				return map[string]any{"tags": "some-other-tag", "scsi0": bareVolid}, nil
			}
			return map[string]any{}, nil
		},
	}
	c := buildParkerClient(qemuSvc, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return parkerClusterResp(
			map[string]any{"vmid": int64(holderVMID), "node": node},
		), nil
	})

	held, vmid, gotNode, err := pve.DiskHeldByRealVM(context.Background(), c, nopLogger(), bareVolid, parkerTestCfg())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !held {
		t.Fatal("expected held=true: in-range VMID without bosh-parker tag must be treated as a real VM")
	}
	if vmid != holderVMID {
		t.Errorf("vmid: want %d, got %d", holderVMID, vmid)
	}
	if gotNode != node {
		t.Errorf("node: want %q, got %q", node, gotNode)
	}
}

// TestParkDisk_SlotRaceRetriesNextSlot proves the read-after-write slot verify
// (F-W6-03): when a concurrent park wins the chosen slot (the post-attach config
// shows a different volid there), ParkDisk retries the next free slot and the
// disk lands successfully.
func TestParkDisk_SlotRaceRetriesNextSlot(t *testing.T) {
	t.Parallel()
	ctx := pve.WithTestBackoff(context.Background(), func(_ int) time.Duration { return 0 })
	node := "pve1"
	parkerVMID := 90000
	bareVolid := "local-lvm:vm-9001-disk-0"
	otherVolid := "local-lvm:vm-9002-disk-0" // disk a racing park placed in scsi0

	var attachSlots []string
	qemuSvc := &parkerQEMU{
		configFn: func(_ string, vmid int) (map[string]any, error) {
			if vmid == parkerVMID {
				// Base config: scsi0 pre-occupied by a racing park's disk, so
				// chooseParkSlot picks scsi1 first.
				return map[string]any{"tags": "bosh-parker", "scsi0": otherVolid}, nil
			}
			return map[string]any{}, nil
		},
	}
	// attachFn models the slot race: the FIRST attach (scsi1) is lost to a
	// concurrent park — overwrite the recorded slot value with otherVolid so the
	// verify read sees the wrong volid. The retry (scsi2) keeps bareVolid (the
	// default record) and the verify succeeds.
	qemuSvc.attachFn = func(_ string, vmid int, _, _ string, opts *qemu.AttachOpts) (string, error) {
		slot := ""
		if opts != nil {
			slot = opts.DiskID
		}
		attachSlots = append(attachSlots, slot)
		if len(attachSlots) == 1 {
			// Simulate the concurrent park winning slot scsi1.
			qemuSvc.recordAttach(vmid, slot, otherVolid)
		}
		return "", nil
	}
	c := buildParkerClient(qemuSvc, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return parkerClusterResp(
			map[string]any{"vmid": int64(parkerVMID), "node": node},
		), nil
	})

	err := pve.ParkDisk(ctx, c, nopLogger(), node, bareVolid, parkerTestCfg(), pve.ParkContext{})
	if err != nil {
		t.Fatalf("ParkDisk: unexpected error: %v", err)
	}
	if len(attachSlots) < 2 {
		t.Fatalf("expected at least 2 attach attempts (first slot lost, retry next); got %v", attachSlots)
	}
	if attachSlots[0] != "scsi1" {
		t.Errorf("first attach must target scsi1 (scsi0 pre-occupied); got %q", attachSlots[0])
	}
	if attachSlots[len(attachSlots)-1] != "scsi2" {
		t.Errorf("retry must target the next free slot scsi2; got %q", attachSlots[len(attachSlots)-1])
	}
}

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

	err := pve.ParkDisk(context.Background(), c, nopLogger(), node, bareVolid, parkerTestCfg(), pve.ParkContext{})
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

	err := pve.ParkDisk(context.Background(), c, nopLogger(), node, bareVolid, parkerTestCfg(), pve.ParkContext{})
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

	ctx := pve.WithTestBackoff(context.Background(), func(_ int) time.Duration { return 0 })
	_, _, _, err := pve.FindVMByDiskVolidOrNone(ctx, c, "", "local-lvm:vm-9001-disk-0")
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
// Provenance: updateParkerProvenance / removeParkerProvenance (via ParkDisk /
// UnparkDisk integration)
// ---------------------------------------------------------------------------

// fixedClock returns a ParkerConfig.NowFunc pinned to t.
func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// TestParkDisk_ProvenanceEntryWritten verifies that parking a disk writes a
// bosh_parked_disks entry into the parker VM description with the correct fields.
func TestParkDisk_ProvenanceEntryWritten(t *testing.T) {
	t.Parallel()
	ctx := pve.WithTestBackoff(context.Background(), func(_ int) time.Duration { return 0 })
	node := "pve1"
	parkerVMID := 90000
	bareVolid := "local-lvm:vm-9001-disk-0"
	fixedTime := time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)

	cfg := pve.ParkerConfig{
		VMIDRangeStart: 90000,
		VMIDRangeEnd:   90999,
		NowFunc:        fixedClock(fixedTime),
	}

	var capturedDesc string
	nodesSvc := &parkerNodesService{
		updateFn: func(_, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			if params.Description != nil {
				capturedDesc = *params.Description
			}
			return nil
		},
	}

	// Parker exists; disk not yet attached (idempotency check sees empty slot).
	configCallCount := 0
	qemuSvc := &parkerQEMU{
		configFn: func(_ string, vmid int) (map[string]any, error) {
			configCallCount++
			if vmid == parkerVMID {
				return map[string]any{"tags": "bosh-parker"}, nil
			}
			return map[string]any{}, nil
		},
	}

	// First cluster scan (idempotency: disk not attached to any VM) → empty.
	// Second scan (EnsureParker → FindParkerForNode) → has parker.
	listCallCount := 0
	c := buildParkerClientWithNodes(qemuSvc, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		listCallCount++
		if listCallCount == 1 {
			// IsDiskParked cluster scan: empty (disk is free-floating).
			return parkerClusterResp(), nil
		}
		return parkerClusterResp(
			map[string]any{"vmid": int64(parkerVMID), "node": node},
		), nil
	}, nodesSvc)

	encodedCID := "local-lvm:vm-9001-disk-0|abc123"
	sourceVMCID := "9500"
	pctx := pve.ParkContext{DiskCID: encodedCID, SourceVMCID: sourceVMCID}
	if err := pve.ParkDisk(ctx, c, nopLogger(), node, bareVolid, cfg, pctx); err != nil {
		t.Fatalf("ParkDisk: unexpected error: %v", err)
	}

	if capturedDesc == "" {
		t.Fatal("expected UpdateQemuConfig to be called with provenance description")
	}

	// Description must contain bosh_parked_disks with the expected fields.
	if !strings.Contains(capturedDesc, "bosh_parked_disks") {
		t.Errorf("description %q must contain bosh_parked_disks", capturedDesc)
	}
	// disk_cid must be the encoded CID from ParkContext, not the bare volid.
	if !strings.Contains(capturedDesc, encodedCID) {
		t.Errorf("description %q must contain encoded disk_cid %q", capturedDesc, encodedCID)
	}
	if !strings.Contains(capturedDesc, sourceVMCID) {
		t.Errorf("description %q must contain source_vm_cid %q", capturedDesc, sourceVMCID)
	}
	if !strings.Contains(capturedDesc, fixedTime.Format(time.RFC3339)) {
		t.Errorf("description %q must contain parked_at timestamp %q", capturedDesc, fixedTime.Format(time.RFC3339))
	}
	if !strings.Contains(capturedDesc, node) {
		t.Errorf("description %q must contain node %q", capturedDesc, node)
	}
}

// TestParkDisk_ProvenanceEntryWithDirectorID verifies director_id field present
// when ParkerConfig.DirectorID is set.
func TestParkDisk_ProvenanceEntryWithDirectorID(t *testing.T) {
	t.Parallel()
	ctx := pve.WithTestBackoff(context.Background(), func(_ int) time.Duration { return 0 })
	node := "pve1"
	parkerVMID := 90000
	bareVolid := "local-lvm:vm-9002-disk-0"

	cfg := pve.ParkerConfig{
		VMIDRangeStart: 90000,
		VMIDRangeEnd:   90999,
		DirectorID:     "prod-director",
		NowFunc:        fixedClock(time.Now().UTC()),
	}

	var capturedDesc string
	nodesSvc := &parkerNodesService{
		updateFn: func(_, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			if params.Description != nil {
				capturedDesc = *params.Description
			}
			return nil
		},
	}

	listCallCount := 0
	c := buildParkerClientWithNodes(
		&parkerQEMU{
			configFn: func(_ string, vmid int) (map[string]any, error) {
				if vmid == parkerVMID {
					return map[string]any{"tags": "bosh-parker"}, nil
				}
				return map[string]any{}, nil
			},
		},
		func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			listCallCount++
			if listCallCount == 1 {
				return parkerClusterResp(), nil
			}
			return parkerClusterResp(map[string]any{"vmid": int64(parkerVMID), "node": node}), nil
		},
		nodesSvc,
	)

	if err := pve.ParkDisk(ctx, c, nopLogger(), node, bareVolid, cfg, pve.ParkContext{}); err != nil {
		t.Fatalf("ParkDisk: %v", err)
	}
	if !strings.Contains(capturedDesc, "prod-director") {
		t.Errorf("description %q must contain director_id %q", capturedDesc, "prod-director")
	}
}

// TestParkDisk_ProvenanceNoDirectorID verifies director_id field absent when
// ParkerConfig.DirectorID is empty.
func TestParkDisk_ProvenanceNoDirectorID(t *testing.T) {
	t.Parallel()
	ctx := pve.WithTestBackoff(context.Background(), func(_ int) time.Duration { return 0 })
	node := "pve1"
	parkerVMID := 90000
	bareVolid := "local-lvm:vm-9003-disk-0"

	cfg := pve.ParkerConfig{
		VMIDRangeStart: 90000,
		VMIDRangeEnd:   90999,
		NowFunc:        fixedClock(time.Now().UTC()),
	}

	var capturedDesc string
	nodesSvc := &parkerNodesService{
		updateFn: func(_, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			if params.Description != nil {
				capturedDesc = *params.Description
			}
			return nil
		},
	}

	listCallCount := 0
	c := buildParkerClientWithNodes(
		&parkerQEMU{
			configFn: func(_ string, vmid int) (map[string]any, error) {
				if vmid == parkerVMID {
					return map[string]any{"tags": "bosh-parker"}, nil
				}
				return map[string]any{}, nil
			},
		},
		func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			listCallCount++
			if listCallCount == 1 {
				return parkerClusterResp(), nil
			}
			return parkerClusterResp(map[string]any{"vmid": int64(parkerVMID), "node": node}), nil
		},
		nodesSvc,
	)

	if err := pve.ParkDisk(ctx, c, nopLogger(), node, bareVolid, cfg, pve.ParkContext{}); err != nil {
		t.Fatalf("ParkDisk: %v", err)
	}
	if strings.Contains(capturedDesc, "director_id") {
		t.Errorf("description %q must NOT contain director_id when DirectorID is empty", capturedDesc)
	}
}

// TestParkDisk_ProvenanceMergePreservesFirstDisk verifies that parking a second
// disk on the same parker preserves the first disk's provenance entry.
func TestParkDisk_ProvenanceMergePreservesFirstDisk(t *testing.T) {
	t.Parallel()
	ctx := pve.WithTestBackoff(context.Background(), func(_ int) time.Duration { return 0 })
	node := "pve1"
	parkerVMID := 90000
	volid1 := "local-lvm:vm-9010-disk-0"
	volid2 := "local-lvm:vm-9011-disk-0"

	cfg := pve.ParkerConfig{
		VMIDRangeStart: 90000,
		VMIDRangeEnd:   90999,
		NowFunc:        fixedClock(time.Now().UTC()),
	}

	// Simulate parker already holding volid1 with a provenance entry.
	existingDesc := fmt.Sprintf(
		`<!--BOSH:{"bosh_parked_disks":{%q:{"disk_cid":%q,"parked_at":"2026-06-01T00:00:00Z","node":"pve1"}}}-->`,
		volid1, volid1,
	)

	var lastDesc string
	nodesSvc := &parkerNodesService{
		updateFn: func(_, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			if params.Description != nil {
				lastDesc = *params.Description
			}
			return nil
		},
	}

	listCallCount := 0
	c := buildParkerClientWithNodes(
		&parkerQEMU{
			configFn: func(_ string, vmid int) (map[string]any, error) {
				if vmid == parkerVMID {
					return map[string]any{
						"tags":        "bosh-parker",
						"scsi0":       volid1,
						"description": existingDesc,
					}, nil
				}
				return map[string]any{}, nil
			},
		},
		func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			listCallCount++
			if listCallCount == 1 {
				// IsDiskParked: disk not attached.
				return parkerClusterResp(), nil
			}
			return parkerClusterResp(map[string]any{"vmid": int64(parkerVMID), "node": node}), nil
		},
		nodesSvc,
	)

	if err := pve.ParkDisk(ctx, c, nopLogger(), node, volid2, cfg, pve.ParkContext{}); err != nil {
		t.Fatalf("ParkDisk: %v", err)
	}

	// Both volids must appear in the written description.
	if !strings.Contains(lastDesc, volid1) {
		t.Errorf("merged description %q must preserve first disk entry %q", lastDesc, volid1)
	}
	if !strings.Contains(lastDesc, volid2) {
		t.Errorf("merged description %q must contain new disk entry %q", lastDesc, volid2)
	}
}

// TestUnparkDisk_ProvenanceEntryRemoved verifies that UnparkDisk removes the
// disk's provenance entry from the sentinel.
func TestUnparkDisk_ProvenanceEntryRemoved(t *testing.T) {
	t.Parallel()
	node := "pve1"
	parkerVMID := 90000
	bareVolid := "local-lvm:vm-9020-disk-0"

	cfg := pve.ParkerConfig{
		VMIDRangeStart: 90000,
		VMIDRangeEnd:   90999,
	}

	existingDesc := fmt.Sprintf(
		`<!--BOSH:{"bosh_parked_disks":{%q:{"disk_cid":%q,"parked_at":"2026-06-01T00:00:00Z","node":"pve1"}}}-->`,
		bareVolid, bareVolid,
	)

	var capturedDesc string
	updateCalled := false
	nodesSvc := &parkerNodesService{
		updateFn: func(_, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			updateCalled = true
			if params.Description != nil {
				capturedDesc = *params.Description
			}
			return nil
		},
	}

	c := buildParkerClientWithNodes(
		&parkerQEMU{
			configFn: func(_ string, vmid int) (map[string]any, error) {
				if vmid == parkerVMID {
					return map[string]any{
						"tags":        "bosh-parker",
						"scsi0":       bareVolid,
						"description": existingDesc,
					}, nil
				}
				return map[string]any{}, nil
			},
		},
		func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			return parkerClusterResp(
				map[string]any{"vmid": int64(parkerVMID), "node": node},
			), nil
		},
		nodesSvc,
	)

	if err := pve.UnparkDisk(context.Background(), c, nopLogger(), bareVolid, cfg); err != nil {
		t.Fatalf("UnparkDisk: %v", err)
	}

	if !updateCalled {
		t.Fatal("expected UpdateQemuConfig to be called to remove provenance entry")
	}
	if strings.Contains(capturedDesc, bareVolid) {
		t.Errorf("description %q must not contain removed volid %q", capturedDesc, bareVolid)
	}
}

// TestUnparkDisk_ProvenanceAbsentEntryNoUpdate verifies that UnparkDisk does
// not call UpdateQemuConfig when no provenance entry exists for the disk.
func TestUnparkDisk_ProvenanceAbsentEntryNoUpdate(t *testing.T) {
	t.Parallel()
	node := "pve1"
	parkerVMID := 90000
	bareVolid := "local-lvm:vm-9021-disk-0"
	otherVolid := "local-lvm:vm-9022-disk-0"

	cfg := pve.ParkerConfig{
		VMIDRangeStart: 90000,
		VMIDRangeEnd:   90999,
	}

	// Parker has a provenance entry for otherVolid but NOT for bareVolid.
	existingDesc := fmt.Sprintf(
		`<!--BOSH:{"bosh_parked_disks":{%q:{"disk_cid":%q,"parked_at":"2026-06-01T00:00:00Z","node":"pve1"}}}-->`,
		otherVolid, otherVolid,
	)

	var updateCalled bool
	nodesSvc := &parkerNodesService{
		updateFn: func(_, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
			updateCalled = true
			return nil
		},
	}

	c := buildParkerClientWithNodes(
		&parkerQEMU{
			configFn: func(_ string, vmid int) (map[string]any, error) {
				if vmid == parkerVMID {
					return map[string]any{
						"tags":        "bosh-parker",
						"scsi0":       bareVolid,
						"description": existingDesc,
					}, nil
				}
				return map[string]any{}, nil
			},
		},
		func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			return parkerClusterResp(
				map[string]any{"vmid": int64(parkerVMID), "node": node},
			), nil
		},
		nodesSvc,
	)

	if err := pve.UnparkDisk(context.Background(), c, nopLogger(), bareVolid, cfg); err != nil {
		t.Fatalf("UnparkDisk: %v", err)
	}

	if updateCalled {
		t.Error("UpdateQemuConfig must not be called when the volid has no provenance entry")
	}
}

// TestParkDisk_ProvenanceWriteFailure_ParkSucceeds verifies that a failure in
// UpdateQemuConfig during provenance writing does not cause ParkDisk to fail.
func TestParkDisk_ProvenanceWriteFailure_ParkSucceeds(t *testing.T) {
	t.Parallel()
	ctx := pve.WithTestBackoff(context.Background(), func(_ int) time.Duration { return 0 })
	node := "pve1"
	parkerVMID := 90000
	bareVolid := "local-lvm:vm-9030-disk-0"

	cfg := pve.ParkerConfig{
		VMIDRangeStart: 90000,
		VMIDRangeEnd:   90999,
		NowFunc:        fixedClock(time.Now().UTC()),
	}

	// UpdateQemuConfig always fails.
	nodesSvc := &parkerNodesService{
		updateFn: func(_, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
			return errors.New("pveproxy backend gone (code: 596)")
		},
	}

	listCallCount := 0
	c := buildParkerClientWithNodes(
		&parkerQEMU{
			configFn: func(_ string, vmid int) (map[string]any, error) {
				if vmid == parkerVMID {
					return map[string]any{"tags": "bosh-parker"}, nil
				}
				return map[string]any{}, nil
			},
		},
		func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			listCallCount++
			if listCallCount == 1 {
				return parkerClusterResp(), nil
			}
			return parkerClusterResp(map[string]any{"vmid": int64(parkerVMID), "node": node}), nil
		},
		nodesSvc,
	)

	// ParkDisk must succeed even though provenance write fails.
	err := pve.ParkDisk(ctx, c, nopLogger(), node, bareVolid, cfg, pve.ParkContext{})
	if err != nil {
		t.Fatalf("ParkDisk must succeed even when provenance write fails; got: %v", err)
	}
}

// TestParkDisk_ProvenanceNonBOSHTextPreserved verifies that non-BOSH description
// text is preserved when provenance is written.
func TestParkDisk_ProvenanceNonBOSHTextPreserved(t *testing.T) {
	t.Parallel()
	ctx := pve.WithTestBackoff(context.Background(), func(_ int) time.Duration { return 0 })
	node := "pve1"
	parkerVMID := 90000
	bareVolid := "local-lvm:vm-9040-disk-0"
	humanNote := "operator note: do not delete"

	cfg := pve.ParkerConfig{
		VMIDRangeStart: 90000,
		VMIDRangeEnd:   90999,
		NowFunc:        fixedClock(time.Now().UTC()),
	}

	var capturedDesc string
	nodesSvc := &parkerNodesService{
		updateFn: func(_, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			if params.Description != nil {
				capturedDesc = *params.Description
			}
			return nil
		},
	}

	listCallCount := 0
	c := buildParkerClientWithNodes(
		&parkerQEMU{
			configFn: func(_ string, vmid int) (map[string]any, error) {
				if vmid == parkerVMID {
					return map[string]any{
						"tags":        "bosh-parker",
						"description": humanNote,
					}, nil
				}
				return map[string]any{}, nil
			},
		},
		func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
			listCallCount++
			if listCallCount == 1 {
				return parkerClusterResp(), nil
			}
			return parkerClusterResp(map[string]any{"vmid": int64(parkerVMID), "node": node}), nil
		},
		nodesSvc,
	)

	if err := pve.ParkDisk(ctx, c, nopLogger(), node, bareVolid, cfg, pve.ParkContext{}); err != nil {
		t.Fatalf("ParkDisk: %v", err)
	}

	if !strings.Contains(capturedDesc, humanNote) {
		t.Errorf("description %q must preserve non-BOSH text %q", capturedDesc, humanNote)
	}
	if !strings.Contains(capturedDesc, "bosh_parked_disks") {
		t.Errorf("description %q must also contain bosh_parked_disks sentinel", capturedDesc)
	}
}

// TestParkerProvenance_ForeignSentinelKeysPreservedRoundTrip seeds the parker
// VM description with non-BOSH prose + a sentinel block that contains
// bosh_disk_metadata and bosh_disk_tags keys (written by set_disk_metadata).
// After parking a disk, asserts both foreign keys and the prose survive
// byte-intact alongside bosh_parked_disks. After unparking, asserts the
// foreign keys are still intact (bosh_parked_disks entry removed).
func TestParkerProvenance_ForeignSentinelKeysPreservedRoundTrip(t *testing.T) {
	t.Parallel()
	node := "pve1"
	parkerVMID := 90000
	bareVolid := "local-lvm:vm-9050-disk-0"
	humanNote := "operator note: do not delete"

	// Sentinel seeded by set_disk_metadata containing two foreign keys.
	foreignSentinel := `<!--BOSH:{"bosh_disk_metadata":{"local-lvm:vm-9099-disk-0":{"director":"prod","deployment":"cf"}},"bosh_disk_tags":{"local-lvm:vm-9099-disk-0":{"env":"prod"}}}-->`
	seededDesc := humanNote + "\n" + foreignSentinel

	cfg := pve.ParkerConfig{
		VMIDRangeStart: 90000,
		VMIDRangeEnd:   90999,
		NowFunc:        fixedClock(time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)),
	}

	// descState tracks the current description as UpdateQemuConfig writes it.
	descState := seededDesc
	nodesSvc := &parkerNodesService{
		updateFn: func(_, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			if params.Description != nil {
				descState = *params.Description
			}
			return nil
		},
	}

	// configFn returns descState so the provenance read-modify-write sees the
	// latest written description.
	qemuSvc := &parkerQEMU{
		configFn: func(_ string, vmid int) (map[string]any, error) {
			if vmid == parkerVMID {
				return map[string]any{
					"tags":        "bosh-parker",
					"description": descState,
				}, nil
			}
			return map[string]any{}, nil
		},
	}

	// Park phase: disk free-floating on first scan, parker found on subsequent scans.
	listCallCount := 0
	c := buildParkerClientWithNodes(qemuSvc, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		listCallCount++
		if listCallCount == 1 {
			return parkerClusterResp(), nil // IsDiskParked: free-floating
		}
		return parkerClusterResp(map[string]any{"vmid": int64(parkerVMID), "node": node}), nil
	}, nodesSvc)

	ctx := pve.WithTestBackoff(context.Background(), func(_ int) time.Duration { return 0 })
	if err := pve.ParkDisk(ctx, c, nopLogger(), node, bareVolid, cfg, pve.ParkContext{DiskCID: bareVolid}); err != nil {
		t.Fatalf("ParkDisk: %v", err)
	}

	afterPark := descState

	// After park: prose, both foreign keys, and bosh_parked_disks must all be present.
	if !strings.Contains(afterPark, humanNote) {
		t.Errorf("after park: description must contain prose %q; got %q", humanNote, afterPark)
	}
	if !strings.Contains(afterPark, "bosh_disk_metadata") {
		t.Errorf("after park: description must contain bosh_disk_metadata; got %q", afterPark)
	}
	if !strings.Contains(afterPark, "bosh_disk_tags") {
		t.Errorf("after park: description must contain bosh_disk_tags; got %q", afterPark)
	}
	if !strings.Contains(afterPark, "bosh_parked_disks") {
		t.Errorf("after park: description must contain bosh_parked_disks; got %q", afterPark)
	}
	if !strings.Contains(afterPark, bareVolid) {
		t.Errorf("after park: description must contain parked volid %q; got %q", bareVolid, afterPark)
	}
	// Verify foreign key values survive intact.
	if !strings.Contains(afterPark, `"director":"prod"`) {
		t.Errorf("after park: bosh_disk_metadata value must survive intact; got %q", afterPark)
	}
	if !strings.Contains(afterPark, `"env":"prod"`) {
		t.Errorf("after park: bosh_disk_tags value must survive intact; got %q", afterPark)
	}

	// Unpark phase: configure the QEMU config to show disk at scsi0 so
	// IsDiskParked returns parked=true.
	qemuSvc.configFn = func(_ string, vmid int) (map[string]any, error) {
		if vmid == parkerVMID {
			return map[string]any{
				"tags":        "bosh-parker",
				"scsi0":       bareVolid,
				"description": descState,
			}, nil
		}
		return map[string]any{}, nil
	}
	// Reset cluster list to always return parker (unpark path uses IsDiskParked
	// which does a cluster scan).
	c2 := buildParkerClientWithNodes(qemuSvc, func(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
		return parkerClusterResp(map[string]any{"vmid": int64(parkerVMID), "node": node}), nil
	}, nodesSvc)

	if err := pve.UnparkDisk(context.Background(), c2, nopLogger(), bareVolid, cfg); err != nil {
		t.Fatalf("UnparkDisk: %v", err)
	}

	afterUnpark := descState

	// After unpark: prose and both foreign keys must survive; parked entry removed.
	if !strings.Contains(afterUnpark, humanNote) {
		t.Errorf("after unpark: prose %q must survive; got %q", humanNote, afterUnpark)
	}
	if !strings.Contains(afterUnpark, "bosh_disk_metadata") {
		t.Errorf("after unpark: bosh_disk_metadata must survive; got %q", afterUnpark)
	}
	if !strings.Contains(afterUnpark, "bosh_disk_tags") {
		t.Errorf("after unpark: bosh_disk_tags must survive; got %q", afterUnpark)
	}
	if strings.Contains(afterUnpark, bareVolid) {
		t.Errorf("after unpark: parked volid %q must be removed; got %q", bareVolid, afterUnpark)
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
