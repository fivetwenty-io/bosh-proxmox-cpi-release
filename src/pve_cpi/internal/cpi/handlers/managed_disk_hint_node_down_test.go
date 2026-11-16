package handlers

import (
	"context"
	"strings"
	"testing"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// hintNodeDownCtx disables retry backoff, so a lookup that does retry the
// down member fails fast instead of sleeping out the curve.
func hintNodeDownCtx() context.Context {
	return pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
}

// TestManagedDiskHintNode_DownNodeDoesNotBlockHintElsewhere: create_disk
// reads the hinted VM's node on every planning and recheck step. With another
// member offline, the read still answers the VM's node on the online member,
// and it never lists the offline one, so it neither stalls on the listing's
// retries nor fails create_disk.
func TestManagedDiskHintNode_DownNodeDoesNotBlockHintElsewhere(t *testing.T) {
	m, _, state := managedDiskFixture(t, "spread", false)
	state.configs = map[int]map[string]any{321: {"name": "vm-321"}}
	state.downNode = "n2"

	node, err := managedDiskHintNode(hintNodeDownCtx(), m.deps, "321")
	if err != nil {
		t.Fatalf("a down member must not fail the hint lookup for a VM elsewhere, got: %v", err)
	}
	if node != "n1" {
		t.Fatalf("expected the hinted VM's node n1, got %q", node)
	}
	if state.downLists != 0 {
		t.Fatalf("the member the cluster reports offline must not be listed, got %d listing attempts", state.downLists)
	}
}

// TestManagedDiskHintNode_HintOnDownNodeFailsRetriable: a hinted VM found on
// no online member may live on the offline one, where no disk can be created
// for it. The lookup fails retriable and names the member, rather than
// reporting the VM as gone or spending the retry budget on the member first.
func TestManagedDiskHintNode_HintOnDownNodeFailsRetriable(t *testing.T) {
	m, _, state := managedDiskFixture(t, "spread", false)
	state.configs = map[int]map[string]any{}
	state.downNode = "n2"

	_, err := managedDiskHintNode(hintNodeDownCtx(), m.deps, "321")
	if err == nil {
		t.Fatal("a hinted VM that may be on the offline member must fail the lookup")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("the down-target error must be retriable, got: %v", err)
	}
	if !strings.Contains(err.Error(), "n2") || strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("the error must name the offline member and not call the VM gone, got: %v", err)
	}
	if state.downLists != 0 {
		t.Fatalf("the member the cluster reports offline must not be listed, got %d listing attempts", state.downLists)
	}
}

// TestManagedDiskHintNode_MissingHintWithEveryMemberListed: with nothing left
// out of the scan, a hinted VM found nowhere still reads as gone.
func TestManagedDiskHintNode_MissingHintWithEveryMemberListed(t *testing.T) {
	m, _, state := managedDiskFixture(t, "spread", false)
	state.configs = map[int]map[string]any{}

	_, err := managedDiskHintNode(hintNodeDownCtx(), m.deps, "321")
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a hinted VM found on no member must read as gone, got: %v", err)
	}
	if cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("a proven-missing hinted VM must stay permanent, got: %v", err)
	}
}
