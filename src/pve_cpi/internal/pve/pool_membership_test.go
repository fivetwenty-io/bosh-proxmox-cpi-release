package pve_test

import (
	"context"
	"strings"
	"testing"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// ---------------------------------------------------------------------------
// GetPoolMembership / SetPoolMembershipOnDescription round-trip
// ---------------------------------------------------------------------------

func TestPoolMembership_RoundTrip(t *testing.T) {
	t.Parallel()

	pm := &pve.PoolMembership{
		Name:          "bosh-d1-dep1",
		Layer:         pve.PoolLayerTemplate,
		Director:      "d1",
		Deployment:    "dep1",
		InstanceGroup: "web",
	}
	desc, err := pve.SetPoolMembershipOnDescription("", pm)
	if err != nil {
		t.Fatalf("SetPoolMembershipOnDescription: %v", err)
	}

	got, ok := pve.GetPoolMembership(desc)
	if !ok {
		t.Fatal("expected a bosh_pool record after the write")
	}
	if *got != *pm {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, pm)
	}
}

func TestPoolMembership_EmptyTokensOmittedFromWire(t *testing.T) {
	t.Parallel()

	pm := &pve.PoolMembership{Name: "bosh-create-env", Layer: pve.PoolLayerTemplate, Deployment: "create-env"}
	desc, err := pve.SetPoolMembershipOnDescription("", pm)
	if err != nil {
		t.Fatalf("SetPoolMembershipOnDescription: %v", err)
	}
	if strings.Contains(desc, "director") || strings.Contains(desc, "instance_group") {
		t.Errorf("empty tokens must be omitted from the wire form, got %q", desc)
	}

	got, ok := pve.GetPoolMembership(desc)
	if !ok || got.Director != "" || got.Deployment != "create-env" {
		t.Errorf("decode of omitted tokens: got %+v ok=%v", got, ok)
	}
}

func TestPoolMembership_AbsentAndCorrupt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		desc string
	}{
		{"empty description", ""},
		{"prose only", "director: d1\ndeployment: dep1\n"},
		{"sentinel without bosh_pool", `<!--BOSH:{"bosh_attached_disks":{"v":"c"}}-->`},
		{"corrupt sentinel JSON", `<!--BOSH:{"bosh_pool":{-->`},
		{"corrupt bosh_pool value", `<!--BOSH:{"bosh_pool":"not-an-object"}-->`},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if pm, ok := pve.GetPoolMembership(tc.desc); ok {
				t.Errorf("expected no record, got %+v", pm)
			}
		})
	}
}

func TestPoolMembership_PreservesOtherSentinelKeysAndProse(t *testing.T) {
	t.Parallel()

	seed := "operator note\n" + `<!--BOSH:{"bosh_attached_disks":{"local-lvm:vm-1-disk-1":"pvd-x"},"bosh_future_key":[1,2]}-->`
	desc, err := pve.SetPoolMembershipOnDescription(seed, &pve.PoolMembership{
		Name: "bosh-d1-dep1", Layer: pve.PoolLayerTemplate,
	})
	if err != nil {
		t.Fatalf("SetPoolMembershipOnDescription: %v", err)
	}

	if !strings.Contains(desc, "operator note") {
		t.Errorf("non-BOSH prose lost: %q", desc)
	}
	if got := pve.GetAttachedDiskCIDs(desc); got["local-lvm:vm-1-disk-1"] != "pvd-x" {
		t.Errorf("bosh_attached_disks lost across pool write: %v", got)
	}
	if !strings.Contains(desc, "bosh_future_key") {
		t.Errorf("unknown sentinel key lost: %q", desc)
	}

	// Deleting the record must leave everything else intact.
	cleared, err := pve.SetPoolMembershipOnDescription(desc, nil)
	if err != nil {
		t.Fatalf("SetPoolMembershipOnDescription(nil): %v", err)
	}
	if _, ok := pve.GetPoolMembership(cleared); ok {
		t.Error("record survived deletion")
	}
	if got := pve.GetAttachedDiskCIDs(cleared); got["local-lvm:vm-1-disk-1"] != "pvd-x" {
		t.Errorf("bosh_attached_disks lost across pool delete: %v", got)
	}
}

// ---------------------------------------------------------------------------
// UpdatePoolMembership (client write path)
// ---------------------------------------------------------------------------

func TestUpdatePoolMembership_WritesMergedDescription(t *testing.T) {
	t.Parallel()

	seedDesc := `<!--BOSH:{"bosh_attached_disks":{"v":"c"}}-->`
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

	pve.UpdatePoolMembership(context.Background(), c, nopLogger(), "pve1", 100, &pve.PoolMembership{
		Name: "bosh-d1-dep1", Layer: pve.PoolLayerTemplate, Director: "d1", Deployment: "dep1",
	})

	if capturedDesc == "" {
		t.Fatal("expected UpdateQemuConfig to be called with a new description")
	}
	pm, ok := pve.GetPoolMembership(capturedDesc)
	if !ok || pm.Name != "bosh-d1-dep1" {
		t.Errorf("written record: got %+v ok=%v", pm, ok)
	}
	if got := pve.GetAttachedDiskCIDs(capturedDesc); got["v"] != "c" {
		t.Errorf("bosh_attached_disks lost by pool membership write: %v", got)
	}
}

func TestUpdatePoolMembership_ConfigFetchFailure_BestEffort(t *testing.T) {
	t.Parallel()

	updateCalled := false
	nodesSvc := &parkerNodesService{
		updateFn: func(_, _ string, _ *sdknodes.UpdateQemuConfigParams) error {
			updateCalled = true
			return nil
		},
	}
	c := buildParkerClientWithNodes(
		&parkerQEMU{configFn: func(_ string, _ int) (map[string]any, error) { return nil, errTestTransport }},
		noopClusterList,
		nodesSvc,
	)

	// Must not panic and must not write on a failed read.
	pve.UpdatePoolMembership(context.Background(), c, nopLogger(), "pve1", 100, &pve.PoolMembership{
		Name: "p", Layer: pve.PoolLayerTemplate,
	})
	if updateCalled {
		t.Error("UpdateQemuConfig must not be called when the config read fails")
	}
}
