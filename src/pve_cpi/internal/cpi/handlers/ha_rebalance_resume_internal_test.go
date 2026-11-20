package handlers

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/ha"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/harebalance"
)

// rbMarked gives VM vmid the mark of an earlier run that names node, with
// failback off.
func rbMarked(f *fakeRebalanceIO, vmid int, node string) {
	v := f.vms[vmid]
	v.comment, v.failback = ha.WithRebalanceMark(v.comment, node), false
}

func TestRebalanceResume_ADryRunListsTheAction(t *testing.T) {
	f := newFakeRebalanceIO()
	f.addVM(101, "pve02", 8)
	f.addVM(102, "pve01", 8)
	f.addVM(103, "pve01", 8)
	rbMarked(f, 101, "pve02")
	rbMarked(f, 102, "pve03")
	rbMarked(f, 103, "pve02")
	f.moving[103] = true
	rep := rbRun(f, RebalanceOptions{}).run(context.Background())
	got := make([]string, 0, len(rep.Resumed))
	for _, r := range rep.Resumed {
		got = append(got, r.Action+" "+string(r.Outcome))
	}
	// The VM PVE HA still moves fails the settled gate, so the dry run exits
	// with the gate code and still lists every marked VM.
	want := []string{"finish planned", "abort planned", "wait planned"}
	if !slices.Equal(got, want) || len(f.writes()) != 0 {
		t.Fatalf("resumed %v, want %v; writes %v", got, want, f.writes())
	}
	if rbSkipKind(rep.Skipped, 101) != harebalance.SkipRebalancePending {
		t.Fatalf("skipped %v", rep.Skipped)
	}
}

func TestRebalanceResume_ApplyFinishesAndAbortsBeforeItPlans(t *testing.T) {
	f := newFakeRebalanceIO()
	f.addVM(101, "pve02", 8)
	f.addVM(102, "pve01", 8)
	f.addVM(103, "pve01", 8)
	f.addVM(104, "pve01", 8)
	rbMarked(f, 101, "pve02")
	rbMarked(f, 102, "pve03")
	rep := rbRun(f, RebalanceOptions{Apply: true}).run(context.Background())
	if rep.ExitCode != RebalanceExitOK {
		t.Fatalf("exit %d halted %q", rep.ExitCode, rep.Halted)
	}
	if len(rep.Resumed) != 2 || rep.Resumed[0].Outcome != RebalanceFinished ||
		rep.Resumed[1].Outcome != RebalanceAborted {
		t.Fatalf("resumed %+v", rep.Resumed)
	}
	if !slices.Equal(f.unmarks[:2], []string{"101:true", "102:true"}) {
		t.Fatalf("unmarks %v", f.unmarks)
	}
	if f.vms[102].node != "pve01" && rep.Moves[0].VMID != 102 {
		t.Fatal("the abort moved the VM")
	}
	if len(rep.Moves) == 0 || rep.Count(RebalanceMoved) != len(rep.Moves) {
		t.Fatalf("the run did not plan after the resume: %v", rbMoves(rep.Moves))
	}
}

func TestRebalanceResume_AMarkThatCannotBeClearedStopsTheRun(t *testing.T) {
	f := newFakeRebalanceIO()
	rbSkewed(f)
	rbMarked(f, 101, "pve03")
	f.unmarkHook = func(int, bool) (bool, error) { return false, errors.New("500") }
	rep := rbRun(f, RebalanceOptions{Apply: true}).run(context.Background())
	if rep.ExitCode != RebalanceExitError || len(rep.Moves) != 0 || rep.Resumed[0].Outcome != RebalanceLeftPending {
		t.Fatalf("exit %d moves %v resumed %+v", rep.ExitCode, rep.Moves, rep.Resumed)
	}
	if !strings.Contains(rep.Halted, "101") || len(f.callsWith("migrate")) != 0 {
		t.Fatalf("halted %q calls %v", rep.Halted, f.calls)
	}
}

func TestRebalanceResume_ABusyVMStaysMarked(t *testing.T) {
	f := newFakeRebalanceIO()
	rbSkewed(f)
	rbMarked(f, 101, "pve01")
	f.claimErr[101] = errRebalanceBusy
	rep := rbRun(f, RebalanceOptions{Apply: true}).run(context.Background())
	if rep.ExitCode != RebalanceExitError || rep.Resumed[0].Outcome != RebalanceLeftPending {
		t.Fatalf("exit %d resumed %+v", rep.ExitCode, rep.Resumed)
	}
}

func TestRebalanceResume_AFinishWithAPinThatDoesNotNameTheTargetKeepsFailbackOff(t *testing.T) {
	f := newFakeRebalanceIO()
	f.addVM(101, "pve02", 8)
	f.pins[101] = rbPin(101, map[string]int{"pve01": ha.PinPriorityPlaced, "pve03": ha.PinPriorityPreferred})
	rbMarked(f, 101, "pve02")
	rep := rbRun(f, RebalanceOptions{Apply: true}).run(context.Background())
	if len(rep.Resumed) != 1 || rep.Resumed[0].Outcome != RebalanceFinished || rep.Resumed[0].Error == "" {
		t.Fatalf("resumed %+v", rep.Resumed)
	}
	if !slices.Equal(f.unmarks, []string{"101:false"}) || len(rep.Warnings) < 2 {
		t.Fatalf("unmarks %v warnings %v", f.unmarks, rep.Warnings)
	}
	if len(f.pinWrites) != 0 {
		t.Fatalf("pin writes %v", f.pinWrites)
	}
}

func TestRebalanceResume_AFinishWithNoPinOfThisDirectorKeepsFailbackOff(t *testing.T) {
	f := newFakeRebalanceIO()
	f.addVM(101, "pve02", 8)
	delete(f.pins, 101)
	rbMarked(f, 101, "pve02")
	rep := rbRun(f, RebalanceOptions{Apply: true}).run(context.Background())
	if len(rep.Resumed) != 1 || rep.Resumed[0].Outcome != RebalanceFinished ||
		!slices.Equal(f.unmarks, []string{"101:false"}) {
		t.Fatalf("resumed %+v unmarks %v", rep.Resumed, f.unmarks)
	}
}

func TestRebalanceMove_ThePinIsRewrittenUnderTheClaimBeforeTheMarkIsDropped(t *testing.T) {
	f := newFakeRebalanceIO()
	rbSkewed(f)
	r := rbRun(f, RebalanceOptions{Apply: true})
	ctx := context.Background()
	view, err := r.observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	newPin := rbPin(101, map[string]int{"pve01": 0, "pve02": 1, "pve03": 0})
	view.plans[101].rewrites["pve02"] = rebalancePinChange{d: &ha.VMDecision{}, pin: newPin}
	m := harebalance.Move{VMID: 101, From: "pve01", To: "pve02"}
	if code, why := r.makeMove(ctx, view, m); code != RebalanceExitOK {
		t.Fatalf("code %d: %s", code, why)
	}
	idx := slices.Index(f.calls, "migrate 101 pve01>pve02")
	want := []string{"migrate 101 pve01>pve02", "record 101", "claim 101", "pin 101", "unmark 101 failback=true",
		"unclaim 101", "unrecord 101"}
	if idx < 0 || !slices.Equal(f.calls[idx:idx+len(want)], want) {
		t.Fatalf("calls %v, want %v", f.calls[idx:], want)
	}
	if !r.report.Moves[0].PinRewritten || !ha.SameRule(f.pins[101], newPin) {
		t.Fatalf("move %+v pin %+v", r.report.Moves[0], f.pins[101])
	}
}

func TestRebalanceMove_APinWriteThatFailsKeepsTheMark(t *testing.T) {
	for name, pinErr := range map[string]error{"changed": errPinChanged, "failed": errors.New("rules lock held")} {
		t.Run(name, func(t *testing.T) {
			f := newFakeRebalanceIO()
			rbSkewed(f)
			f.pinErr = pinErr
			r := rbRun(f, RebalanceOptions{Apply: true})
			ctx := context.Background()
			view, err := r.observe(ctx)
			if err != nil {
				t.Fatal(err)
			}
			view.plans[101].rewrites["pve02"] = rebalancePinChange{d: &ha.VMDecision{},
				pin: rbPin(101, map[string]int{"pve01": 0, "pve02": 1, "pve03": 0})}
			code, _ := r.makeMove(ctx, view, harebalance.Move{VMID: 101, From: "pve01", To: "pve02"})
			if code != RebalanceExitError || r.report.Moves[0].Outcome != RebalanceLeftPending || len(f.unmarks) != 0 {
				t.Fatalf("code %d move %+v unmarks %v", code, r.report.Moves[0], f.unmarks)
			}
			if node, ok := ha.RebalancePending(&f.vms[101].comment); !ok || node != "pve02" {
				t.Fatalf("mark %q %v", node, ok)
			}
		})
	}
}

func TestRebalanceMove_AVMWithNoPlanIsRefused(t *testing.T) {
	f := newFakeRebalanceIO()
	r := rbRun(f, RebalanceOptions{Apply: true})
	view := &rebalanceView{plans: map[int]*rebalanceVMPlan{}}
	code, _ := r.makeMove(context.Background(), view, harebalance.Move{VMID: 101, From: "pve01", To: "pve02"})
	if code != RebalanceExitOK || r.report.Moves[0].Outcome != RebalanceRefused || r.excluded[101] == "" {
		t.Fatalf("code %d moves %+v", code, r.report.Moves)
	}
}

func TestRebalanceMove_TheHoldIsConfirmedRightBeforeTheMigration(t *testing.T) {
	cases := map[string]func(v *rbVM){
		"failback on again": func(v *rbVM) { v.failback = true },
		"mark gone":         func(v *rbVM) { v.comment, _ = ha.WithoutRebalanceMark(v.comment) },
		"mark names another node": func(v *rbVM) {
			c, _ := ha.WithoutRebalanceMark(v.comment)
			v.comment = ha.WithRebalanceMark(c, "pve03")
		},
		"resource gone": func(v *rbVM) { v.noResource = true },
	}
	for name, breakHold := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeRebalanceIO()
			rbSkewed(f)
			f.beforeMigrate = func(vmid int) {
				if vmid == 101 {
					breakHold(f.vms[101])
				}
			}
			rep := rbRun(f, RebalanceOptions{Apply: true, MaxMoves: 1}).run(context.Background())
			if rep.Moves[0].Outcome == RebalanceMoved || len(f.callsWith("migrate 101 pve01>pve02 not sent")) != 1 {
				t.Fatalf("moves %v calls %v", rbMoves(rep.Moves), f.calls)
			}
		})
	}
	t.Run("intact", func(t *testing.T) {
		f := newFakeRebalanceIO()
		rbSkewed(f)
		rbMarked(f, 101, "pve02")
		if err := rbRun(f, RebalanceOptions{}).confirmMark(context.Background(), 101, "pve02"); err != nil {
			t.Fatal(err)
		}
	})
}
