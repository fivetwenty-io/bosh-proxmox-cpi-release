package placement_test

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/placement"
)

// TestEstimateHeadroom_GuestStillBeingCreatedIsLeftOut pins how the estimate
// treats a guest that another call is still creating. Under a parallel deploy
// a sibling's guest exists before PVE reports its memory, so it carries no
// figure yet. The estimate leaves such a guest out instead of making itself
// unavailable for the whole cluster, while a settled guest with no figure
// still makes it unavailable, and a guest still being created that already
// reports memory counts like any other.
func TestEstimateHeadroom_GuestStillBeingCreatedIsLeftOut(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{hrNode("pve1", 16), hrNode("pve2", 16)}
	all := []string{"pve1", "pve2"}

	t.Run("no memory yet", func(t *testing.T) {
		t.Parallel()
		creating := hrGuest(210, "pve1", 0, false)
		creating.Creating = true
		check := hrCheck(placement.HeadroomWarn, hrVM(1, 4, all...), creating)
		if got := placement.EstimateHeadroom(facts, *check, "pve1"); got.Short() {
			t.Fatalf("estimate = %v; want it to run and fit, leaving the guest out", got.Lines())
		}
		m := placement.EstimateHeadroomMargin(facts, []placement.HeadroomGuest{creating}, false)
		if m.Unavailable != "" || m.Percent != 100 {
			t.Fatalf("margin = %+v; want ok(100%%), leaving the guest out", m)
		}
	})
	t.Run("settled with no memory", func(t *testing.T) {
		t.Parallel()
		check := hrCheck(placement.HeadroomWarn, hrVM(1, 4, all...), hrGuest(210, "pve1", 0, false))
		got := placement.EstimateHeadroom(facts, *check, "pve1")
		if !strings.Contains(got.Unavailable, "guest 210 on pve1 has no configured memory") {
			t.Fatalf("estimate = %+v; want it unavailable for the settled guest", got)
		}
	})
	t.Run("memory already reported", func(t *testing.T) {
		t.Parallel()
		// 14 GiB on each node leaves 2 GiB free, so the 4 GiB VM fits on no
		// survivor once its node fails, and the guest still being created
		// counts toward that.
		creating := hrGuest(210, "pve2", 14, false)
		creating.Creating = true
		check := hrCheck(placement.HeadroomWarn, hrVM(1, 4, all...), hrGuest(211, "pve1", 10, false), creating)
		got := placement.EstimateHeadroom(facts, *check, "pve1")
		if got.Unavailable != "" || len(got.Shortfalls) == 0 {
			t.Fatalf("estimate = %+v; want a shortfall that counts the guest", got)
		}
	})
}
