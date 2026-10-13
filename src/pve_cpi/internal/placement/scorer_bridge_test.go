package placement_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/placement"
)

func TestFilter_RequiredBridges_RejectsNodeMissingBridge(t *testing.T) {
	t.Parallel()

	facts := []placement.NodeFacts{onlineFact("pve1"), onlineFact("pve2")}
	req := placement.Request{
		RequiredBridges: []string{"vmbr7"},
		BridgeChecker: func(node, _ string) (bool, error) {
			return node == "pve1", nil
		},
	}
	pass, rej := placement.Filter(facts, req)

	if len(pass) != 1 || pass[0].Node != "pve1" {
		t.Fatalf("expected only pve1 to pass; got %v", nodeNames(pass))
	}
	if rej["pve2"] != "missing bridge vmbr7" {
		t.Errorf("pve2 reason = %q, want %q", rej["pve2"], "missing bridge vmbr7")
	}
}

func TestFilter_RequiredBridges_CheckerError_RejectsAsLookupFailure(t *testing.T) {
	t.Parallel()

	facts := []placement.NodeFacts{onlineFact("pve1")}
	req := placement.Request{
		RequiredBridges: []string{"vmbr0"},
		BridgeChecker: func(_, _ string) (bool, error) {
			return false, errors.New("boom")
		},
	}
	pass, rej := placement.Filter(facts, req)

	if len(pass) != 0 {
		t.Fatalf("expected no node to pass; got %v", nodeNames(pass))
	}
	if !strings.HasPrefix(rej["pve1"], "network lookup failed: ") || !strings.Contains(rej["pve1"], "boom") {
		t.Errorf("pve1 reason = %q, want a network lookup failed reason carrying the error", rej["pve1"])
	}
}

func TestFilter_RequiredBridges_NeedsEveryBridge(t *testing.T) {
	t.Parallel()

	facts := []placement.NodeFacts{onlineFact("pve1")}
	req := placement.Request{
		RequiredBridges: []string{"vmbr0", "vmbr9"},
		BridgeChecker: func(_, bridge string) (bool, error) {
			return bridge == "vmbr0", nil
		},
	}
	pass, rej := placement.Filter(facts, req)

	if len(pass) != 0 || rej["pve1"] != "missing bridge vmbr9" {
		t.Errorf("pass=%v rej=%v, want pve1 rejected for vmbr9", nodeNames(pass), rej)
	}
}

func TestFilter_RequiredBridges_NilCheckerOrEmptyList_NoOp(t *testing.T) {
	t.Parallel()

	facts := []placement.NodeFacts{onlineFact("pve1")}
	if pass, _ := placement.Filter(facts, placement.Request{RequiredBridges: []string{"vmbr0"}}); len(pass) != 1 {
		t.Errorf("nil checker must be a no-op; got %v", nodeNames(pass))
	}
	called := false
	req := placement.Request{BridgeChecker: func(_, _ string) (bool, error) { called = true; return false, nil }}
	if pass, _ := placement.Filter(facts, req); len(pass) != 1 || called {
		t.Errorf("empty bridge list must not call the checker (called=%v, pass=%v)", called, nodeNames(pass))
	}
}
