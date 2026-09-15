package pve_test

import (
	"context"
	"strings"
	"testing"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// ---------------------------------------------------------------------------
// GetVMStemcell / SetVMStemcellOnDescription round-trip
// ---------------------------------------------------------------------------

func TestVMStemcell_RoundTrip(t *testing.T) {
	t.Parallel()

	sc := &pve.VMStemcell{
		Label: "bosh-openstack-kvm-ubuntu-noble-1.585",
		CID:   ":heavy:local-lvm:import/bosh-stemcell-bosh-openstack-kvm-ubuntu-noble-1.585-deadbeef.qcow2",
		Kind:  "heavy",
		SHA8:  "deadbeef",
	}
	desc, err := pve.SetVMStemcellOnDescription("", sc)
	if err != nil {
		t.Fatalf("SetVMStemcellOnDescription: %v", err)
	}

	got, ok := pve.GetVMStemcell(desc)
	if !ok {
		t.Fatal("expected a bosh_stemcell record after the write")
	}
	if *got != *sc {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, sc)
	}
	// The exact dotted version is the whole point of keeping the label out of
	// the tag alphabet, so guard it explicitly.
	if !strings.Contains(desc, "noble-1.585") {
		t.Errorf("dotted stemcell version lost on the wire: %q", desc)
	}
}

func TestVMStemcell_EmptyFieldsOmittedFromWire(t *testing.T) {
	t.Parallel()

	sc := &pve.VMStemcell{Label: "bosh-openstack-kvm-ubuntu-noble-1.585", CID: ":light:local:import/x-00000000.qcow2"}
	desc, err := pve.SetVMStemcellOnDescription("", sc)
	if err != nil {
		t.Fatalf("SetVMStemcellOnDescription: %v", err)
	}
	if strings.Contains(desc, "sha8") || strings.Contains(desc, "kind") {
		t.Errorf("empty fields must be omitted from the wire form, got %q", desc)
	}

	got, ok := pve.GetVMStemcell(desc)
	if !ok || got.SHA8 != "" || got.Label != "bosh-openstack-kvm-ubuntu-noble-1.585" {
		t.Errorf("decode of omitted fields: got %+v ok=%v", got, ok)
	}
}

func TestVMStemcell_AbsentAndCorrupt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		desc string
	}{
		{"empty description", ""},
		{"prose only", "deployment: dep1\njob: web\n"},
		{"sentinel without bosh_stemcell", `<!--BOSH:{"bosh_pool":{"name":"p"}}-->`},
		{"corrupt sentinel JSON", `<!--BOSH:{"bosh_stemcell":{-->`},
		{"corrupt bosh_stemcell value", `<!--BOSH:{"bosh_stemcell":"not-an-object"}-->`},
		{"record names nothing", `<!--BOSH:{"bosh_stemcell":{"sha8":"deadbeef"}}-->`},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if sc, ok := pve.GetVMStemcell(tc.desc); ok {
				t.Errorf("expected no record, got %+v", sc)
			}
		})
	}
}

func TestVMStemcell_PreservesOtherSentinelKeysAndProse(t *testing.T) {
	t.Parallel()

	seed := "operator note\n" + `<!--BOSH:{"bosh_attached_disks":{"local-lvm:vm-1-disk-1":"pvd-x"},"bosh_pool":{"name":"p","layer":"template"},"bosh_future_key":[1,2]}-->`
	desc, err := pve.SetVMStemcellOnDescription(seed, &pve.VMStemcell{
		Label: "bosh-openstack-kvm-ubuntu-noble-1.585", CID: ":heavy:local:import/x-deadbeef.qcow2",
	})
	if err != nil {
		t.Fatalf("SetVMStemcellOnDescription: %v", err)
	}

	if !strings.Contains(desc, "operator note") {
		t.Errorf("non-BOSH prose lost: %q", desc)
	}
	if got := pve.GetAttachedDiskCIDs(desc); got["local-lvm:vm-1-disk-1"] != "pvd-x" {
		t.Errorf("bosh_attached_disks lost across stemcell write: %v", got)
	}
	if pm, ok := pve.GetPoolMembership(desc); !ok || pm.Name != "p" {
		t.Errorf("bosh_pool lost across stemcell write: %+v ok=%v", pm, ok)
	}
	if !strings.Contains(desc, "bosh_future_key") {
		t.Errorf("unknown sentinel key lost: %q", desc)
	}

	// Deleting the record must leave everything else intact.
	cleared, err := pve.SetVMStemcellOnDescription(desc, nil)
	if err != nil {
		t.Fatalf("SetVMStemcellOnDescription(nil): %v", err)
	}
	if _, ok := pve.GetVMStemcell(cleared); ok {
		t.Error("record survived deletion")
	}
	if pm, ok := pve.GetPoolMembership(cleared); !ok || pm.Name != "p" {
		t.Errorf("bosh_pool lost across stemcell delete: %+v ok=%v", pm, ok)
	}
}

// ---------------------------------------------------------------------------
// UpdateVMCreateProvenance (client write path)
// ---------------------------------------------------------------------------

func TestUpdateVMCreateProvenance_WritesBothRecordsInOneCall(t *testing.T) {
	t.Parallel()

	seedDesc := `<!--BOSH:{"bosh_attached_disks":{"v":"c"}}-->`
	var capturedDesc string
	writes := 0
	nodesSvc := &parkerNodesService{
		updateFn: func(_, _ string, params *sdknodes.UpdateQemuConfigParams) error {
			writes++
			if params.Description != nil {
				capturedDesc = *params.Description
			}
			return nil
		},
	}
	c := buildParkerClientWithNodes(
		&parkerQEMU{configFn: func(_ string, _ int) (map[string]any, error) {
			return map[string]any{"description": seedDesc}, nil
		}},
		noopClusterList,
		nodesSvc,
	)

	pve.UpdateVMCreateProvenance(context.Background(), c, nopLogger(), "pve1", 100,
		&pve.PoolMembership{Name: "bosh-d1-dep1", Layer: pve.PoolLayerTemplate},
		&pve.VMStemcell{
			Label: "bosh-openstack-kvm-ubuntu-noble-1.585", CID: ":heavy:local:import/x-deadbeef.qcow2", Kind: "heavy", SHA8: "deadbeef",
		})

	if capturedDesc == "" {
		t.Fatal("expected UpdateQemuConfig to be called with a new description")
	}
	// Both records must land in a single write: create_vm runs this for every
	// VM a deploy creates, so a second round trip here is a real cost.
	if writes != 1 {
		t.Errorf("want exactly 1 description write, got %d", writes)
	}
	sc, ok := pve.GetVMStemcell(capturedDesc)
	if !ok || sc.Label != "bosh-openstack-kvm-ubuntu-noble-1.585" {
		t.Errorf("written stemcell record: got %+v ok=%v", sc, ok)
	}
	pm, ok := pve.GetPoolMembership(capturedDesc)
	if !ok || pm.Name != "bosh-d1-dep1" {
		t.Errorf("written pool record: got %+v ok=%v", pm, ok)
	}
	if got := pve.GetAttachedDiskCIDs(capturedDesc); got["v"] != "c" {
		t.Errorf("bosh_attached_disks lost by provenance write: %v", got)
	}
}

// A caller that knows only one of the two records leaves the other alone
// rather than deleting it.
func TestUpdateVMCreateProvenance_NilRecordIsSkippedNotDeleted(t *testing.T) {
	t.Parallel()

	seedDesc := `<!--BOSH:{"bosh_pool":{"name":"kept","layer":"template"}}-->`
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
			return map[string]any{"description": seedDesc}, nil
		}},
		noopClusterList,
		nodesSvc,
	)

	pve.UpdateVMCreateProvenance(context.Background(), c, nopLogger(), "pve1", 100, nil, &pve.VMStemcell{
		Label: "bosh-openstack-kvm-ubuntu-noble-1.585", CID: ":heavy:local:import/x-deadbeef.qcow2",
	})

	if pm, ok := pve.GetPoolMembership(capturedDesc); !ok || pm.Name != "kept" {
		t.Errorf("a nil pool record must leave the existing one intact: %+v ok=%v", pm, ok)
	}
	if sc, ok := pve.GetVMStemcell(capturedDesc); !ok || sc.Label == "" {
		t.Errorf("stemcell record not written: %+v ok=%v", sc, ok)
	}
}

func TestUpdateVMCreateProvenance_BestEffortOnFailures(t *testing.T) {
	t.Parallel()

	// A config read that fails must not panic and must not attempt a write.
	writeCalled := false
	nodesSvc := &parkerNodesService{
		updateFn: func(_, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
			writeCalled = true
			return nil
		},
	}
	c := buildParkerClientWithNodes(
		&parkerQEMU{configFn: func(_ string, _ int) (map[string]any, error) {
			return nil, context.DeadlineExceeded
		}},
		noopClusterList,
		nodesSvc,
	)

	pve.UpdateVMCreateProvenance(context.Background(), c, nopLogger(), "pve1", 100, nil, &pve.VMStemcell{Label: "l", CID: "c"})
	if writeCalled {
		t.Error("a failed config read must not be followed by a description write")
	}

	// A write that fails is swallowed the same way: provenance is advisory, and
	// losing it must never fail the create_vm the caller asked for.
	failingWrite := buildParkerClientWithNodes(
		&parkerQEMU{configFn: func(_ string, _ int) (map[string]any, error) {
			return map[string]any{"description": ""}, nil
		}},
		noopClusterList,
		&parkerNodesService{
			updateFn: func(_, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
				return context.DeadlineExceeded
			},
		},
	)
	pve.UpdateVMCreateProvenance(context.Background(), failingWrite, nopLogger(), "pve1", 100,
		&pve.PoolMembership{Name: "p", Layer: pve.PoolLayerTemplate},
		&pve.VMStemcell{Label: "l", CID: "c"})

	// Guard clauses: nothing is attempted with no records or an unusable target.
	pve.UpdateVMCreateProvenance(context.Background(), c, nopLogger(), "pve1", 100, nil, nil)
	pve.UpdateVMCreateProvenance(context.Background(), c, nopLogger(), "", 100, nil, &pve.VMStemcell{Label: "l"})
	pve.UpdateVMCreateProvenance(context.Background(), c, nopLogger(), "pve1", 0, nil, &pve.VMStemcell{Label: "l"})
	pve.UpdateVMCreateProvenance(context.Background(), nil, nopLogger(), "pve1", 100, nil, &pve.VMStemcell{Label: "l"})
}

// A managed VM carries its storage-allocation marker as plain text in the same
// description field. The provenance write now happens even when no resource
// pool is configured, so this is the first write that touches a marker-bearing
// description on a pool-less managed create; the marker has to come back out
// intact and unambiguous.
func TestUpdateVMCreateProvenance_PreservesStorageAllocationMarker(t *testing.T) {
	t.Parallel()

	marker := pve.StorageAllocationMarker{
		Version:      1,
		Namespace:    "ocfp-cf1-lab-mgmt",
		AllocationID: "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		AgentSHA256:  strings.Repeat("ab", 32),
		Kind:         "vm",
	}
	encoded, err := pve.FormatStorageAllocationMarker(marker)
	if err != nil {
		t.Fatalf("FormatStorageAllocationMarker: %v", err)
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
	c := buildParkerClientWithNodes(
		&parkerQEMU{configFn: func(_ string, _ int) (map[string]any, error) {
			return map[string]any{"description": encoded}, nil
		}},
		noopClusterList,
		nodesSvc,
	)

	pve.UpdateVMCreateProvenance(context.Background(), c, nopLogger(), "pve1", 100, nil, &pve.VMStemcell{
		Label: "bosh-openstack-kvm-ubuntu-noble-1.585", CID: ":heavy:local:import/x-deadbeef.qcow2",
	})

	got, found, parseErr := pve.ParseStorageAllocationMarker(capturedDesc)
	if parseErr != nil || !found {
		t.Fatalf("allocation marker lost or mangled by the provenance write: found=%v err=%v desc=%q", found, parseErr, capturedDesc)
	}
	if got != marker {
		t.Errorf("allocation marker changed: got %+v, want %+v", got, marker)
	}
	if sc, ok := pve.GetVMStemcell(capturedDesc); !ok || sc.Label == "" {
		t.Errorf("stemcell record not written alongside the marker: %+v ok=%v", sc, ok)
	}
}
