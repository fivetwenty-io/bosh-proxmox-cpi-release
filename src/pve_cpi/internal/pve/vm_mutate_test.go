package pve

// MutateVMConfig protocol tests. They reuse the fake clock and the scripted
// cluster scan from vm_settle_test.go, and they inject every race the
// protocol exists for: a migration that starts after the settle wait and
// before the write lands, a held transport error on either side of the write,
// a config that moved under a stale index, and a source parker that moves
// while the target stays put.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

func lockedErr(vmid int, lock string) error {
	return sdkerrors.ParseAPIError(500, []byte(fmt.Sprintf(`{"message":"VM %d is locked (%s)\n"}`, vmid, lock)))
}

func configMissingErr(node string, vmid int) error {
	return sdkerrors.ParseAPIError(500, []byte(fmt.Sprintf(
		`{"message":"Configuration file 'nodes/%s/qemu-server/%d.conf' does not exist\n"}`, node, vmid)))
}

func heldTransportErr() error {
	return &sdkerrors.ConnectionError{Host: "pve-node1", Port: 8006, Message: "connection reset by peer"}
}

// recordingApply returns an Apply that records the nodes it was sent to and
// answers with the result step returns for each attempt.
func recordingApply(nodes *[]string, step func(attempt int, node string) error) func(context.Context, string) error {
	return func(_ context.Context, node string) error {
		*nodes = append(*nodes, node)
		return step(len(*nodes), node)
	}
}

func TestMutateVMConfig_LandsOnTheCallersNode(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 8001, func(time.Duration) *scanRow { return &scanRow{node: "pve-node1"} })
	var sent []string
	loc, err := MutateVMConfig(clk.ctx(context.Background()), c, nil, VMConfigMutation{
		VMID: 8001, Node: "pve-node1",
		Apply: recordingApply(&sent, func(int, string) error { return nil }),
	})
	if err != nil || loc.Node != "pve-node1" {
		t.Fatalf("loc, err = %+v, %v; want pve-node1", loc, err)
	}
	if len(sent) != 1 || clk.sleeps != 0 {
		t.Errorf("sent %v with %d sleeps; want one send and no wait", sent, clk.sleeps)
	}
}

// The settle wait returns with the VM on pve-node1, and HA starts a migration
// before the write lands. PVE refuses it for the migrate lock, which arrives
// as a 500, so the helper waits out the migration and sends to pve-node2.
func TestMutateVMConfig_MigrationStartsAfterTheWait(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	migrating := false
	var migrationStart time.Duration
	c := scriptedScanClient(clk, 8002, func(e time.Duration) *scanRow {
		switch {
		case !migrating:
			return &scanRow{node: "pve-node1", haState: "started"}
		case e-migrationStart < 4*time.Minute:
			return &scanRow{node: "pve-node1", lock: "migrate", haState: "migrate"}
		default:
			return &scanRow{node: "pve-node2", haState: "started"}
		}
	})
	var sent []string
	var reresolved []string
	loc, err := MutateVMConfig(clk.ctx(context.Background()), c, nil, VMConfigMutation{
		VMID:  8002,
		Label: "attach_disk",
		Apply: recordingApply(&sent, func(attempt int, node string) error {
			if attempt == 1 {
				migrating, migrationStart = true, clk.Elapsed()
				return lockedErr(8002, "migrate")
			}
			if node != "pve-node2" {
				return fmt.Errorf("sent to %s while the VM is on pve-node2", node)
			}
			return nil
		}),
		Reresolve: func(_ context.Context, vm VMLocation) error {
			reresolved = append(reresolved, vm.Node)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loc.Node != "pve-node2" {
		t.Errorf("landed on %q; want pve-node2", loc.Node)
	}
	if strings.Join(sent, ",") != "pve-node1,pve-node2" {
		t.Errorf("sent to %v; want pve-node1 then pve-node2", sent)
	}
	if strings.Join(reresolved, ",") != "pve-node2" {
		t.Errorf("re-resolved %v; want the other identities checked against pve-node2", reresolved)
	}
}

// The index still names the old node after the migration's config rename, so
// the settle wait sees no lock on pve-node1. Only the config probe can say
// where the VM went.
func TestMutateVMConfig_ConfigMissingUnderAStaleIndex(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 8003, func(time.Duration) *scanRow { return &scanRow{node: "pve-node1"} })
	c.qemuSvc = &fakeQEMUService{configFn: func(_ context.Context, node string, vmid int) (map[string]any, error) {
		if node == "pve-node2" {
			return map[string]any{"name": "vm"}, nil
		}
		return nil, configMissingErr(node, vmid)
	}}
	var sent []string
	loc, err := MutateVMConfig(clk.ctx(context.Background()), c, nil, VMConfigMutation{
		VMID: 8003, Node: "pve-node1",
		Apply: recordingApply(&sent, func(_ int, node string) error {
			if node == "pve-node1" {
				return configMissingErr(node, 8003)
			}
			return nil
		}),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loc.Node != "pve-node2" || !loc.FromConfig {
		t.Errorf("loc = %+v; want the probe's pve-node2", loc)
	}
	if strings.Join(sent, ",") != "pve-node1,pve-node2" {
		t.Errorf("sent to %v", sent)
	}
}

func TestMutateVMConfig_ConfigMissingEverywhereIsNotAbsence(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 8004, func(time.Duration) *scanRow { return &scanRow{node: "pve-node1"} })
	var sent []string
	_, err := MutateVMConfig(clk.ctx(context.Background()), c, nil, VMConfigMutation{
		VMID: 8004, Node: "pve-node1",
		Apply: recordingApply(&sent, func(_ int, node string) error { return configMissingErr(node, 8004) }),
	})
	requireSettleRetriable(t, err, "8004", "absence is not proven")
	if len(sent) != 1 {
		t.Errorf("sent %v; a VM found nowhere must not be sent the change again", sent)
	}
}

func TestMutateVMConfig_LockThatNeverClears(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 8005, func(time.Duration) *scanRow {
		return &scanRow{node: "pve-node1", lock: "create"}
	})
	var sent []string
	_, err := MutateVMConfig(clk.ctx(context.Background()), c, nil, VMConfigMutation{
		VMID: 8005, Node: "pve-node1",
		Apply: recordingApply(&sent, func(int, string) error { return lockedErr(8005, "create") }),
	})
	requireSettleRetriable(t, err, "locked (create)", `"pve-node1"`)
	if len(sent) != 1 {
		t.Errorf("sent %v; the lock never cleared, so the change goes out once", sent)
	}
	if e := clk.Elapsed(); e != settleOtherLockBudget {
		t.Errorf("waited %v; want the %v budget for a create lock", e, settleOtherLockBudget)
	}
}

// A VM whose lock keeps coming back is bounded by the protocol's own budget.
// Without a context deadline that is twice the motion budget.
func TestMutateVMConfig_IdentityThatKeepsMovingRunsOutOfBudget(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 8006, func(time.Duration) *scanRow { return &scanRow{node: "pve-node1"} })
	var sent []string
	_, err := MutateVMConfig(clk.ctx(context.Background()), c, nil, VMConfigMutation{
		VMID: 8006, Node: "pve-node1", MotionWait: time.Minute,
		Apply: recordingApply(&sent, func(int, string) error { return lockedErr(8006, "migrate") }),
		Reresolve: func(ctx context.Context, _ VMLocation) error {
			// Each round costs half a minute of the protocol's two minutes.
			return clk.Sleep(ctx, 30*time.Second)
		},
	})
	requireSettleRetriable(t, err, "budget ran out", "locked (migrate)")
	if len(sent) != 5 {
		t.Errorf("sent %d times; want 5 inside the 2m budget", len(sent))
	}
}

func TestMutateVMConfig_RoundBound(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 8007, func(time.Duration) *scanRow { return &scanRow{node: "pve-node1"} })
	var sent []string
	_, err := MutateVMConfig(clk.ctx(context.Background()), c, nil, VMConfigMutation{
		VMID: 8007, Node: "pve-node1",
		Apply: recordingApply(&sent, func(int, string) error { return lockedErr(8007, "backup") }),
	})
	requireSettleRetriable(t, err, "budget ran out", "locked (backup)")
	if len(sent) != mutateMaxRounds+1 {
		t.Errorf("sent %d times; want %d", len(sent), mutateMaxRounds+1)
	}
}

func TestMutateVMConfig_HeldTransportErrorWhereTheChangeLanded(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 8008, func(time.Duration) *scanRow { return &scanRow{node: "pve-node1"} })
	var sent, checked []string
	loc, err := MutateVMConfig(clk.ctx(context.Background()), c, nil, VMConfigMutation{
		VMID: 8008, Node: "pve-node1",
		Apply: recordingApply(&sent, func(int, string) error { return heldTransportErr() }),
		Landed: func(_ context.Context, node string) (bool, error) {
			checked = append(checked, node)
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("a change that landed is a success: %v", err)
	}
	if loc.Node != "pve-node1" || len(sent) != 1 || strings.Join(checked, ",") != "pve-node1" {
		t.Errorf("loc %+v, sent %v, checked %v; want one send and one check on pve-node1", loc, sent, checked)
	}
}

func TestMutateVMConfig_HeldTransportErrorWhereTheChangeDidNotLand(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 8009, func(time.Duration) *scanRow { return &scanRow{node: "pve-node1"} })
	var sent []string
	checks := 0
	_, err := MutateVMConfig(clk.ctx(context.Background()), c, nil, VMConfigMutation{
		VMID: 8009, Node: "pve-node1",
		Apply: recordingApply(&sent, func(attempt int, _ string) error {
			if attempt == 1 {
				return heldTransportErr()
			}
			return nil
		}),
		Landed: func(context.Context, string) (bool, error) {
			checks++
			return false, nil
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sent) != 2 || checks != 1 {
		t.Errorf("sent %v after %d checks; want the re-read before the second send", sent, checks)
	}
}

// The change was ambiguous on pve-node1, and the re-read finds the VM moved to
// pve-node2 without it, so the resend goes to pve-node2.
func TestMutateVMConfig_AmbiguousResultThenTheVMMoved(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	moved := false
	c := scriptedScanClient(clk, 8010, func(time.Duration) *scanRow {
		if moved {
			return &scanRow{node: "pve-node2"}
		}
		return &scanRow{node: "pve-node1"}
	})
	var sent []string
	_, err := MutateVMConfig(clk.ctx(context.Background()), c, nil, VMConfigMutation{
		VMID: 8010, Node: "pve-node1",
		Apply: recordingApply(&sent, func(attempt int, _ string) error {
			if attempt == 1 {
				moved = true
				return sdkerrors.ParseAPIError(502, []byte(`{"message":"Bad Gateway"}`))
			}
			return nil
		}),
		Landed: func(_ context.Context, node string) (bool, error) { return false, nil },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(sent, ",") != "pve-node1,pve-node2" {
		t.Errorf("sent to %v; want the resend on the VM's new node", sent)
	}
}

// After the ambiguous result, the VM is on pve-node2, so the identities the
// change names are re-resolved for pve-node2 before the resend goes there.
func TestMutateVMConfig_AmbiguousMoveReresolvesTheNewNode(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	moved := false
	c := scriptedScanClient(clk, 8030, func(time.Duration) *scanRow {
		if moved {
			return &scanRow{node: "pve-node2"}
		}
		return &scanRow{node: "pve-node1"}
	})
	var sent, reresolved []string
	_, err := MutateVMConfig(clk.ctx(context.Background()), c, nil, VMConfigMutation{
		VMID: 8030, Node: "pve-node1",
		Apply: recordingApply(&sent, func(attempt int, _ string) error {
			if attempt == 1 {
				moved = true
				return heldTransportErr()
			}
			if len(reresolved) == 0 {
				t.Error("the resend went out before Reresolve ran")
			}
			return nil
		}),
		Landed: func(context.Context, string) (bool, error) { return false, nil },
		Reresolve: func(_ context.Context, vm VMLocation) error {
			reresolved = append(reresolved, vm.Node)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(reresolved, ",") != "pve-node2" || strings.Join(sent, ",") != "pve-node1,pve-node2" {
		t.Errorf("re-resolved %v and sent to %v; want one re-resolve for pve-node2 before the resend there",
			reresolved, sent)
	}
}

// The re-read finds the VM where it was, so nothing needs re-resolving.
func TestMutateVMConfig_AmbiguousWithoutAMoveDoesNotReresolve(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 8031, func(time.Duration) *scanRow { return &scanRow{node: "pve-node1"} })
	var sent []string
	_, err := MutateVMConfig(clk.ctx(context.Background()), c, nil, VMConfigMutation{
		VMID: 8031, Node: "pve-node1",
		Apply: recordingApply(&sent, func(attempt int, _ string) error {
			if attempt == 1 {
				return heldTransportErr()
			}
			return nil
		}),
		Landed: func(context.Context, string) (bool, error) { return false, nil },
		Reresolve: func(context.Context, VMLocation) error {
			t.Error("Reresolve ran although the VM stayed on its node")
			return nil
		},
	})
	if err != nil || len(sent) != 2 {
		t.Fatalf("err %v sent %v; want the resend on the same node", err, sent)
	}
}

// A Reresolve error after the move stops the protocol and comes back as it is.
func TestMutateVMConfig_AmbiguousMoveReresolveErrorStops(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	moved := false
	c := scriptedScanClient(clk, 8032, func(time.Duration) *scanRow {
		if moved {
			return &scanRow{node: "pve-node2"}
		}
		return &scanRow{node: "pve-node1"}
	})
	want := cpierrors.Retriable("the source parker moved")
	var sent []string
	_, err := MutateVMConfig(clk.ctx(context.Background()), c, nil, VMConfigMutation{
		VMID: 8032, Node: "pve-node1",
		Apply: recordingApply(&sent, func(int, string) error {
			moved = true
			return heldTransportErr()
		}),
		Landed:    func(context.Context, string) (bool, error) { return false, nil },
		Reresolve: func(context.Context, VMLocation) error { return want },
	})
	if !errors.Is(err, want) || err.Error() != want.Error() {
		t.Fatalf("err = %v; want the Reresolve error unchanged", err)
	}
	if len(sent) != 1 {
		t.Errorf("sent %v; want no resend after Reresolve failed", sent)
	}
}

func TestMutateVMConfig_AmbiguousWithoutALandedCheckIsNotSentAgain(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 8011, func(time.Duration) *scanRow { return &scanRow{node: "pve-node1"} })
	var sent []string
	_, err := MutateVMConfig(clk.ctx(context.Background()), c, nil, VMConfigMutation{
		VMID: 8011, Node: "pve-node1",
		Apply: recordingApply(&sent, func(int, string) error { return heldTransportErr() }),
	})
	requireSettleRetriable(t, err, "unknown outcome", "not sent again")
	if len(sent) != 1 {
		t.Errorf("sent %v; want exactly one send", sent)
	}
}

func TestMutateVMConfig_AmbiguousBound(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 8012, func(time.Duration) *scanRow { return &scanRow{node: "pve-node1"} })
	var sent []string
	_, err := MutateVMConfig(clk.ctx(context.Background()), c, nil, VMConfigMutation{
		VMID: 8012, Node: "pve-node1",
		Apply:  recordingApply(&sent, func(int, string) error { return heldTransportErr() }),
		Landed: func(context.Context, string) (bool, error) { return false, nil },
	})
	requireSettleRetriable(t, err, "unknown outcome")
	if len(sent) != mutateMaxAmbiguous+1 {
		t.Errorf("sent %d times; want %d", len(sent), mutateMaxAmbiguous+1)
	}
}

// A re-read that fails leaves the outcome unknown, so the helper reads again
// rather than sending blind.
func TestMutateVMConfig_RecheckReadFailsThenAnswers(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 8013, func(time.Duration) *scanRow { return &scanRow{node: "pve-node1"} })
	var sent []string
	reads := 0
	_, err := MutateVMConfig(clk.ctx(context.Background()), c, nil, VMConfigMutation{
		VMID: 8013, Node: "pve-node1",
		Apply: recordingApply(&sent, func(int, string) error { return heldTransportErr() }),
		Landed: func(context.Context, string) (bool, error) {
			reads++
			if reads < 3 {
				return false, heldTransportErr()
			}
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sent) != 1 || reads != 3 {
		t.Errorf("sent %v after %d reads; want one send and three reads", sent, reads)
	}

	// A re-read that never answers ends with an unknown outcome and no resend.
	clk2 := newSettleTestClock()
	c2 := scriptedScanClient(clk2, 8013, func(time.Duration) *scanRow { return &scanRow{node: "pve-node1"} })
	var sent2 []string
	_, err = MutateVMConfig(clk2.ctx(context.Background()), c2, nil, VMConfigMutation{
		VMID: 8013, Node: "pve-node1",
		Apply:  recordingApply(&sent2, func(int, string) error { return heldTransportErr() }),
		Landed: func(context.Context, string) (bool, error) { return false, heldTransportErr() },
	})
	requireSettleRetriable(t, err, "unknown outcome")
	if len(sent2) != 1 {
		t.Errorf("sent %v; an unconfirmed change must not be sent again", sent2)
	}
}

// The landed check meets a config-missing answer from the node the index still
// names, and the probe finds the VM elsewhere.
func TestMutateVMConfig_RecheckFollowsTheConfig(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 8014, func(time.Duration) *scanRow { return &scanRow{node: "pve-node1"} })
	c.qemuSvc = &fakeQEMUService{configFn: func(_ context.Context, node string, vmid int) (map[string]any, error) {
		if node == "pve-node2" {
			return map[string]any{"name": "vm"}, nil
		}
		return nil, configMissingErr(node, vmid)
	}}
	var checked []string
	loc, err := MutateVMConfig(clk.ctx(context.Background()), c, nil, VMConfigMutation{
		VMID: 8014, Node: "pve-node1",
		Apply: func(context.Context, string) error { return heldTransportErr() },
		Landed: func(_ context.Context, node string) (bool, error) {
			checked = append(checked, node)
			if node == "pve-node1" {
				return false, configMissingErr(node, 8014)
			}
			return true, nil
		},
	})
	if err != nil || loc.Node != "pve-node2" {
		t.Fatalf("loc, err = %+v, %v; want landed on pve-node2", loc, err)
	}
	if strings.Join(checked, ",") != "pve-node1,pve-node2" {
		t.Errorf("checked %v", checked)
	}
}

// The change moves a disk off a source parker into the target VM. The parker
// migrates while the target stays on pve-node1, so PVE refuses the change for
// the parker's lock. The helper keeps the target on pve-node1 and lets
// Reresolve follow the parker before it sends again.
func TestMutateVMConfig_SourceParkerMovesWhileTheTargetStaysPut(t *testing.T) {
	t.Parallel()
	const target, parker = 8015, 9015
	clk := newSettleTestClock()
	var parkerMoveAt time.Duration = -1
	c := scriptedScanClient(clk, target, func(time.Duration) *scanRow { return &scanRow{node: "pve-node1"} })
	parkerClient := scriptedScanClient(clk, parker, func(e time.Duration) *scanRow {
		if parkerMoveAt < 0 || e-parkerMoveAt < 90*time.Second {
			return &scanRow{node: "pve-node1", lock: "migrate"}
		}
		return &scanRow{node: "pve-node2"}
	})
	parkerNode := "pve-node1"
	var sent []string
	loc, err := MutateVMConfig(clk.ctx(context.Background()), c, nil, VMConfigMutation{
		VMID: target, Node: "pve-node1",
		Apply: recordingApply(&sent, func(attempt int, node string) error {
			if attempt == 1 {
				parkerMoveAt = clk.Elapsed()
				return lockedErr(parker, "migrate")
			}
			if parkerNode != "pve-node2" {
				return fmt.Errorf("sent with the parker's stale node %s", parkerNode)
			}
			return nil
		}),
		Reresolve: func(ctx context.Context, vm VMLocation) error {
			if vm.Node != "pve-node1" {
				return fmt.Errorf("target moved to %s", vm.Node)
			}
			p, err := AwaitVMSettled(ctx, parkerClient, nil, VMSettleRequest{VMID: parker})
			if err != nil {
				return err
			}
			parkerNode = p.Node
			return nil
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loc.Node != "pve-node1" || strings.Join(sent, ",") != "pve-node1,pve-node1" {
		t.Errorf("loc %+v, sent %v; want both sends to the target on pve-node1", loc, sent)
	}
	if parkerNode != "pve-node2" {
		t.Errorf("parker node = %q; want pve-node2", parkerNode)
	}
}

func TestMutateVMConfig_ReresolveErrorStopsTheProtocol(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 8016, func(time.Duration) *scanRow { return &scanRow{node: "pve-node1"} })
	want := cpierrors.Retriable("replication job moved")
	var sent []string
	_, err := MutateVMConfig(clk.ctx(context.Background()), c, nil, VMConfigMutation{
		VMID: 8016, Node: "pve-node1",
		Apply:     recordingApply(&sent, func(int, string) error { return lockedErr(8016, "snapshot") }),
		Reresolve: func(context.Context, VMLocation) error { return want },
	})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v; want the re-resolve error", err)
	}
	if len(sent) != 1 {
		t.Errorf("sent %v", sent)
	}
}

func TestMutateVMConfig_SuspendedLockReturnsAtOnce(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 8017, func(time.Duration) *scanRow {
		return &scanRow{node: "pve-node1", lock: "suspended"}
	})
	var sent []string
	_, err := MutateVMConfig(clk.ctx(context.Background()), c, nil, VMConfigMutation{
		VMID: 8017, Node: "pve-node1",
		Apply: recordingApply(&sent, func(int, string) error { return lockedErr(8017, "suspended") }),
	})
	requireSettleRetriable(t, err, "locked (suspended)", "resuming")
	if len(sent) != 1 || clk.sleeps != 0 {
		t.Errorf("sent %v with %d sleeps; want one send and no wait", sent, clk.sleeps)
	}
}

func TestMutateVMConfig_DefiniteErrorsReturnAsTheyCame(t *testing.T) {
	t.Parallel()
	for _, want := range []error{
		&sdkerrors.APIError{HTTPCode: 400, Message: "parameter verification failed"},
		errors.New("some other failure"),
		&DriveDeletePendingError{},
	} {
		clk := newSettleTestClock()
		c := scriptedScanClient(clk, 8018, func(time.Duration) *scanRow { return &scanRow{node: "pve-node1"} })
		var sent []string
		_, err := MutateVMConfig(clk.ctx(context.Background()), c, nil, VMConfigMutation{
			VMID: 8018, Node: "pve-node1",
			Apply:  recordingApply(&sent, func(int, string) error { return want }),
			Landed: func(context.Context, string) (bool, error) { return false, nil },
		})
		if !errors.Is(err, want) {
			t.Errorf("err = %v; want %v unchanged", err, want)
		}
		if len(sent) != 1 {
			t.Errorf("%v: sent %v", want, sent)
		}
	}
}

func TestMutateVMConfig_ContextEndedIsAnUnknownOutcome(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 8019, func(time.Duration) *scanRow { return &scanRow{node: "pve-node1"} })
	_, err := MutateVMConfig(clk.ctx(ctx), c, nil, VMConfigMutation{
		VMID: 8019, Node: "pve-node1",
		Apply: func(context.Context, string) error {
			cancel()
			return context.Canceled
		},
		Landed: func(context.Context, string) (bool, error) {
			t.Error("no re-read after the context ended")
			return false, nil
		},
	})
	requireSettleRetriable(t, err, "unknown outcome")
}

// A 4xx is PVE's verdict even when the context ends at the same moment, so it
// comes back as it came, not as an unknown outcome.
func TestMutateVMConfig_DefiniteErrorWinsOverAnEndedContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 8033, func(time.Duration) *scanRow { return &scanRow{node: "pve-node1"} })
	want := &sdkerrors.APIError{HTTPCode: 400, Message: "parameter verification failed"}
	_, err := MutateVMConfig(clk.ctx(ctx), c, nil, VMConfigMutation{
		VMID: 8033, Node: "pve-node1",
		Apply: func(context.Context, string) error {
			cancel()
			return want
		},
		Landed: func(context.Context, string) (bool, error) { return false, nil },
	})
	if !errors.Is(err, want) || strings.Contains(err.Error(), "unknown outcome") {
		t.Fatalf("err = %v; want the 400 unchanged", err)
	}
}

func TestMutateVMConfig_EmptyNodeWaitsFirst(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 8020, func(e time.Duration) *scanRow {
		if e < time.Minute {
			return &scanRow{node: "pve-node1", lock: "migrate"}
		}
		return &scanRow{node: "pve-node3"}
	})
	var sent []string
	loc, err := MutateVMConfig(clk.ctx(context.Background()), c, nil, VMConfigMutation{
		VMID:  8020,
		Apply: recordingApply(&sent, func(int, string) error { return nil }),
	})
	if err != nil || loc.Node != "pve-node3" || strings.Join(sent, ",") != "pve-node3" {
		t.Fatalf("loc %+v, sent %v, err %v; want one send to pve-node3", loc, sent, err)
	}

	// A VM the wait cannot find is not sent anything.
	clk2 := newSettleTestClock()
	c2 := scriptedScanClient(clk2, 8020, func(time.Duration) *scanRow { return nil })
	_, err = MutateVMConfig(clk2.ctx(context.Background()), c2, nil, VMConfigMutation{
		VMID:  8020,
		Apply: func(context.Context, string) error { t.Error("nothing to send to"); return nil },
	})
	requireSettleRetriable(t, err, "was not sent")
}

func TestMutateVMConfig_UsageErrors(t *testing.T) {
	t.Parallel()
	for _, m := range []VMConfigMutation{
		{VMID: 0, Apply: func(context.Context, string) error { return nil }},
		{VMID: 1},
	} {
		_, err := MutateVMConfig(context.Background(), nil, nil, m)
		if err == nil || cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
			t.Errorf("err = %v; want a non-retriable usage error", err)
		}
	}
	if got := (VMConfigMutation{}).label(); got != "mutate_vm_config" {
		t.Errorf("default label = %q", got)
	}
}

// A positive Budget ends the protocol early, even where the motion budget
// would let a migrate lock be waited on for ten minutes.
func TestMutateVMConfig_BudgetCapsTheWait(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 8020, func(time.Duration) *scanRow {
		return &scanRow{node: "pve-node1", lock: "migrate", haState: "migrate"}
	})
	var sent []string
	_, err := MutateVMConfig(clk.ctx(context.Background()), c, nil, VMConfigMutation{
		VMID: 8020, Node: "pve-node1", MotionWait: 10 * time.Minute, Budget: 10 * time.Second,
		Apply: recordingApply(&sent, func(int, string) error { return lockedErr(8020, "migrate") }),
	})
	requireSettleRetriable(t, err, "locked (migrate)")
	if e := clk.Elapsed(); e != 10*time.Second {
		t.Errorf("waited %v; want the 10s budget, not the motion budget", e)
	}
	if len(sent) != 1 {
		t.Errorf("sent %v; want one send before the budget ended the wait", sent)
	}
}
