package placement_test

import (
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/placement"
)

// hrNode builds an online node with total GiB of memory.
func hrNode(name string, totalGiB int64) placement.NodeFacts {
	return placement.NodeFacts{Node: name, Online: true, TotalMemBytes: totalGiB * gib}
}

// hrGuest builds a guest of memGiB on node.
func hrGuest(vmid int, node string, memGiB int64, ha bool, admissible ...string) placement.HeadroomGuest {
	return placement.HeadroomGuest{VMID: vmid, Node: node, MemBytes: memGiB * gib, HA: ha, Admissible: admissible}
}

func hrCheck(mode string, vm placement.HeadroomVM, guests ...placement.HeadroomGuest) *placement.HeadroomCheck {
	return &placement.HeadroomCheck{Mode: mode, VM: vm, Guests: guests}
}

func hrVM(vmid int, memGiB int64, admissible ...string) placement.HeadroomVM {
	return placement.HeadroomVM{VMID: vmid, MemBytes: memGiB * gib, Admissible: admissible}
}

func TestEstimateHeadroom_LastVMJustFits(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{hrNode("pve1", 32), hrNode("pve2", 32), hrNode("pve3", 32)}
	all := []string{"pve1", "pve2", "pve3"}
	guests := []placement.HeadroomGuest{
		hrGuest(101, "pve1", 16, true, all...),
		hrGuest(102, "pve2", 16, true, all...),
		hrGuest(103, "pve3", 16, true, all...),
	}
	// Each survivor holds one 16 GiB guest, so a 16 GiB VM on pve3 fits exactly
	// once the failed node's VMs spread over two survivors with 16 GiB free each.
	// The failed node carries the new VM and 103 (32 GiB), which uses both.
	check := hrCheck(placement.HeadroomWarn, hrVM(104, 16, all...), guests...)
	if got := placement.EstimateHeadroom(facts, *check, "pve3"); got.Short() {
		t.Fatalf("estimate short, want fits exactly: %v", got.Lines())
	}

	// One byte more on the new VM no longer fits any survivor, which has exactly
	// 16 GiB free.
	check.VM.MemBytes = 16*gib + 1
	got := placement.EstimateHeadroom(facts, *check, "pve3")
	if !got.Short() {
		t.Fatal("estimate fits, want short by one byte")
	}
	if len(got.Shortfalls) != 1 || got.Shortfalls[0].FailedNode != "pve3" {
		t.Fatalf("shortfalls = %+v, want one for pve3", got.Shortfalls)
	}
}

func TestEstimateHeadroom_ExactLineText(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{hrNode("pve1", 32), hrNode("pve2", 16), hrNode("pve3", 32)}
	all := []string{"pve1", "pve2", "pve3"}
	guests := []placement.HeadroomGuest{
		hrGuest(201, "pve1", 20, false),
		hrGuest(202, "pve2", 16, false),
	}
	check := hrCheck(placement.HeadroomWarn, hrVM(104, 16, all...), guests...)
	// pve1 has 12 GiB free, pve2 has none, and the 16 GiB VM on pve3 fits neither.
	got := placement.EstimateHeadroom(facts, *check, "pve3")
	if len(got.Shortfalls) != 1 {
		t.Fatalf("shortfalls = %+v, want 1", got.Shortfalls)
	}
	want := "ha: headroom estimate short after losing pve3: VM 104 (16384 MiB) fits on no surviving node in its pin, " +
		"largest free 12288 MiB on pve1"
	if line := got.Lines()[0]; line != want {
		t.Errorf("line = %q\nwant   %q", line, want)
	}
}

func TestEstimateHeadroom_OneLargeNode(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{hrNode("big", 128), hrNode("s1", 16), hrNode("s2", 16)}
	all := []string{"big", "s1", "s2"}
	// A 48 GiB VM on the large node cannot move to either 16 GiB survivor.
	check := hrCheck(placement.HeadroomEnforce, hrVM(300, 48, all...))
	got := placement.EstimateHeadroom(facts, *check, "big")
	if len(got.Shortfalls) != 1 || got.Shortfalls[0].FailedNode != "big" {
		t.Fatalf("shortfalls = %+v, want one for big", got.Shortfalls)
	}
	if s := got.Shortfalls[0]; s.LargestFreeBytes != 16*gib || s.LargestFreeNode != "s1" {
		t.Errorf("largest free = %d on %q, want 16 GiB on s1", s.LargestFreeBytes, s.LargestFreeNode)
	}

	// The same VM on a small node leaves the 128 GiB node able to take it, and
	// when the big node fails nothing HA is on it.
	if got := placement.EstimateHeadroom(facts, *check, "s1"); got.Short() {
		t.Errorf("VM on s1 short: %v", got.Lines())
	}
}

func TestEstimateHeadroom_OfflineNodes(t *testing.T) {
	t.Parallel()
	off := hrNode("pve3", 64)
	off.Online = false
	facts := []placement.NodeFacts{hrNode("pve1", 16), hrNode("pve2", 16), off}
	all := []string{"pve1", "pve2", "pve3"}

	// The offline node has the room, but it is no survivor, so the 12 GiB VM
	// fits only because pve2 keeps 12 GiB free.
	check := hrCheck(placement.HeadroomWarn, hrVM(10, 12, all...), hrGuest(11, "pve2", 4, false))
	if got := placement.EstimateHeadroom(facts, *check, "pve1"); got.Short() {
		t.Fatalf("short, want fits on pve2 after losing pve1? lines = %v", got.Lines())
	}
	// With pve2 full the offline node's memory does not help.
	check = hrCheck(placement.HeadroomWarn, hrVM(10, 12, all...), hrGuest(11, "pve2", 16, false))
	got := placement.EstimateHeadroom(facts, *check, "pve1")
	if !got.Short() {
		t.Fatal("estimate fits, want short because the offline node is no survivor")
	}
	for _, s := range got.Shortfalls {
		if s.FailedNode == "pve3" {
			t.Errorf("offline node was tried as a failing node: %+v", s)
		}
	}
	// The planned node must be online.
	if got := placement.EstimateHeadroom(facts, *check, "pve3"); got.Unavailable == "" {
		t.Error("planned offline node did not make the estimate unavailable")
	}
	if got := placement.EstimateHeadroom(facts, *check, "nope"); got.Unavailable == "" {
		t.Error("unknown planned node did not make the estimate unavailable")
	}
}

func TestEstimateHeadroom_MaintenanceNodeIsNotATarget(t *testing.T) {
	t.Parallel()
	maint := hrNode("pve2", 64)
	maint.InMaintenance = true
	facts := []placement.NodeFacts{hrNode("pve1", 16), maint}
	check := hrCheck(placement.HeadroomWarn, hrVM(10, 8, "pve1", "pve2"))
	got := placement.EstimateHeadroom(facts, *check, "pve1")
	if len(got.Shortfalls) != 1 || got.Shortfalls[0].FailedNode != "pve1" {
		t.Fatalf("shortfalls = %+v, want pve1 to fail with no target", got.Shortfalls)
	}
	if got.Shortfalls[0].LargestFreeNode != "" {
		t.Errorf("largest free node = %q, want none", got.Shortfalls[0].LargestFreeNode)
	}
	if line := got.Lines()[0]; !strings.Contains(line, "none of those nodes is online") {
		t.Errorf("line = %q, want the no-survivor wording", line)
	}
}

func TestEstimateHeadroom_NonHAGuestsFillSurvivors(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{hrNode("pve1", 32), hrNode("pve2", 32), hrNode("pve3", 32)}
	all := []string{"pve1", "pve2", "pve3"}
	// Aggregate free memory is plenty (the survivors hold 96 - 60 = 36 GiB with
	// the failed node's guests excluded), yet each survivor has under 10 GiB.
	guests := []placement.HeadroomGuest{
		hrGuest(401, "pve1", 26, false),
		hrGuest(402, "pve2", 26, false),
		hrGuest(403, "pve3", 4, true, all...),
	}
	check := hrCheck(placement.HeadroomEnforce, hrVM(404, 10, all...), guests...)
	got := placement.EstimateHeadroom(facts, *check, "pve3")
	if !got.Short() || got.Shortfalls[0].FailedNode != "pve3" {
		t.Fatalf("shortfalls = %+v, want pve3 short", got.Shortfalls)
	}
	if s := got.Shortfalls[0]; s.VMID != 404 {
		t.Errorf("shortfall VM = %d, want the largest, 404", s.VMID)
	}
}

func TestEstimateHeadroom_FitsInAggregateButNotInPin(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{hrNode("pve1", 32), hrNode("pve2", 32), hrNode("pve3", 32)}
	guests := []placement.HeadroomGuest{
		hrGuest(501, "pve1", 28, false),
		hrGuest(502, "pve2", 4, false),
	}
	// A 16 GiB VM that may run on pve1 or pve3 only: pve2 has 28 GiB free, but
	// the pin excludes it.
	check := hrCheck(placement.HeadroomWarn, hrVM(503, 16, "pve1", "pve3"), guests...)
	got := placement.EstimateHeadroom(facts, *check, "pve3")
	if !got.Short() {
		t.Fatal("estimate fits, want short because only pve2 has room and the pin excludes it")
	}
	if s := got.Shortfalls[0]; s.LargestFreeNode != "pve1" || s.LargestFreeBytes != 4*gib {
		t.Errorf("largest free = %d on %q, want 4 GiB on pve1", s.LargestFreeBytes, s.LargestFreeNode)
	}

	// Widening the pin to the node with room makes it fit.
	check = hrCheck(placement.HeadroomWarn, hrVM(503, 16, "pve1", "pve2", "pve3"), guests...)
	if got := placement.EstimateHeadroom(facts, *check, "pve3"); got.Short() {
		t.Errorf("widened pin still short: %v", got.Lines())
	}
}

func TestEstimateHeadroom_NonStrictAZCountsOtherAdmissibleNodes(t *testing.T) {
	t.Parallel()
	// The preferred AZ is pve1 and pve2, and both are full. A non-strict AZ keeps
	// pve3 in the hard admissible set, so the VM counts as fitting there.
	facts := []placement.NodeFacts{hrNode("pve1", 16), hrNode("pve2", 16), hrNode("pve3", 64), hrNode("pve4", 64)}
	guests := []placement.HeadroomGuest{
		hrGuest(601, "pve1", 16, false),
		hrGuest(602, "pve2", 16, false),
	}
	nonStrict := []string{"pve1", "pve2", "pve3", "pve4"}
	check := hrCheck(placement.HeadroomEnforce, hrVM(603, 24, nonStrict...), guests...)
	if got := placement.EstimateHeadroom(facts, *check, "pve1"); got.Short() {
		t.Errorf("non-strict AZ short: %v", got.Lines())
	}
	// A strict AZ narrows the same set to the full nodes, and it is short.
	strict := hrCheck(placement.HeadroomEnforce, hrVM(603, 24, "pve1", "pve2"), guests...)
	if got := placement.EstimateHeadroom(facts, *strict, "pve1"); !got.Short() {
		t.Error("strict AZ fits, want short")
	}
}

func TestEstimateHeadroom_LargestFirstPacking(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{hrNode("a", 8), hrNode("b", 4), hrNode("c", 64)}
	all := []string{"a", "b", "c"}
	guests := []placement.HeadroomGuest{hrGuest(701, "c", 4, true, all...)}
	// On c: 4 GiB (701) and the 8 GiB VM. Smallest-first would put 4 on a and
	// strand the 8; largest first puts 8 on a and 4 on b.
	check := hrCheck(placement.HeadroomEnforce, hrVM(702, 8, all...), guests...)
	if got := placement.EstimateHeadroom(facts, *check, "c"); got.Short() {
		t.Errorf("largest-first packing short: %v", got.Lines())
	}
}

func TestEstimateHeadroom_MoreThanOneFailure(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{hrNode("pve2", 16), hrNode("pve1", 16)}
	all := []string{"pve1", "pve2"}
	guests := []placement.HeadroomGuest{hrGuest(801, "pve1", 12, true, all...)}
	check := hrCheck(placement.HeadroomWarn, hrVM(802, 12, all...), guests...)
	got := placement.EstimateHeadroom(facts, *check, "pve2")
	if len(got.Shortfalls) != 2 || got.Shortfalls[0].FailedNode != "pve1" || got.Shortfalls[1].FailedNode != "pve2" {
		t.Fatalf("shortfalls = %+v, want pve1 then pve2", got.Shortfalls)
	}
}

func TestEstimateHeadroom_AdmissibleAndInputs(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{hrNode("pve1", 16), hrNode("pve2", 16)}

	t.Run("empty admissible set fits nowhere", func(t *testing.T) {
		t.Parallel()
		check := hrCheck(placement.HeadroomWarn, hrVM(1, 4))
		if got := placement.EstimateHeadroom(facts, *check, "pve1"); !got.Short() {
			t.Error("empty admissible set fits, want short")
		}
	})
	t.Run("admissible for a planned node", func(t *testing.T) {
		t.Parallel()
		vm := hrVM(1, 4, "pve1")
		vm.AdmissibleFor = func(node string) []string {
			if node == "pve1" {
				return []string{"pve1", "pve2"}
			}
			return []string{node}
		}
		check := hrCheck(placement.HeadroomWarn, vm)
		if got := placement.EstimateHeadroom(facts, *check, "pve1"); got.Short() {
			t.Errorf("pve1 short: %v", got.Lines())
		}
		if got := placement.EstimateHeadroom(facts, *check, "pve2"); !got.Short() {
			t.Error("pve2 fits, want short because its set is itself")
		}
	})
	t.Run("a guest with the VM's VMID is counted once", func(t *testing.T) {
		t.Parallel()
		all := []string{"pve1", "pve2"}
		check := hrCheck(placement.HeadroomWarn, hrVM(7, 10, all...), hrGuest(7, "pve1", 10, true, all...))
		if got := placement.EstimateHeadroom(facts, *check, "pve2"); got.Short() {
			t.Errorf("duplicate VMID short: %v", got.Lines())
		}
	})
	t.Run("a VM without memory is unavailable", func(t *testing.T) {
		t.Parallel()
		check := hrCheck(placement.HeadroomWarn, placement.HeadroomVM{VMID: 1, Admissible: []string{"pve1"}})
		got := placement.EstimateHeadroom(facts, *check, "pve1")
		if got.Unavailable == "" || !got.Short() {
			t.Errorf("result = %+v, want unavailable and short", got)
		}
		if lines := got.Lines(); len(lines) != 1 || !strings.HasPrefix(lines[0], "ha: headroom estimate unavailable: ") {
			t.Errorf("lines = %v", lines)
		}
	})
	t.Run("unreadable guests are unavailable", func(t *testing.T) {
		t.Parallel()
		check := hrCheck(placement.HeadroomWarn, hrVM(1, 4, "pve1"))
		check.GuestsUnknown = true
		if got := placement.EstimateHeadroom(facts, *check, "pve1"); got.Unavailable == "" {
			t.Error("unreadable guests did not make the estimate unavailable")
		}
	})
	t.Run("a guest without positive memory is unavailable", func(t *testing.T) {
		t.Parallel()
		for _, mem := range []int64{0, -gib} {
			check := hrCheck(placement.HeadroomWarn, hrVM(1, 4, "pve1", "pve2"),
				placement.HeadroomGuest{VMID: 9, Node: "pve2", MemBytes: mem, HA: true, Admissible: []string{"pve1", "pve2"}})
			got := placement.EstimateHeadroom(facts, *check, "pve1")
			if got.Unavailable == "" || !got.Short() || len(got.Shortfalls) != 0 {
				t.Errorf("memory %d: result = %+v, want unavailable and short", mem, got)
			}
			if !strings.Contains(got.Unavailable, "guest 9 on pve2") {
				t.Errorf("memory %d: reason %q does not name the guest", mem, got.Unavailable)
			}
		}
	})
	t.Run("a guest on an offline node without memory is ignored", func(t *testing.T) {
		t.Parallel()
		down := placement.NodeFacts{Node: "pve3", Online: false, TotalMemBytes: 16 * gib}
		check := hrCheck(placement.HeadroomWarn, hrVM(1, 4, "pve1", "pve2"),
			placement.HeadroomGuest{VMID: 9, Node: "pve3", HA: true, Admissible: []string{"pve1", "pve2"}})
		got := placement.EstimateHeadroom(append([]placement.NodeFacts{down}, facts...), *check, "pve1")
		if got.Unavailable != "" {
			t.Errorf("unavailable: %s", got.Unavailable)
		}
	})
	t.Run("an overcommitted survivor reports zero free", func(t *testing.T) {
		t.Parallel()
		check := hrCheck(placement.HeadroomWarn, hrVM(1, 4, "pve1", "pve2"), hrGuest(2, "pve2", 40, false))
		got := placement.EstimateHeadroom(facts, *check, "pve1")
		if len(got.Shortfalls) != 1 || got.Shortfalls[0].LargestFreeBytes != 0 {
			t.Errorf("shortfalls = %+v, want zero free", got.Shortfalls)
		}
	})
	t.Run("a fractional MiB VM rounds up and free rounds down", func(t *testing.T) {
		t.Parallel()
		s := placement.HeadroomShortfall{FailedNode: "n", VMID: 1, MemBytes: 1024*1024 + 1,
			LargestFreeBytes: 2*1024*1024 - 1, LargestFreeNode: "m"}
		if want := "VM 1 (2 MiB)"; !strings.Contains(s.Line(), want) {
			t.Errorf("line = %q, want %q", s.Line(), want)
		}
		if want := "largest free 1 MiB on m"; !strings.Contains(s.Line(), want) {
			t.Errorf("line = %q, want %q", s.Line(), want)
		}
	})
	t.Run("a duplicate name in the set counts once", func(t *testing.T) {
		t.Parallel()
		check := hrCheck(placement.HeadroomWarn, hrVM(1, 8, "pve1", "pve2", "pve2"))
		if got := placement.EstimateHeadroom(facts, *check, "pve1"); got.Short() {
			t.Errorf("short: %v", got.Lines())
		}
	})
}

func TestEnforceHeadroom_Modes(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{hrNode("pve1", 16), hrNode("pve2", 16)}
	all := []string{"pve1", "pve2"}
	guests := []placement.HeadroomGuest{hrGuest(2, "pve2", 16, false)}
	short := func(mode string) *placement.HeadroomCheck {
		return hrCheck(mode, hrVM(1, 8, all...), guests...)
	}

	t.Run("nil check", func(t *testing.T) {
		t.Parallel()
		logger, obs := log.NewObservedLogger(slog.LevelDebug)
		if err := placement.EnforceHeadroom(logger, facts, nil, "pve1"); err != nil || obs.Len() != 0 {
			t.Errorf("err = %v, log entries = %d; want none", err, obs.Len())
		}
	})
	for _, mode := range []string{"", placement.HeadroomOff} {
		t.Run("off "+mode, func(t *testing.T) {
			t.Parallel()
			logger, obs := log.NewObservedLogger(slog.LevelDebug)
			if err := placement.EnforceHeadroom(logger, facts, short(mode), "pve1"); err != nil || obs.Len() != 0 {
				t.Errorf("err = %v, log entries = %d; want none", err, obs.Len())
			}
		})
	}
	t.Run("warn logs and places", func(t *testing.T) {
		t.Parallel()
		logger, obs := log.NewObservedLogger(slog.LevelDebug)
		if err := placement.EnforceHeadroom(logger, facts, short(placement.HeadroomWarn), "pve1"); err != nil {
			t.Fatalf("warn returned %v", err)
		}
		entries := obs.All()
		if len(entries) != 1 || entries[0].Level != slog.LevelWarn {
			t.Fatalf("entries = %+v, want one warning", entries)
		}
		want := "ha: headroom estimate short after losing pve1: VM 1 (8192 MiB) fits on no surviving node in its pin, " +
			"largest free 0 MiB on pve2"
		if entries[0].Message != want {
			t.Errorf("message = %q\nwant      %q", entries[0].Message, want)
		}
	})
	t.Run("warn is quiet when there is room", func(t *testing.T) {
		t.Parallel()
		logger, obs := log.NewObservedLogger(slog.LevelDebug)
		check := hrCheck(placement.HeadroomWarn, hrVM(1, 8, all...))
		if err := placement.EnforceHeadroom(logger, facts, check, "pve1"); err != nil || obs.Len() != 0 {
			t.Errorf("err = %v, log entries = %d; want none", err, obs.Len())
		}
	})
	for _, mode := range []string{placement.HeadroomEnforce, "strict"} {
		t.Run("refuse "+mode, func(t *testing.T) {
			t.Parallel()
			logger, obs := log.NewObservedLogger(slog.LevelDebug)
			err := placement.EnforceHeadroom(logger, facts, short(mode), "pve1")
			if !errors.Is(err, placement.ErrHeadroomShort) {
				t.Fatalf("err = %v, want ErrHeadroomShort", err)
			}
			if !strings.Contains(err.Error(), "VM 1 (8192 MiB) fits on no surviving node in its pin, largest free 0 MiB on pve2") {
				t.Errorf("error %q lacks the estimate numbers", err)
			}
			if obs.Len() != 0 {
				t.Errorf("enforce logged %d entries, want none", obs.Len())
			}
		})
	}
	t.Run("an unavailable estimate is refused in enforce and warned in warn", func(t *testing.T) {
		t.Parallel()
		logger, obs := log.NewObservedLogger(slog.LevelDebug)
		check := short(placement.HeadroomEnforce)
		check.GuestsUnknown = true
		if err := placement.EnforceHeadroom(logger, facts, check, "pve1"); !errors.Is(err, placement.ErrHeadroomShort) {
			t.Errorf("enforce err = %v, want ErrHeadroomShort", err)
		}
		check.Mode = placement.HeadroomWarn
		if err := placement.EnforceHeadroom(logger, facts, check, "pve1"); err != nil || obs.Len() != 1 {
			t.Errorf("warn err = %v, entries = %d; want nil and 1", err, obs.Len())
		}
	})
}

func TestEnforceHeadroom_NilLogger(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{hrNode("pve1", 16), hrNode("pve2", 16)}
	all := []string{"pve1", "pve2"}
	guests := []placement.HeadroomGuest{hrGuest(2, "pve2", 16, false)}
	for _, mode := range []string{"", placement.HeadroomOff, placement.HeadroomWarn, placement.HeadroomEnforce} {
		t.Run("mode "+mode, func(t *testing.T) {
			t.Parallel()
			err := placement.EnforceHeadroom(nil, facts, hrCheck(mode, hrVM(1, 8, all...), guests...), "pve1")
			if refused := mode == placement.HeadroomEnforce; (err != nil) != refused {
				t.Errorf("err = %v, want refusal %v", err, refused)
			}
		})
	}
	t.Run("warn with an unavailable estimate", func(t *testing.T) {
		t.Parallel()
		check := hrCheck(placement.HeadroomWarn, hrVM(1, 8, all...), guests...)
		check.GuestsUnknown = true
		if err := placement.EnforceHeadroom(nil, facts, check, "pve1"); err != nil {
			t.Errorf("err = %v", err)
		}
	})
}

func TestEnforceHeadroom_UnavailableIsTransient(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{hrNode("pve1", 16), hrNode("pve2", 16)}
	all := []string{"pve1", "pve2"}
	check := hrCheck(placement.HeadroomEnforce, hrVM(1, 8, all...), hrGuest(2, "pve2", 16, false))
	if err := placement.EnforceHeadroom(nil, facts, check, "pve1"); errors.Is(err, placement.ErrHeadroomUnavailable) {
		t.Errorf("a shortfall wraps ErrHeadroomUnavailable: %v", err)
	}
	check.GuestsUnknown = true
	err := placement.EnforceHeadroom(nil, facts, check, "pve1")
	if !errors.Is(err, placement.ErrHeadroomUnavailable) || !errors.Is(err, placement.ErrHeadroomShort) {
		t.Errorf("err = %v, want ErrHeadroomUnavailable and ErrHeadroomShort", err)
	}
}

func TestFilter_HeadroomUnavailableReason(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{hrNode("pve1", 16), hrNode("pve2", 16)}
	all := []string{"pve1", "pve2"}
	cases := map[string]*placement.HeadroomCheck{
		"unreadable guests": func() *placement.HeadroomCheck {
			c := hrCheck(placement.HeadroomEnforce, hrVM(1, 4, all...))
			c.GuestsUnknown = true
			return c
		}(),
		"VM without memory": hrCheck(placement.HeadroomEnforce, placement.HeadroomVM{VMID: 1, Admissible: all}),
		"guest without memory": hrCheck(placement.HeadroomEnforce, hrVM(1, 4, all...),
			placement.HeadroomGuest{VMID: 2, Node: "pve2", HA: false}),
	}
	for name, hc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pass, rej := placement.Filter(facts, placement.Request{Headroom: hc})
			if len(pass) != 0 {
				t.Errorf("pass = %v, want none in enforce mode", pass)
			}
			for _, n := range []string{"pve1", "pve2"} {
				if !strings.HasPrefix(rej[n], placement.HeadroomUnavailablePrefix) ||
					strings.HasPrefix(rej[n], placement.HeadroomShortPrefix) {
					t.Errorf("rejection for %s = %q, want the unavailable prefix", n, rej[n])
				}
			}
		})
	}
	t.Run("a shortfall keeps the short prefix", func(t *testing.T) {
		t.Parallel()
		hc := hrCheck(placement.HeadroomEnforce, hrVM(1, 8, all...), hrGuest(2, "pve2", 16, false))
		_, rej := placement.Filter(facts, placement.Request{Headroom: hc})
		if !strings.HasPrefix(rej["pve1"], placement.HeadroomShortPrefix) {
			t.Errorf("rejection for pve1 = %q", rej["pve1"])
		}
	})
}

func TestFilter_Headroom(t *testing.T) {
	t.Parallel()
	facts := []placement.NodeFacts{hrNode("pve1", 16), hrNode("pve2", 16), hrNode("pve3", 16)}
	all := []string{"pve1", "pve2", "pve3"}
	// pve2 and pve3 each hold 12 GiB of non-HA guests, so they have 4 GiB free. An
	// 8 GiB VM placed on pve1 has nowhere to go when pve1 fails, while the same VM
	// on pve2 or pve3 can move to the empty pve1.
	guests := []placement.HeadroomGuest{
		hrGuest(2, "pve2", 12, false),
		hrGuest(3, "pve3", 12, false),
	}

	t.Run("enforce rejects the short node only", func(t *testing.T) {
		t.Parallel()
		req := placement.Request{Headroom: hrCheck(placement.HeadroomEnforce, hrVM(1, 8, all...), guests...)}
		pass, rej := placement.Filter(facts, req)
		if len(pass) != 2 || pass[0].Node != "pve2" || pass[1].Node != "pve3" {
			t.Fatalf("pass = %v, want pve2 and pve3", pass)
		}
		if want := "headroom short: ha: headroom estimate short after losing pve1: VM 1 (8192 MiB) fits on no " +
			"surviving node in its pin, largest free 4096 MiB on pve2"; rej["pve1"] != want {
			t.Errorf("rejection for pve1 = %q\nwant %q", rej["pve1"], want)
		}
	})
	t.Run("enforce rejects a VM pinned to the node that would fail", func(t *testing.T) {
		t.Parallel()
		req := placement.Request{Headroom: hrCheck(placement.HeadroomEnforce, hrVM(1, 8, "pve1"), guests...)}
		pass, rej := placement.Filter(facts, req)
		for _, f := range pass {
			if f.Node == "pve1" {
				t.Errorf("pve1 passed, rejections = %v", rej)
			}
		}
		if !strings.HasPrefix(rej["pve1"], "headroom short: ") {
			t.Errorf("rejection for pve1 = %q", rej["pve1"])
		}
	})
	t.Run("warn never removes a node", func(t *testing.T) {
		t.Parallel()
		req := placement.Request{Headroom: hrCheck(placement.HeadroomWarn, hrVM(1, 8, all...), guests...)}
		pass, rej := placement.Filter(facts, req)
		if len(pass) != 3 || rej != nil {
			t.Errorf("pass = %v, rejections = %v; want all three and none", pass, rej)
		}
	})
	t.Run("off and nil leave the filter alone", func(t *testing.T) {
		t.Parallel()
		for _, hc := range []*placement.HeadroomCheck{nil, hrCheck(placement.HeadroomOff, hrVM(1, 8), guests...)} {
			pass, rej := placement.Filter(facts, placement.Request{Headroom: hc})
			if len(pass) != 3 || rej != nil {
				t.Errorf("pass = %v, rejections = %v; want all three and none", pass, rej)
			}
		}
	})
	t.Run("an earlier rejection is not overwritten", func(t *testing.T) {
		t.Parallel()
		req := placement.Request{
			CandidateNodes: []string{"pve1"},
			Headroom:       hrCheck(placement.HeadroomEnforce, hrVM(1, 8, all...), guests...),
		}
		_, rej := placement.Filter(facts, req)
		if rej["pve2"] != "not in candidate node set" {
			t.Errorf("pve2 rejection = %q, want the candidate-set reason", rej["pve2"])
		}
	})
}
