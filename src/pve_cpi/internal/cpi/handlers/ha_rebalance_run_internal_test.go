package handlers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/harebalance"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func TestRebalanceRun_DryRunPlansAndWritesNothing(t *testing.T) {
	f := newFakeRebalanceIO()
	rbSkewed(f)
	rep := rbRun(f, RebalanceOptions{}).run(context.Background())
	if rep.ExitCode != RebalanceExitOK || rep.Applied {
		t.Fatalf("exit %d applied %v halted %q", rep.ExitCode, rep.Applied, rep.Halted)
	}
	want := []string{"101 pve01>pve02 planned", "102 pve01>pve03 planned"}
	if got := rbMoves(rep.Moves); !slices.Equal(got, want) {
		t.Fatalf("moves %v, want %v", got, want)
	}
	if w := f.writes(); len(w) != 0 || len(f.callsWith("runlock")) != 0 || len(f.callsWith("states")) != 0 {
		t.Fatalf("a dry run took a lock, waited, or wrote: %v", f.calls)
	}
	if len(rep.Planned) != 0 || rep.SpreadBefore < 0.37 || rep.SpreadAfter > 0.13 {
		t.Fatalf("planned %v spread %v to %v", rep.Planned, rep.SpreadBefore, rep.SpreadAfter)
	}
}

// rbMoves renders moves as "vmid from>to outcome".
func rbMoves(moves []RebalanceMove) []string {
	out := make([]string, 0, len(moves))
	for i := range moves {
		m := &moves[i]
		out = append(out, fmt.Sprintf("%d %s>%s %s", m.VMID, m.From, m.To, m.Outcome))
	}
	return out
}

func TestRebalanceRun_ApplyMovesOneAtATimeInTheOrderOfTheDesign(t *testing.T) {
	f := newFakeRebalanceIO()
	rbSkewed(f)
	rep := rbRun(f, RebalanceOptions{Apply: true}).run(context.Background())
	if rep.ExitCode != RebalanceExitOK || rep.Halted != "" {
		t.Fatalf("exit %d halted %q", rep.ExitCode, rep.Halted)
	}
	if got, want := rbMoves(rep.Moves), []string{"101 pve01>pve02 moved", "102 pve01>pve03 moved"}; !slices.Equal(got,
		want) {
		t.Fatalf("moves %v, want %v", got, want)
	}
	if got := rbMoves(rep.Planned); len(got) != 2 || rep.DiffersFromPlan {
		t.Fatalf("planned %v differs %v", got, rep.DiffersFromPlan)
	}
	// One move: the hold under the record and the claim, the migration with
	// neither held, then failback and the mark under both again, and the
	// check that the VM stayed after two CRM rounds.
	first := f.calls[:slices.Index(f.calls, "snapshot")+1]
	if first[0] != "runlock" {
		t.Fatalf("the run lock is not taken first: %v", f.calls)
	}
	idx := slices.Index(f.calls, "record 101")
	want := []string{"record 101", "claim 101", "reread 101", "mark 101 pve02", "unclaim 101", "unrecord 101",
		"migrate 101 pve01>pve02", "record 101", "claim 101", "unmark 101 failback=true", "unclaim 101",
		"unrecord 101", "sleep 20s", "states", "snapshot"}
	if idx < 0 || !slices.Equal(f.calls[idx:idx+len(want)], want) {
		t.Fatalf("calls %v, want %v", f.calls[idx:], want)
	}
	if f.release != 1 || f.calls[len(f.calls)-1] != "release" {
		t.Fatalf("the run lock was released %d times: %v", f.release, f.calls)
	}
	for _, id := range []int{101, 102} {
		if _, ok := ha.RebalancePending(&f.vms[id].comment); ok || !f.vms[id].failback {
			t.Fatalf("VM %d keeps the mark or failback off: %+v", id, f.vms[id])
		}
	}
	if rep.Moves[0].Seconds != 20 {
		t.Fatalf("seconds %v", rep.Moves[0].Seconds)
	}
}

func TestRebalanceRun_AnUndecidedFailbackStaysOff(t *testing.T) {
	f := newFakeRebalanceIO()
	rbSkewed(f)
	f.want = vmFailbackWant{reason: "the profile can't decide"}
	rep := rbRun(f, RebalanceOptions{Apply: true, MaxMoves: 1}).run(context.Background())
	if rep.ExitCode != RebalanceExitOK || !slices.Equal(f.unmarks, []string{"101:false"}) {
		t.Fatalf("exit %d unmarks %v", rep.ExitCode, f.unmarks)
	}
	if rep.Deferred != 1 {
		t.Fatalf("deferred %d, want the one move past --max-moves", rep.Deferred)
	}
}

func TestRebalanceRun_RunLockHeldRefusesWithTheGateCode(t *testing.T) {
	f := newFakeRebalanceIO()
	rbSkewed(f)
	f.lockErr = pve.ErrClusterLockHeld
	rep := rbRun(f, RebalanceOptions{Apply: true}).run(context.Background())
	if rep.ExitCode != RebalanceExitGate || rep.Halted != RebalanceGateRunLock || len(f.callsWith("snapshot")) != 0 {
		t.Fatalf("exit %d halted %q calls %v", rep.ExitCode, rep.Halted, f.calls)
	}
	f = newFakeRebalanceIO()
	f.lockErr = errors.New("permission denied")
	rep = rbRun(f, RebalanceOptions{Apply: true}).run(context.Background())
	if rep.ExitCode != RebalanceExitError || !strings.Contains(rep.Halted, "permission denied") {
		t.Fatalf("exit %d halted %q", rep.ExitCode, rep.Halted)
	}
}

func TestRebalanceRun_AFailedGateStopsBeforeAnyWrite(t *testing.T) {
	for _, apply := range []bool{false, true} {
		f := newFakeRebalanceIO()
		rbSkewed(f)
		f.act.locks = []string{"bosh-lock-vm-200"}
		rep := rbRun(f, RebalanceOptions{Apply: apply}).run(context.Background())
		if rep.ExitCode != RebalanceExitGate || rep.Halted != RebalanceGateActivity || len(f.writes()) != 0 {
			t.Fatalf("apply %v: exit %d halted %q writes %v", apply, rep.ExitCode, rep.Halted, f.writes())
		}
		// A dry run still shows what it would do once the gate clears.
		if !apply && len(rep.Moves) != 2 {
			t.Fatalf("a dry run with a failed gate planned %v", rep.Moves)
		}
		if apply && (len(rep.Moves) != 0 || f.release != 1) {
			t.Fatalf("moves %v release %d", rep.Moves, f.release)
		}
	}
}

func TestRebalanceRun_GatesAreCheckedAgainBeforeEveryMove(t *testing.T) {
	f := newFakeRebalanceIO()
	rbSkewed(f)
	f.observeHook = func(n int) {
		if n == 2 {
			f.act.tasks = []string{"qmstart 300 on pve02"}
		}
	}
	rep := rbRun(f, RebalanceOptions{Apply: true}).run(context.Background())
	if rep.ExitCode != RebalanceExitGate || rep.Halted != RebalanceGateActivity || rep.Count(RebalanceMoved) != 1 {
		t.Fatalf("exit %d halted %q moves %v", rep.ExitCode, rep.Halted, rbMoves(rep.Moves))
	}
}

func TestRebalanceRun_AMoveThatCannotStartIsRefusedAndTheVMIsExcluded(t *testing.T) {
	cases := map[string]func(f *fakeRebalanceIO){
		"digest conflict on the hold": func(f *fakeRebalanceIO) {
			f.markHook = func(vmid int) (bool, error) { return vmid != 101, nil }
		},
		"claim held": func(f *fakeRebalanceIO) {
			f.claimErr[101] = fmt.Errorf("%w: another call claims the VM", errRebalanceBusy)
		},
		"record held": func(f *fakeRebalanceIO) {
			f.recordErr[101] = fmt.Errorf("%w: another call holds the record", errRebalanceBusy)
		},
		"record unmatched": func(f *fakeRebalanceIO) {
			f.recordErr[101] = &rebalanceSkip{reason: "the journal's records for the VM do not match its config"}
		},
		"pin changed": func(f *fakeRebalanceIO) {
			f.beforeReread = func(vmid int) {
				if vmid == 101 {
					f.pins[101] = rbPin(101, map[string]int{"pve01": 0, "pve02": 0})
				}
			}
		},
		"config lock": func(f *fakeRebalanceIO) {
			f.beforeReread = func(vmid int) {
				if vmid == 101 {
					f.vms[101].cfg["lock"] = "backup"
				}
			}
		},
		"volumes changed": func(f *fakeRebalanceIO) {
			f.beforeReread = func(vmid int) {
				if vmid == 101 {
					f.vms[101].cfg["scsi1"] = "nfs2:vm-101-disk-1,size=1G"
				}
			}
		},
		"resource stopped": func(f *fakeRebalanceIO) {
			f.beforeReread = func(vmid int) {
				if vmid == 101 {
					f.vms[101].state = "stopped"
				}
			}
		},
		"VM gone from its node": func(f *fakeRebalanceIO) {
			f.beforeReread = func(vmid int) {
				if vmid == 101 {
					f.vms[101].node = "pve03"
				}
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeRebalanceIO()
			rbSkewed(f)
			setup(f)
			rep := rbRun(f, RebalanceOptions{Apply: true}).run(context.Background())
			if rep.ExitCode != RebalanceExitOK {
				t.Fatalf("exit %d halted %q", rep.ExitCode, rep.Halted)
			}
			if rep.Moves[0].VMID != 101 || rep.Moves[0].Outcome != RebalanceRefused || rep.Moves[0].Error == "" {
				t.Fatalf("first move %+v", rep.Moves[0])
			}
			for _, w := range f.writes() {
				if strings.HasPrefix(w, "migrate 101") || w == "mark 101 pve02" {
					t.Fatalf("VM 101 was written: %v", f.writes())
				}
			}
			if !rep.DiffersFromPlan || rep.Count(RebalanceMoved) == 0 {
				t.Fatalf("the run did not go on with another VM: %v", rbMoves(rep.Moves))
			}
			if k := rbSkipKind(rep.Skipped, 101); k != harebalance.SkipRefused && f.vms[101].node == "pve01" {
				t.Fatalf("VM 101 is skipped as %q, want refused", k)
			}
		})
	}
}

func rbSkipKind(skips []RebalanceSkip, vmid int) harebalance.SkipKind {
	for i := range skips {
		if skips[i].VMID == vmid {
			return skips[i].Kind
		}
	}
	return ""
}

func TestRebalanceRun_AClaimNoVMCanTakeStopsTheRun(t *testing.T) {
	f := newFakeRebalanceIO()
	rbSkewed(f)
	f.claimErr[101] = &rebalanceHalt{reason: "PVE refused the VMID claim for lack of permission"}
	rep := rbRun(f, RebalanceOptions{Apply: true}).run(context.Background())
	if rep.ExitCode != RebalanceExitError || !strings.Contains(rep.Halted, "permission") || len(f.writes()) != 0 {
		t.Fatalf("exit %d halted %q writes %v", rep.ExitCode, rep.Halted, f.writes())
	}
}

func TestRebalanceRun_AHoldWriteThatFailsStopsTheRun(t *testing.T) {
	f := newFakeRebalanceIO()
	rbSkewed(f)
	f.markHook = func(int) (bool, error) { return false, errors.New("500 internal error") }
	rep := rbRun(f, RebalanceOptions{Apply: true}).run(context.Background())
	if rep.ExitCode != RebalanceExitError || len(f.callsWith("migrate")) != 0 || rep.Moves[0].Outcome != RebalanceRefused {
		t.Fatalf("exit %d halted %q calls %v", rep.ExitCode, rep.Halted, f.calls)
	}
}

// rbMoveFails makes the migration of VM 101 fail with failure, leaving it on
// node and moving when moving is set.
func rbMoveFails(f *fakeRebalanceIO, failure ha.MoveFailure, node string, moving bool) {
	f.migrateFn = func(_ context.Context, vmid int, _, _ string) (pve.VMLocation, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.vms[vmid].node = node
		f.moving[vmid] = moving
		return pve.VMLocation{}, &ha.MoveError{Failure: failure, Reason: "the move ended"}
	}
}

func TestRebalanceRun_MigrationFailures(t *testing.T) {
	cases := []struct {
		name    string
		failure ha.MoveFailure
		node    string
		moving  bool
		unmark  error
		outcome RebalanceOutcome
		exit    int
		// marked says the VM keeps the mark and failback off.
		marked bool
	}{
		{"refused, restored", ha.MoveRefused, "pve01", false, nil, RebalanceRefused, RebalanceExitOK, false},
		{"refused, restore fails", ha.MoveRefused, "pve01", false, errors.New("500"), RebalanceLeftPending,
			RebalanceExitError, true},
		{"node down", ha.MoveNodeDown, "pve01", false, nil, RebalanceFailed, RebalanceExitError, false},
		{"failed on the source", ha.MoveIncomplete, "pve01", false, nil, RebalanceFailed, RebalanceExitError, false},
		{"still migrating", ha.MoveIncomplete, "pve01", true, nil, RebalanceLeftPending, RebalanceExitError, true},
		{"landed late", ha.MoveIncomplete, "pve02", false, nil, RebalanceLeftPending, RebalanceExitError, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeRebalanceIO()
			rbSkewed(f)
			rbMoveFails(f, c.failure, c.node, c.moving)
			if c.unmark != nil {
				f.unmarkHook = func(int, bool) (bool, error) { return false, c.unmark }
			}
			rep := rbRun(f, RebalanceOptions{Apply: true}).run(context.Background())
			if rep.ExitCode != c.exit || rep.Moves[0].Outcome != c.outcome {
				t.Fatalf("exit %d halted %q moves %v", rep.ExitCode, rep.Halted, rbMoves(rep.Moves))
			}
			_, marked := ha.RebalancePending(&f.vms[101].comment)
			if marked != c.marked || f.vms[101].failback == c.marked {
				t.Fatalf("VM 101 marked %v failback %v, want marked %v", marked, f.vms[101].failback, c.marked)
			}
			if c.exit != RebalanceExitOK && rep.Halted == "" {
				t.Fatal("no reason for the halt")
			}
			if c.exit == RebalanceExitOK && (len(rep.Moves) < 2 || rbSkipKind(rep.Skipped, 101) != harebalance.SkipRefused) {
				t.Fatalf("a refused move did not exclude the VM and go on: %v %v", rbMoves(rep.Moves), rep.Skipped)
			}
		})
	}
}

func TestRebalanceRun_ASignalDuringTheMigrationLeavesTheMark(t *testing.T) {
	f := newFakeRebalanceIO()
	rbSkewed(f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.migrateFn = func(ctx context.Context, vmid int, _, _ string) (pve.VMLocation, error) {
		f.mu.Lock()
		f.moving[vmid] = true
		f.mu.Unlock()
		cancel()
		return pve.VMLocation{}, &ha.MoveError{Failure: ha.MoveIncomplete, Reason: "the wait ended", Err: ctx.Err()}
	}
	rep := rbRun(f, RebalanceOptions{Apply: true}).run(ctx)
	if rep.ExitCode != RebalanceExitError || rep.Moves[0].Outcome != RebalanceLeftPending || len(f.unmarks) != 0 {
		t.Fatalf("exit %d moves %v unmarks %v", rep.ExitCode, rbMoves(rep.Moves), f.unmarks)
	}
	if f.release != 1 {
		t.Fatal("the run lock was not released")
	}
}

func TestRebalanceRun_ASignalDuringTheSettleWaitStopsBeforeAnyWrite(t *testing.T) {
	f := newFakeRebalanceIO()
	rbSkewed(f)
	f.moving[101] = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.sleepHook = func(context.Context, time.Duration) error {
		cancel()
		return nil
	}
	rep := rbRun(f, RebalanceOptions{Apply: true}).run(ctx)
	if rep.ExitCode != RebalanceExitError || !strings.Contains(rep.Halted, "interrupted") || len(f.writes()) != 0 {
		t.Fatalf("exit %d halted %q writes %v", rep.ExitCode, rep.Halted, f.writes())
	}
	if f.release != 1 {
		t.Fatal("the run lock was not released")
	}
}

func TestRebalanceRun_TheSettleWait(t *testing.T) {
	t.Run("clears", func(t *testing.T) {
		f := newFakeRebalanceIO()
		rbSkewed(f)
		f.moving[101], f.settleAfter = true, 3
		rep := rbRun(f, RebalanceOptions{Apply: true}).run(context.Background())
		if rep.ExitCode != RebalanceExitOK || rep.Count(RebalanceMoved) != 2 || len(f.callsWith("sleep 5s")) != 2 {
			t.Fatalf("exit %d halted %q calls %v", rep.ExitCode, rep.Halted, f.calls)
		}
	})
	t.Run("runs out", func(t *testing.T) {
		f := newFakeRebalanceIO()
		rbSkewed(f)
		f.moving[101] = true
		rep := rbRun(f, RebalanceOptions{Apply: true, SettleTimeout: time.Minute}).run(context.Background())
		if rep.ExitCode != RebalanceExitGate || rep.Halted != RebalanceGateSettled || len(f.writes()) != 0 {
			t.Fatalf("exit %d halted %q", rep.ExitCode, rep.Halted)
		}
		if n := len(f.callsWith("sleep 5s")); n != 12 {
			t.Fatalf("%d polls in a minute", n)
		}
	})
}

func TestRebalanceRun_FailuresAfterTheVMLands(t *testing.T) {
	t.Run("failback restore fails", func(t *testing.T) {
		f := newFakeRebalanceIO()
		rbSkewed(f)
		f.unmarkHook = func(int, bool) (bool, error) { return false, errors.New("500") }
		rep := rbRun(f, RebalanceOptions{Apply: true}).run(context.Background())
		if rep.ExitCode != RebalanceExitError || rep.Moves[0].Outcome != RebalanceLeftPending ||
			f.vms[101].node != "pve02" || f.vms[101].failback {
			t.Fatalf("exit %d moves %v vm %+v", rep.ExitCode, rbMoves(rep.Moves), f.vms[101])
		}
		if node, ok := ha.RebalancePending(&f.vms[101].comment); !ok || node != "pve02" {
			t.Fatalf("the mark is %q %v", node, ok)
		}
	})
	t.Run("every restore loses the race", func(t *testing.T) {
		f := newFakeRebalanceIO()
		rbSkewed(f)
		f.unmarkHook = func(int, bool) (bool, error) { return false, nil }
		rep := rbRun(f, RebalanceOptions{Apply: true}).run(context.Background())
		if rep.ExitCode != RebalanceExitError || len(f.callsWith("unmark 101")) != rebalanceUnmarkAttempts {
			t.Fatalf("exit %d calls %v", rep.ExitCode, f.callsWith("unmark"))
		}
	})
	t.Run("a restore that loses one race tries again", func(t *testing.T) {
		f := newFakeRebalanceIO()
		rbSkewed(f)
		tries := 0
		f.unmarkHook = func(int, bool) (bool, error) {
			tries++
			return tries > 1, nil
		}
		rep := rbRun(f, RebalanceOptions{Apply: true, MaxMoves: 1}).run(context.Background())
		if rep.ExitCode != RebalanceExitOK || rep.Moves[0].Outcome != RebalanceMoved {
			t.Fatalf("exit %d moves %v", rep.ExitCode, rbMoves(rep.Moves))
		}
	})
	t.Run("the failback decision fails", func(t *testing.T) {
		f := newFakeRebalanceIO()
		rbSkewed(f)
		f.wantErr = errors.New("profile read failed")
		rep := rbRun(f, RebalanceOptions{Apply: true}).run(context.Background())
		if rep.ExitCode != RebalanceExitError || rep.Moves[0].Outcome != RebalanceLeftPending || len(f.unmarks) != 0 {
			t.Fatalf("exit %d moves %v", rep.ExitCode, rbMoves(rep.Moves))
		}
	})
	t.Run("the VM moves again", func(t *testing.T) {
		f := newFakeRebalanceIO()
		rbSkewed(f)
		f.afterConfirm = func(vmid int) (string, error) {
			if f.vms[vmid].node == "pve02" && len(f.unmarks) > 0 {
				return "pve01", nil
			}
			return "", nil
		}
		rep := rbRun(f, RebalanceOptions{Apply: true}).run(context.Background())
		if rep.ExitCode != RebalanceExitError || rep.Moves[0].Outcome != RebalanceFailed ||
			!strings.Contains(rep.Halted, "moved again") {
			t.Fatalf("exit %d halted %q moves %v", rep.ExitCode, rep.Halted, rbMoves(rep.Moves))
		}
	})
	t.Run("the check that the VM stayed fails", func(t *testing.T) {
		f := newFakeRebalanceIO()
		rbSkewed(f)
		f.afterConfirm = func(int) (string, error) {
			if len(f.unmarks) > 0 {
				return "", errors.New("cluster resources unreadable")
			}
			return "", nil
		}
		rep := rbRun(f, RebalanceOptions{Apply: true}).run(context.Background())
		if rep.ExitCode != RebalanceExitError || rep.Moves[0].Outcome != RebalanceMoved {
			t.Fatalf("exit %d moves %v", rep.ExitCode, rbMoves(rep.Moves))
		}
	})
}

func TestRebalanceRun_TheRunEndsAtMaxDuration(t *testing.T) {
	f := newFakeRebalanceIO()
	rbSkewed(f)
	rep := rbRun(f, RebalanceOptions{Apply: true, MaxDuration: 15 * time.Second}).run(context.Background())
	if rep.ExitCode != RebalanceExitOK || rep.Count(RebalanceMoved) != 1 || rep.Deferred != 1 {
		t.Fatalf("exit %d moves %v deferred %d", rep.ExitCode, rbMoves(rep.Moves), rep.Deferred)
	}
}

func TestRebalanceRun_ASignalAfterAMoveStopsTheRun(t *testing.T) {
	f := newFakeRebalanceIO()
	rbSkewed(f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.sleepHook = func(_ context.Context, d time.Duration) error {
		if d == rebalanceConfirmWait {
			cancel()
		}
		return nil
	}
	rep := rbRun(f, RebalanceOptions{Apply: true}).run(ctx)
	if rep.ExitCode != RebalanceExitError || rep.Count(RebalanceMoved) != 1 || !strings.Contains(rep.Halted,
		"interrupted") {
		t.Fatalf("exit %d halted %q moves %v", rep.ExitCode, rep.Halted, rbMoves(rep.Moves))
	}
}

func TestRebalanceRun_ABastionApplyWithNoJournalWarns(t *testing.T) {
	f := newFakeRebalanceIO()
	rep := rbRun(f, RebalanceOptions{Apply: true}).run(context.Background())
	if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "no allocation journal") {
		t.Fatalf("warnings %v", rep.Warnings)
	}
	rep = rbRun(newFakeRebalanceIO(), RebalanceOptions{}).run(context.Background())
	if len(rep.Warnings) != 0 {
		t.Fatalf("a dry run warned %v", rep.Warnings)
	}
}

func TestRebalanceRun_AFreshFailbackHoldKeepsFailbackOffAtTheRelease(t *testing.T) {
	f := newFakeRebalanceIO()
	f.addVM(101, "pve01", 8)
	f.addVM(102, "pve01", 8)
	f.addVM(103, "pve01", 8)
	// An attach holds failback on VM 101 while the VM moves.
	f.migrateFn = func(_ context.Context, vmid int, _, to string) (pve.VMLocation, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		v := f.vms[vmid]
		v.node = to
		if vmid == 101 {
			v.comment = ha.WithFailbackHold(v.comment, f.clock)
			f.digest++
		}
		return pve.VMLocation{Node: to, Found: true}, nil
	}
	rep := rbRun(f, RebalanceOptions{Apply: true, MaxMoves: 1}).run(context.Background())
	if rep.ExitCode != RebalanceExitOK {
		t.Fatalf("exit %d halted %q", rep.ExitCode, rep.Halted)
	}
	if !slices.Equal(f.unmarks, []string{"101:false"}) || f.vms[101].failback {
		t.Fatalf("unmarks %v failback %v", f.unmarks, f.vms[101].failback)
	}
	if _, held := ha.FailbackHoldOf(&f.vms[101].comment); !held {
		t.Fatalf("the release dropped the hold: %q", f.vms[101].comment)
	}
}
