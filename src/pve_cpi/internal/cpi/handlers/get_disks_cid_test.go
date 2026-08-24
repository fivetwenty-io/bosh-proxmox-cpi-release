package handlers_test

import (
	"context"
	"sync"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestHandleGetDisks_ReturnsRecordedCIDWhenPresent verifies get_disks echoes
// the Director-supplied disk_cid recorded by attach_disk instead of the bare
// volid, when a bosh_attached_disks entry exists for the attached volid.
func TestHandleGetDisks_ReturnsRecordedCIDWhenPresent(t *testing.T) {
	t.Parallel()
	const recordedCID = "pvd-eyJ2IjoibG9jYWwtbHZtOnZtLTkwMDEtZGlzay0wIn0"
	qemuSvc := &getDisksQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
				"scsi0":       "local-lvm:vm-100-disk-0", // system disk → excluded
				"scsi1":       "local-lvm:vm-9001-disk-0",
				"description": `<!--BOSH:{"bosh_attached_disks":{"local-lvm:vm-9001-disk-0":"` + recordedCID + `"}}-->`,
			}, nil
		},
	}

	h := handlers.HandleGetDisks(getDisksDeps(qemuSvc))
	result, err := h.Handle(context.Background(), marshalArgs("100"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	diskCIDs := result.([]string)
	if len(diskCIDs) != 1 || diskCIDs[0] != recordedCID {
		t.Errorf("want [%q], got %v", recordedCID, diskCIDs)
	}
}

// TestHandleGetDisks_FallsBackWhenVolidNotRecorded verifies get_disks falls
// back to the bare volid, re-encoded through EncodeDiskCID, when a sentinel
// is present but has no entry for the currently-attached volid (e.g. a stale
// entry for a different disk).
func TestHandleGetDisks_FallsBackWhenVolidNotRecorded(t *testing.T) {
	t.Parallel()
	const bareVolid = "local-lvm:vm-9001-disk-0"
	qemuSvc := &getDisksQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
				"scsi0":       "local-lvm:vm-100-disk-0",
				"scsi1":       bareVolid,
				"description": `<!--BOSH:{"bosh_attached_disks":{"local-lvm:vm-9099-disk-0":"pvd-other"}}-->`,
			}, nil
		},
	}

	h := handlers.HandleGetDisks(getDisksDeps(qemuSvc))
	result, err := h.Handle(context.Background(), marshalArgs("100"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	diskCIDs := result.([]string)
	want := mustEncodeDiskCID(t, bareVolid, nil)
	if len(diskCIDs) != 1 || diskCIDs[0] != want {
		t.Errorf("want re-encoded fallback [%s], got %v", want, diskCIDs)
	}
}

// TestHandleGetDisks_CorruptSentinel_FallsBackGracefully verifies a
// corrupted description sentinel does not error get_disks; every disk falls
// back to its bare volid, re-encoded through EncodeDiskCID.
func TestHandleGetDisks_CorruptSentinel_FallsBackGracefully(t *testing.T) {
	t.Parallel()
	const bareVolid = "local-lvm:vm-9001-disk-0"
	qemuSvc := &getDisksQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
				"scsi0":       "local-lvm:vm-100-disk-0",
				"scsi1":       bareVolid,
				"description": `<!--BOSH:{not valid json-->`,
			}, nil
		},
	}

	h := handlers.HandleGetDisks(getDisksDeps(qemuSvc))
	result, err := h.Handle(context.Background(), marshalArgs("100"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("corrupt sentinel must not error get_disks: %v", err)
	}

	diskCIDs := result.([]string)
	want := mustEncodeDiskCID(t, bareVolid, nil)
	if len(diskCIDs) != 1 || diskCIDs[0] != want {
		t.Errorf("want re-encoded fallback on corrupt sentinel [%s], got %v", want, diskCIDs)
	}
}

// TestHandleGetDisks_NoDescription_FallsBackToBareVolid verifies the
// no-sentinel-at-all case (pre-feature VM, or a disk attached by a
// pre-envelope CPI release) returns the bare volid re-encoded through
// EncodeDiskCID — a decodable pvd- envelope rather than the raw
// "<storage>:<volid>" string every other disk handler now rejects.
func TestHandleGetDisks_NoDescription_FallsBackToBareVolid(t *testing.T) {
	t.Parallel()
	const bareVolid = "local-lvm:vm-9001-disk-0"
	qemuSvc := &getDisksQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
				"scsi0": "local-lvm:vm-100-disk-0",
				"scsi1": bareVolid,
			}, nil
		},
	}

	h := handlers.HandleGetDisks(getDisksDeps(qemuSvc))
	result, err := h.Handle(context.Background(), marshalArgs("100"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	diskCIDs := result.([]string)
	want := mustEncodeDiskCID(t, bareVolid, nil)
	if len(diskCIDs) != 1 || diskCIDs[0] != want {
		t.Errorf("want re-encoded fallback [%s], got %v", want, diskCIDs)
	}
}

// ---------------------------------------------------------------------------
// Round trip: attach_disk records a pvd- envelope CID, get_disks returns
// that exact string.
// ---------------------------------------------------------------------------

// roundTripCfgStore is a mutable, concurrency-safe VM config map shared by
// attach_disk's Config/AttachDisk calls and get_disks's Config call in the
// round-trip test, so a get_disks call observes the description attach_disk
// wrote via UpdateQemuConfig.
type roundTripCfgStore struct {
	mu  sync.Mutex
	cfg map[string]any
}

func (s *roundTripCfgStore) get() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make(map[string]any, len(s.cfg))
	for k, v := range s.cfg {
		cp[k] = v
	}
	return cp
}

func (s *roundTripCfgStore) setDescription(desc string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg["description"] = desc
}

// roundTripQEMUService implements just enough of qemu.Service (Config,
// AttachDisk) to drive both attach_disk and get_disks against the same
// backing roundTripCfgStore. Any other method panics via the embedded
// zero-value qemu.Service (not expected to be called in this test).
type roundTripQEMUService struct {
	qemu.Service
	store              *roundTripCfgStore
	attachReturnDiskID string
}

func (m *roundTripQEMUService) Config(_ context.Context, _ string, _ int) (map[string]any, error) {
	return m.store.get(), nil
}

func (m *roundTripQEMUService) AttachDisk(_ context.Context, _ string, _ int, _ string, _ string, _ *qemu.AttachOpts) (string, error) {
	return m.attachReturnDiskID, nil
}

// ListSnapshots satisfies attach_disk's snapshot pre-flight guard
// (attachDiskSnapshotGuard); returning the synthetic "current" entry means
// no real snapshots exist, so the guard proceeds without affecting this test.
func (m *roundTripQEMUService) ListSnapshots(_ context.Context, _ string, _ int) ([]map[string]any, error) {
	return []map[string]any{{"name": "current"}}, nil
}

// TestAttachThenGetDisks_RoundTripReturnsExactCID drives attach_disk with a
// pvd- envelope CID (carrying pool/node/az metadata that cannot be
// reconstructed from PVE state), then calls get_disks against the same VM
// and asserts the returned CID is byte-identical to what attach_disk
// received — the cloudcheck membership fidelity this feature exists for.
func TestAttachThenGetDisks_RoundTripReturnsExactCID(t *testing.T) {
	t.Parallel()
	const vmCID = "100"
	bareVolid := "local-lvm:vm-9010-disk-0"
	envelopeCID := mustEncodeDiskCID(t, bareVolid, &pve.DiskCIDMeta{Pool: "local-lvm", Node: "pve-node1", AZ: "az1"})

	store := &roundTripCfgStore{cfg: map[string]any{
		"scsi0": testDiskCID,
		"scsi1": bareVolid,
	}}
	qemuSvc := &roundTripQEMUService{store: store, attachReturnDiskID: "scsi1"}
	nodesSvc := &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _ string, _ string, params *nodes.UpdateQemuConfigParams) error {
			if params.Description != nil {
				store.setDescription(*params.Description)
			}
			return nil
		},
	}

	deps := testDepsFoundVM(100, qemuSvc, nodesSvc, nil, &captureAgent{})

	attachH := handlers.HandleAttachDisk(deps)
	if _, err := attachH.Handle(context.Background(), attachArgs(t, vmCID, envelopeCID), jsonrpc.Context{}); err != nil {
		t.Fatalf("attach_disk: unexpected error: %v", err)
	}

	getH := handlers.HandleGetDisks(deps)
	result, err := getH.Handle(context.Background(), marshalArgs(vmCID), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("get_disks: unexpected error: %v", err)
	}

	diskCIDs, ok := result.([]string)
	if !ok {
		t.Fatalf("get_disks: result: want []string, got %T", result)
	}
	if len(diskCIDs) != 1 {
		t.Fatalf("get_disks: want 1 persistent disk, got %d: %v", len(diskCIDs), diskCIDs)
	}
	if diskCIDs[0] != envelopeCID {
		t.Errorf("round trip: want exact envelope CID %q, got %q", envelopeCID, diskCIDs[0])
	}
}
