package pve_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// errTestTransport is a generic transport-failure sentinel used by
// attached-disk CID tests that need Config or UpdateQemuConfig to fail.
var errTestTransport = errors.New("attached-disk CID test: simulated transport failure")

// noopClusterList is a cluster.ListResourcesParams listFn for tests that
// exercise UpdateAttachedDiskCID/RemoveAttachedDiskCID: neither function
// calls Cluster(), so a call here indicates a test wiring mistake.
func noopClusterList(_ context.Context, _ *cluster.ListResourcesParams) (*cluster.ListResourcesResponse, error) {
	panic("attached-disk CID codec must not call Cluster()")
}

// ---------------------------------------------------------------------------
// UpdateAttachedDiskCID
// ---------------------------------------------------------------------------

func TestUpdateAttachedDiskCID_RecordsEntry(t *testing.T) {
	t.Parallel()
	node := "pve1"
	vmid := 100
	bareVolid := "local-lvm:vm-9001-disk-0"
	cid := "pvd-eyJ2IjoibG9jYWwtbHZtOnZtLTkwMDEtZGlzay0wIiwibSI6eyJwb29sIjoibG9jYWwtbHZtIn19"

	var capturedDesc string
	nodesSvc := &parkerNodesService{
		updateFn: func(_, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			if params.Description != nil {
				capturedDesc = *params.Description
			}
			return nil
		},
	}
	c := buildParkerClientWithNodes(
		&parkerQEMU{configFn: func(_ string, _ int) (map[string]any, error) { return map[string]any{}, nil }},
		noopClusterList,
		nodesSvc,
	)

	pve.UpdateAttachedDiskCID(context.Background(), c, nopLogger(), node, vmid, bareVolid, cid)

	if capturedDesc == "" {
		t.Fatal("expected UpdateQemuConfig to be called with a new description")
	}
	got := pve.GetAttachedDiskCIDs(capturedDesc)
	if got[bareVolid] != cid {
		t.Errorf("recorded CID: want %q, got %q (map: %v)", cid, got[bareVolid], got)
	}
}

// TestUpdateAttachedDiskCID_MergePreservesOtherKeys seeds the VM description
// with non-BOSH prose, a bosh_parked_disks entry, and an unknown sentinel
// key, then verifies all three survive a bosh_attached_disks write.
func TestUpdateAttachedDiskCID_MergePreservesOtherKeys(t *testing.T) {
	t.Parallel()
	node := "pve1"
	vmid := 100
	bareVolid := "local-lvm:vm-9002-disk-0"
	cid := "local-lvm:vm-9002-disk-0"
	humanNote := "operator note: do not delete"

	seededDesc := humanNote + "\n" +
		`<!--BOSH:{"bosh_parked_disks":{"local-lvm:vm-9099-disk-0":{"disk_cid":"local-lvm:vm-9099-disk-0","parked_at":"2026-06-01T00:00:00Z","node":"pve1"}},"unknown_future_key":{"x":1}}-->`

	nodesSvc := &parkerNodesService{}
	var capturedDesc string
	nodesSvc.updateFn = func(_, _ string, params *sdknodes.UpdateQemuConfigParams) error {
		if params.Description != nil {
			capturedDesc = *params.Description
		}
		return nil
	}
	c := buildParkerClientWithNodes(
		&parkerQEMU{configFn: func(_ string, _ int) (map[string]any, error) {
			return map[string]any{"description": seededDesc}, nil
		}},
		noopClusterList,
		nodesSvc,
	)

	pve.UpdateAttachedDiskCID(context.Background(), c, nopLogger(), node, vmid, bareVolid, cid)

	if capturedDesc == "" {
		t.Fatal("expected UpdateQemuConfig to be called")
	}
	if !strings.Contains(capturedDesc, humanNote) {
		t.Errorf("description %q must preserve non-BOSH prose", capturedDesc)
	}
	if !strings.Contains(capturedDesc, "bosh_parked_disks") {
		t.Errorf("description %q must preserve bosh_parked_disks", capturedDesc)
	}
	if !strings.Contains(capturedDesc, "unknown_future_key") {
		t.Errorf("description %q must preserve unknown_future_key", capturedDesc)
	}
	got := pve.GetAttachedDiskCIDs(capturedDesc)
	if got[bareVolid] != cid {
		t.Errorf("recorded CID: want %q, got %q", cid, got[bareVolid])
	}
}

// TestUpdateAttachedDiskCID_ConfigFetchFailure_BestEffort verifies a Config
// fetch failure does not panic and simply skips the write (UpdateQemuConfig
// never called).
func TestUpdateAttachedDiskCID_ConfigFetchFailure_BestEffort(t *testing.T) {
	t.Parallel()
	updateCalled := false
	nodesSvc := &parkerNodesService{
		updateFn: func(_, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
			updateCalled = true
			return nil
		},
	}
	c := buildParkerClientWithNodes(
		&parkerQEMU{configFn: func(_ string, _ int) (map[string]any, error) {
			return nil, errTestTransport
		}},
		noopClusterList,
		nodesSvc,
	)

	// Must not panic.
	pve.UpdateAttachedDiskCID(context.Background(), c, nopLogger(), "pve1", 100, "local-lvm:vm-1-disk-0", "cid")

	if updateCalled {
		t.Error("expected UpdateQemuConfig NOT to be called when Config fetch fails")
	}
}

// TestUpdateAttachedDiskCID_UpdateFailure_BestEffort verifies an
// UpdateQemuConfig failure is swallowed (logged WARN, no panic, no error
// propagation possible since the function returns nothing).
func TestUpdateAttachedDiskCID_UpdateFailure_BestEffort(t *testing.T) {
	t.Parallel()
	logger, obs := log.NewObservedLogger(log.LevelWarn)

	nodesSvc := &parkerNodesService{
		updateFn: func(_, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
			return errTestTransport
		},
	}
	c := buildParkerClientWithNodes(
		&parkerQEMU{configFn: func(_ string, _ int) (map[string]any, error) { return map[string]any{}, nil }},
		noopClusterList,
		nodesSvc,
	)

	// Must not panic despite the write failing.
	pve.UpdateAttachedDiskCID(context.Background(), c, logger, "pve1", 100, "local-lvm:vm-1-disk-0", "cid")

	found := false
	for _, e := range obs.All() {
		if e.Level == log.LevelWarn {
			found = true
		}
	}
	if !found {
		t.Error("expected a WARN log entry on UpdateQemuConfig failure")
	}
}

// TestUpdateAttachedDiskCID_NilNodesService_NoOp mirrors
// updateParkerProvenance's "no nodes service available" guard: when Nodes()
// returns nil the function must skip silently rather than panicking.
func TestUpdateAttachedDiskCID_NilNodesService_NoOp(t *testing.T) {
	t.Parallel()
	c := buildParkerClient(
		&parkerQEMU{configFn: func(_ string, _ int) (map[string]any, error) { return map[string]any{}, nil }},
		noopClusterList,
	)
	// buildParkerClient's underlying diskClusterClient.Nodes() returns nil.
	pve.UpdateAttachedDiskCID(context.Background(), c, nopLogger(), "pve1", 100, "local-lvm:vm-1-disk-0", "cid")
}

// TestUpdateAttachedDiskCID_BlankCID_NoOp verifies a blank cidVerbatim never
// triggers a Config fetch or write (nothing meaningful to record).
func TestUpdateAttachedDiskCID_BlankCID_NoOp(t *testing.T) {
	t.Parallel()
	configCalled := false
	c := buildParkerClientWithNodes(
		&parkerQEMU{configFn: func(_ string, _ int) (map[string]any, error) {
			configCalled = true
			return map[string]any{}, nil
		}},
		noopClusterList,
		&parkerNodesService{},
	)
	pve.UpdateAttachedDiskCID(context.Background(), c, nopLogger(), "pve1", 100, "local-lvm:vm-1-disk-0", "")
	if configCalled {
		t.Error("expected no Config fetch for a blank cidVerbatim")
	}
}

// ---------------------------------------------------------------------------
// RemoveAttachedDiskCID
// ---------------------------------------------------------------------------

func TestRemoveAttachedDiskCID_RemovesEntry(t *testing.T) {
	t.Parallel()
	node := "pve1"
	vmid := 100
	bareVolid := "local-lvm:vm-9003-disk-0"
	otherVolid := "local-lvm:vm-9004-disk-0"

	seededDesc := `<!--BOSH:{"bosh_attached_disks":{"local-lvm:vm-9003-disk-0":"pvd-xyz","local-lvm:vm-9004-disk-0":"pvd-abc"}}-->`

	var capturedDesc string
	nodesSvc := &parkerNodesService{
		updateFn: func(_, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			if params.Description != nil {
				capturedDesc = *params.Description
			}
			return nil
		},
	}
	c := buildParkerClientWithNodes(
		&parkerQEMU{configFn: func(_ string, _ int) (map[string]any, error) {
			return map[string]any{"description": seededDesc}, nil
		}},
		noopClusterList,
		nodesSvc,
	)

	pve.RemoveAttachedDiskCID(context.Background(), c, nopLogger(), node, vmid, bareVolid)

	if capturedDesc == "" {
		t.Fatal("expected UpdateQemuConfig to be called")
	}
	got := pve.GetAttachedDiskCIDs(capturedDesc)
	if _, exists := got[bareVolid]; exists {
		t.Errorf("removed volid %q must not be present; map: %v", bareVolid, got)
	}
	if got[otherVolid] != "pvd-abc" {
		t.Errorf("other entry %q must survive removal; map: %v", otherVolid, got)
	}
}

// TestRemoveAttachedDiskCID_PreservesOtherKeys verifies non-BOSH prose and
// foreign sentinel keys survive a removal write.
func TestRemoveAttachedDiskCID_PreservesOtherKeys(t *testing.T) {
	t.Parallel()
	node := "pve1"
	vmid := 100
	bareVolid := "local-lvm:vm-9005-disk-0"
	humanNote := "operator note: do not delete"

	seededDesc := humanNote + "\n" +
		`<!--BOSH:{"bosh_attached_disks":{"local-lvm:vm-9005-disk-0":"pvd-xyz"},"bosh_parked_disks":{"local-lvm:vm-9099-disk-0":{"disk_cid":"x","parked_at":"2026-06-01T00:00:00Z","node":"pve1"}}}-->`

	var capturedDesc string
	nodesSvc := &parkerNodesService{
		updateFn: func(_, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			if params.Description != nil {
				capturedDesc = *params.Description
			}
			return nil
		},
	}
	c := buildParkerClientWithNodes(
		&parkerQEMU{configFn: func(_ string, _ int) (map[string]any, error) {
			return map[string]any{"description": seededDesc}, nil
		}},
		noopClusterList,
		nodesSvc,
	)

	pve.RemoveAttachedDiskCID(context.Background(), c, nopLogger(), node, vmid, bareVolid)

	if !strings.Contains(capturedDesc, humanNote) {
		t.Errorf("description %q must preserve non-BOSH prose", capturedDesc)
	}
	if !strings.Contains(capturedDesc, "bosh_parked_disks") {
		t.Errorf("description %q must preserve bosh_parked_disks", capturedDesc)
	}
	if strings.Contains(capturedDesc, "bosh_attached_disks") {
		t.Errorf("description %q must drop empty bosh_attached_disks key entirely", capturedDesc)
	}
}

// TestRemoveAttachedDiskCID_AbsentEntry_SkipsConfigWrite verifies no
// UpdateQemuConfig call is made when the volid has no recorded entry.
func TestRemoveAttachedDiskCID_AbsentEntry_SkipsConfigWrite(t *testing.T) {
	t.Parallel()
	updateCalled := false
	nodesSvc := &parkerNodesService{
		updateFn: func(_, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
			updateCalled = true
			return nil
		},
	}
	c := buildParkerClientWithNodes(
		&parkerQEMU{configFn: func(_ string, _ int) (map[string]any, error) {
			return map[string]any{"description": `<!--BOSH:{"bosh_attached_disks":{"local-lvm:vm-1-disk-0":"cid-1"}}-->`}, nil
		}},
		noopClusterList,
		nodesSvc,
	)

	pve.RemoveAttachedDiskCID(context.Background(), c, nopLogger(), "pve1", 100, "local-lvm:vm-2-disk-0")

	if updateCalled {
		t.Error("expected UpdateQemuConfig NOT to be called when the entry is absent")
	}
}

// TestRemoveAttachedDiskCID_ConfigFetchFailure_BestEffort mirrors the update
// path: a Config fetch error must not panic and must skip the write.
func TestRemoveAttachedDiskCID_ConfigFetchFailure_BestEffort(t *testing.T) {
	t.Parallel()
	updateCalled := false
	nodesSvc := &parkerNodesService{
		updateFn: func(_, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
			updateCalled = true
			return nil
		},
	}
	c := buildParkerClientWithNodes(
		&parkerQEMU{configFn: func(_ string, _ int) (map[string]any, error) {
			return nil, errTestTransport
		}},
		noopClusterList,
		nodesSvc,
	)

	pve.RemoveAttachedDiskCID(context.Background(), c, nopLogger(), "pve1", 100, "local-lvm:vm-1-disk-0")

	if updateCalled {
		t.Error("expected UpdateQemuConfig NOT to be called when Config fetch fails")
	}
}

// ---------------------------------------------------------------------------
// GetAttachedDiskCIDs
// ---------------------------------------------------------------------------

func TestGetAttachedDiskCIDs_EmptyDescription(t *testing.T) {
	t.Parallel()
	got := pve.GetAttachedDiskCIDs("")
	if len(got) != 0 {
		t.Errorf("want empty map for empty description, got %v", got)
	}
}

func TestGetAttachedDiskCIDs_NoSentinel(t *testing.T) {
	t.Parallel()
	got := pve.GetAttachedDiskCIDs("just prose, no sentinel block")
	if len(got) != 0 {
		t.Errorf("want empty map when no sentinel present, got %v", got)
	}
}

func TestGetAttachedDiskCIDs_CorruptSentinelJSON_FallsBackToEmpty(t *testing.T) {
	t.Parallel()
	// Sentinel present but the JSON payload is malformed.
	got := pve.GetAttachedDiskCIDs(`<!--BOSH:{not valid json-->`)
	if len(got) != 0 {
		t.Errorf("want empty map for corrupted sentinel JSON, got %v", got)
	}
}

func TestGetAttachedDiskCIDs_CorruptOwnKeyValue_FallsBackToEmpty(t *testing.T) {
	t.Parallel()
	// Sentinel JSON is valid, but bosh_attached_disks's value is the wrong shape
	// (array instead of object) — our own key fails to decode; other keys are
	// unaffected (not exercised here, but the corrupt key alone must not panic).
	got := pve.GetAttachedDiskCIDs(`<!--BOSH:{"bosh_attached_disks":["not","a","map"]}-->`)
	if len(got) != 0 {
		t.Errorf("want empty map when bosh_attached_disks value is malformed, got %v", got)
	}
}

func TestGetAttachedDiskCIDs_ReturnsRecordedEntries(t *testing.T) {
	t.Parallel()
	got := pve.GetAttachedDiskCIDs(`<!--BOSH:{"bosh_attached_disks":{"local-lvm:vm-1-disk-0":"pvd-abc","local-lvm:vm-2-disk-0":"pvd-def"}}-->`)
	if got["local-lvm:vm-1-disk-0"] != "pvd-abc" || got["local-lvm:vm-2-disk-0"] != "pvd-def" {
		t.Errorf("unexpected map contents: %v", got)
	}
}

// ---------------------------------------------------------------------------
// Round trip: Update then Get
// ---------------------------------------------------------------------------

func TestAttachedDiskCID_UpdateThenGet_RoundTrip(t *testing.T) {
	t.Parallel()
	bareVolid := "local-lvm:vm-9010-disk-0"
	envelopeCID := "pvd-eyJ2IjoibG9jYWwtbHZtOnZtLTkwMTAtZGlzay0wIiwibSI6eyJwb29sIjoibG9jYWwtbHZtIiwibm9kZSI6InB2ZTEifX0"

	var descState string
	nodesSvc := &parkerNodesService{
		updateFn: func(_, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			if params.Description != nil {
				descState = *params.Description
			}
			return nil
		},
	}
	c := buildParkerClientWithNodes(
		&parkerQEMU{configFn: func(_ string, _ int) (map[string]any, error) {
			return map[string]any{"description": descState}, nil
		}},
		noopClusterList,
		nodesSvc,
	)

	pve.UpdateAttachedDiskCID(context.Background(), c, nopLogger(), "pve1", 100, bareVolid, envelopeCID)

	got := pve.GetAttachedDiskCIDs(descState)
	if got[bareVolid] != envelopeCID {
		t.Errorf("round trip: want %q, got %q", envelopeCID, got[bareVolid])
	}
}
