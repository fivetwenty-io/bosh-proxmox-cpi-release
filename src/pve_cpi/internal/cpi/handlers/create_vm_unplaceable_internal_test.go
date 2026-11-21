package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/placement"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
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

// TestResolveTargetNode_LocalPinOnANodeWithUnreadNotesIsRetriable pins a VM
// to the node that holds its local disk while that node's notes cannot be
// read. The create cannot go anywhere else, and the node comes back once it
// answers, so the error is retriable and says the notes could not be read.
func TestResolveTargetNode_LocalPinOnANodeWithUnreadNotesIsRetriable(t *testing.T) {
	t.Parallel()
	pinned := onlineNode("pve1")
	pinned["notes_unreadable"] = true
	deps := buildFaultDomainDeps(
		[]map[string]any{pinned, onlineNode("pve2")},
		nil,
		func(c *config.CPIConfig) {
			c.Placement = &config.PlacementConfig{AZMap: map[string][]string{"zone-a": {"pve1", "pve2"}}}
		},
	)
	_, err := resolveTargetNodeWithRNG(context.Background(), deps, createVMCloudProps{},
		[]string{encodeLocalCID(t, "pve1")}, nil, nil)
	if err == nil {
		t.Fatal("expected an error for a local pin on a node whose notes cannot be read")
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || !typed.OkToRetry() {
		t.Fatalf("want a retriable error, got %T %v", err, err)
	}
	if !strings.Contains(err.Error(), "notes could not be read") {
		t.Errorf("error must say the notes could not be read: %v", err)
	}
}

// TestClassifyFilterResult_UnreadNotesMakeTheExhaustionRetriable checks that
// a node left out only because its notes could not be read makes the
// exhaustion retriable, even beside a node rejected for good, because the
// unread node can take the VM once it answers.
func TestClassifyFilterResult_UnreadNotesMakeTheExhaustionRetriable(t *testing.T) {
	t.Parallel()
	facts := placement.NodeFacts{
		Node: "pve2", Online: true, InMaintenance: true, NotesUnread: true,
		UnplaceableReason: pve.NotesUnreadReason(errors.New("node config unavailable")),
	}
	_, rejections := placement.Filter([]placement.NodeFacts{facts}, placement.Request{})
	rejections["pve1"] = "insufficient free storage"
	if !classifyFilterResult(rejections) {
		t.Errorf("a node whose notes could not be read must make the exhaustion retriable: %v", rejections)
	}
}
