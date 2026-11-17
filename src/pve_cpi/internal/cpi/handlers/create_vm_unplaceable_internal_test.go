package handlers

import (
	"context"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// TestClassifyFilterResult_UnplaceableIsTransient checks that a node rejected
// because it cannot take new work for a reason other than maintenance, such as
// a lost quorum, counts as transient, as a node in maintenance does.
func TestClassifyFilterResult_UnplaceableIsTransient(t *testing.T) {
	t.Parallel()
	rejections := map[string]string{
		"lab-pve-cpi-az2-0": "node offline",
		"lab-pve-cpi-az2-1": "node not placeable (entry node does not report the cluster quorate)",
	}
	if !classifyFilterResult(rejections) {
		t.Error("a rejection for a lost quorum must be retriable")
	}
}

// TestResolveTargetNode_LocalPinNamesLostQuorum pins a VM to the node that
// holds its local disk on a cluster that has lost quorum. The error names the
// lost quorum instead of saying the node is in maintenance.
func TestResolveTargetNode_LocalPinNamesLostQuorum(t *testing.T) {
	t.Parallel()
	deps := buildFaultDomainDeps(
		[]map[string]any{onlineNode("pve1"), onlineNode("pve2")},
		nil,
		func(c *config.CPIConfig) {
			c.Placement = &config.PlacementConfig{AZMap: map[string][]string{"zone-a": {"pve1", "pve2"}}}
		},
	)
	deps.PVE.(*faultDomainTestPVE).clusterClient.notQuorate = true
	_, err := resolveTargetNodeWithRNG(context.Background(), deps, createVMCloudProps{},
		[]string{encodeLocalCID(t, "pve1")}, nil, nil)
	if err == nil {
		t.Fatal("expected an error for a local pin on a cluster without quorum")
	}
	if strings.Contains(err.Error(), "maintenance") {
		t.Errorf("error must not blame maintenance: %v", err)
	}
	if !strings.Contains(err.Error(), "entry node does not report the cluster quorate") {
		t.Errorf("error must name the lost quorum: %v", err)
	}
}
