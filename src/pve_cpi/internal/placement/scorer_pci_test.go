package placement_test

import (
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/placement"
)

// ---------------------------------------------------------------------------
// PCI filter tests
// ---------------------------------------------------------------------------

func onlineFact(node string) placement.NodeFacts {
	return placement.NodeFacts{
		Node:          node,
		Online:        true,
		TotalMemBytes: 8 * gib,
		FreeMemBytes:  4 * gib,
		MaxCPU:        8,
	}
}

func TestFilter_PCIRequired_NodeWithDevice_Passes(t *testing.T) {
	t.Parallel()

	facts := []placement.NodeFacts{
		onlineFact("pve1"),
		onlineFact("pve2"),
	}

	// pve1 has the device; pve2 does not.
	checker := func(node string) (bool, error) {
		return node == "pve1", nil
	}

	req := placement.Request{
		RequiredPCIAddresses: []string{"0000:01:00.0"},
		PCIChecker:           checker,
	}
	pass, rej := placement.Filter(facts, req)

	if len(pass) != 1 || pass[0].Node != "pve1" {
		t.Errorf("expected only pve1 in pass; got %v", nodeNames(pass))
	}
	if rej["pve2"] == "" {
		t.Error("pve2 should be rejected (no device)")
	}
}

func TestFilter_PCIRequired_AllNodesLackDevice_AllRejected(t *testing.T) {
	t.Parallel()

	facts := []placement.NodeFacts{
		onlineFact("pve1"),
		onlineFact("pve2"),
	}

	checker := func(_ string) (bool, error) {
		return false, nil
	}

	req := placement.Request{
		RequiredPCIAddresses: []string{"0000:02:00.0"},
		PCIChecker:           checker,
	}
	pass, rej := placement.Filter(facts, req)

	if len(pass) != 0 {
		t.Errorf("expected no passing nodes; got %v", nodeNames(pass))
	}
	if len(rej) != 2 {
		t.Errorf("expected 2 rejections; got %v", rej)
	}
}

func TestFilter_PCIRequired_CheckerError_NodeRejectedFailSafe(t *testing.T) {
	t.Parallel()

	facts := []placement.NodeFacts{
		onlineFact("pve1"),
	}

	checkErr := errors.New("PVE API unavailable")
	checker := func(_ string) (bool, error) {
		return false, checkErr
	}

	req := placement.Request{
		RequiredPCIAddresses: []string{"0000:01:00.0"},
		PCIChecker:           checker,
	}
	pass, rej := placement.Filter(facts, req)

	if len(pass) != 0 {
		t.Errorf("expected no passing nodes on checker error; got %v", nodeNames(pass))
	}
	if rej["pve1"] == "" {
		t.Error("pve1 should be rejected when checker returns error")
	}
}

func TestFilter_NoPCIRequired_NoCheckerCalled(t *testing.T) {
	t.Parallel()

	facts := []placement.NodeFacts{
		onlineFact("pve1"),
		onlineFact("pve2"),
	}

	checkerCalled := false
	checker := func(_ string) (bool, error) {
		checkerCalled = true
		return false, nil
	}

	// RequiredPCIAddresses empty → checker must not be called.
	req := placement.Request{
		RequiredPCIAddresses: []string{},
		PCIChecker:           checker,
	}
	pass, _ := placement.Filter(facts, req)

	if checkerCalled {
		t.Error("PCIChecker should not be called when RequiredPCIAddresses is empty")
	}
	if len(pass) != 2 {
		t.Errorf("expected both nodes to pass; got %v", nodeNames(pass))
	}
}

func TestFilter_PCIRequired_NilChecker_NodePasses(t *testing.T) {
	t.Parallel()

	// When PCIChecker is nil but RequiredPCIAddresses is set, the PCI pass is
	// skipped (callers that cannot provide I/O should not hard-block placement).
	facts := []placement.NodeFacts{onlineFact("pve1")}
	req := placement.Request{
		RequiredPCIAddresses: []string{"0000:01:00.0"},
		PCIChecker:           nil,
	}
	pass, _ := placement.Filter(facts, req)
	if len(pass) != 1 {
		t.Errorf("expected pve1 to pass with nil checker; got %v", nodeNames(pass))
	}
}

func TestFilter_PCIRequired_OnlineCheckRunsFirst(t *testing.T) {
	t.Parallel()

	// Offline node should be rejected by online check before PCI checker fires.
	checkerCalled := false
	checker := func(_ string) (bool, error) {
		checkerCalled = true
		return true, nil
	}

	facts := []placement.NodeFacts{
		{Node: "pve1", Online: false, TotalMemBytes: 8 * gib, FreeMemBytes: 4 * gib, MaxCPU: 8},
	}
	req := placement.Request{
		RequiredPCIAddresses: []string{"0000:01:00.0"},
		PCIChecker:           checker,
	}
	placement.Filter(facts, req)

	if checkerCalled {
		t.Error("PCIChecker must not be called for an offline node")
	}
}
