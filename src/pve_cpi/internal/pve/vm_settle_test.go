package pve

// AwaitVMSettled contract tests. Every case runs on a fake clock, and the
// cluster scan is scripted against that clock, so a ten-minute migration
// runs in microseconds and the budgets are checked to the second.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// settleTestClock is a fake clock whose sleep advances time at once.
type settleTestClock struct {
	mu     sync.Mutex
	start  time.Time
	now    time.Time
	sleeps int
}

func newSettleTestClock() *settleTestClock {
	t0 := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	return &settleTestClock{start: t0, now: t0}
}

func (c *settleTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *settleTestClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.sleeps++
	return nil
}

// Elapsed is the fake time since the clock started.
func (c *settleTestClock) Elapsed() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now.Sub(c.start)
}

func (c *settleTestClock) ctx(parent context.Context) context.Context {
	return WithVMSettleClockForTest(WithTestBackoff(parent, func(int) time.Duration { return 0 }), c.Now, c.Sleep)
}

// scanRow is one /cluster/resources row for the VM under test.
type scanRow struct {
	node, lock, haState string
}

// scriptedScanClient serves a cluster scan whose row for vmid comes from
// rowAt, evaluated at the fake clock's elapsed time. A nil row means the
// index has no row for the VM.
func scriptedScanClient(clk *settleTestClock, vmid int, rowAt func(elapsed time.Duration) *scanRow) *findVMTestClient {
	return &findVMTestClient{
		backendTestClient: backendTestClient{
			clusterSvc: &fakeCluster{
				listFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
					row := rowAt(clk.Elapsed())
					if row == nil {
						return clusterResp(), nil
					}
					fields := map[string]any{"vmid": vmid, "node": row.node, "type": "qemu", "status": "running"}
					if row.lock != "" {
						fields["lock"] = row.lock
					}
					if row.haState != "" {
						fields["hastate"] = row.haState
					}
					return clusterResp(fields), nil
				},
				configNodesFn: findVMConfigNodes("pve-node1", "pve-node2"),
			},
		},
		qemuSvc: &fakeQEMUService{
			configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
				return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
			},
		},
	}
}

func requireSettleRetriable(t *testing.T, err error, wantInMsg ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a retriable error, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("error %q is not retriable", err)
	}
	for _, want := range wantInMsg {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

func TestAwaitVMSettled_SettledAtOnceDoesNotSleep(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 7001, func(time.Duration) *scanRow {
		return &scanRow{node: "pve-node1", haState: "started"}
	})
	loc, err := AwaitVMSettled(clk.ctx(context.Background()), c, nil, VMSettleRequest{VMID: 7001})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !loc.Found || loc.Node != "pve-node1" {
		t.Errorf("loc = %+v; want pve-node1", loc)
	}
	if clk.sleeps != 0 {
		t.Errorf("slept %d times; a settled VM must not wait", clk.sleeps)
	}
}

// A migration that moves the VM mid-wait must end the wait on the new node,
// never the node the wait started on.
func TestAwaitVMSettled_FollowsAMigrationToTheNewNode(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 7002, func(e time.Duration) *scanRow {
		if e < 5*time.Minute {
			return &scanRow{node: "pve-node1", lock: "migrate", haState: "migrate"}
		}
		return &scanRow{node: "pve-node2", haState: "started"}
	})
	loc, err := AwaitVMSettled(clk.ctx(context.Background()), c, nil, VMSettleRequest{VMID: 7002})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if loc.Node != "pve-node2" {
		t.Errorf("node = %q; want the migration's target pve-node2", loc.Node)
	}
	if e := clk.Elapsed(); e < 5*time.Minute || e > 5*time.Minute+settlePollCap {
		t.Errorf("waited %v; want just past the 5m migration", e)
	}
}

func TestAwaitVMSettled_BudgetsByLock(t *testing.T) {
	t.Parallel()
	cases := []struct {
		lock, haState string
		motion        time.Duration
		want          time.Duration
	}{
		{lock: "migrate", want: DefaultMotionWait},
		{lock: "migrate", motion: 15 * time.Minute, want: 15 * time.Minute},
		{lock: "backup", want: 120 * time.Second},
		{lock: "snapshot", want: 120 * time.Second},
		{lock: "snapshot-delete", want: 120 * time.Second},
		{lock: "clone", want: 120 * time.Second},
		{lock: "create", want: 60 * time.Second},
		{lock: "rollback", want: 60 * time.Second},
		{haState: "migrate", want: DefaultMotionWait},
		{haState: "relocate", want: DefaultMotionWait},
		{haState: "request_stop", want: 60 * time.Second},
		{haState: "freeze", want: 60 * time.Second},
		{haState: "queued", want: 60 * time.Second},
		{lock: "backup", haState: "migrate", want: DefaultMotionWait},
	}
	for _, tc := range cases {
		t.Run(tc.lock+"/"+tc.haState, func(t *testing.T) {
			t.Parallel()
			clk := newSettleTestClock()
			c := scriptedScanClient(clk, 7003, func(time.Duration) *scanRow {
				return &scanRow{node: "pve-node1", lock: tc.lock, haState: tc.haState}
			})
			_, err := AwaitVMSettled(clk.ctx(context.Background()), c, nil,
				VMSettleRequest{VMID: 7003, MotionWait: tc.motion, Label: "attach_disk"})
			want := []string{"attach_disk", "7003", `"pve-node1"`}
			if tc.lock != "" {
				want = append(want, "locked ("+tc.lock+")")
			}
			requireSettleRetriable(t, err, want...)
			if e := clk.Elapsed(); e != tc.want {
				t.Errorf("waited %v; want the %v budget", e, tc.want)
			}
		})
	}
}

func TestAwaitVMSettled_BackupLockThatOutlastsItsBudget(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 7004, func(e time.Duration) *scanRow {
		if e < 3*time.Minute {
			return &scanRow{node: "pve-node1", lock: "backup"}
		}
		return &scanRow{node: "pve-node1"}
	})
	_, err := AwaitVMSettled(clk.ctx(context.Background()), c, nil, VMSettleRequest{VMID: 7004})
	requireSettleRetriable(t, err, "locked (backup)")
	if e := clk.Elapsed(); e != 120*time.Second {
		t.Errorf("waited %v; want the 120s backup budget", e)
	}

	// The same lock clearing inside its budget settles.
	clk2 := newSettleTestClock()
	c2 := scriptedScanClient(clk2, 7004, func(e time.Duration) *scanRow {
		if e < 100*time.Second {
			return &scanRow{node: "pve-node1", lock: "backup"}
		}
		return &scanRow{node: "pve-node1"}
	})
	if _, err := AwaitVMSettled(clk2.ctx(context.Background()), c2, nil, VMSettleRequest{VMID: 7004}); err != nil {
		t.Fatalf("a backup lock that clears at 100s must settle: %v", err)
	}
}

// A migration that hands over to a backup keeps the larger budget it already
// earned rather than restarting the clock.
func TestAwaitVMSettled_BudgetIsTheLargestSeen(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 7005, func(e time.Duration) *scanRow {
		switch {
		case e < 4*time.Minute:
			return &scanRow{node: "pve-node1", lock: "migrate"}
		case e < 8*time.Minute:
			return &scanRow{node: "pve-node2", lock: "backup"}
		default:
			return &scanRow{node: "pve-node2"}
		}
	})
	loc, err := AwaitVMSettled(clk.ctx(context.Background()), c, nil, VMSettleRequest{VMID: 7005})
	if err != nil {
		t.Fatalf("the backup after the migration ends inside the 10m budget: %v", err)
	}
	if loc.Node != "pve-node2" {
		t.Errorf("node = %q; want pve-node2", loc.Node)
	}
}

func TestAwaitVMSettled_RefusesAtOnce(t *testing.T) {
	t.Parallel()
	for _, row := range []scanRow{
		{node: "pve-node3", haState: "fence"},
		{node: "pve-node3", haState: "recovery"},
		{node: "pve-node1", lock: "suspended"},
	} {
		clk := newSettleTestClock()
		c := scriptedScanClient(clk, 7006, func(time.Duration) *scanRow { return &row })
		_, err := AwaitVMSettled(clk.ctx(context.Background()), c, nil, VMSettleRequest{VMID: 7006})
		requireSettleRetriable(t, err, row.node)
		if clk.sleeps != 0 {
			t.Errorf("%+v: slept %d times; waiting cannot clear it", row, clk.sleeps)
		}
	}
}

func TestAwaitVMSettled_StableHAStatesSettle(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"", "started", "stopped", "error", "ignored", "disabled", "enabled"} {
		clk := newSettleTestClock()
		c := scriptedScanClient(clk, 7007, func(time.Duration) *scanRow {
			return &scanRow{node: "pve-node1", haState: state}
		})
		if _, err := AwaitVMSettled(clk.ctx(context.Background()), c, nil, VMSettleRequest{VMID: 7007}); err != nil {
			t.Errorf("hastate %q: %v; want settled", state, err)
		}
	}
}

// The context's deadline caps the wait a few seconds early, so the call's
// own error, which names the lock, reaches the Director before the method's
// operation timeout does.
func TestAwaitVMSettled_StaysInsideTheContextDeadline(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	realNow := time.Now()
	clk.start, clk.now = realNow, realNow
	ctx, cancel := context.WithDeadline(context.Background(), realNow.Add(90*time.Second))
	defer cancel()
	c := scriptedScanClient(clk, 7008, func(time.Duration) *scanRow {
		return &scanRow{node: "pve-node1", lock: "migrate"}
	})
	_, err := AwaitVMSettled(clk.ctx(ctx), c, nil, VMSettleRequest{VMID: 7008})
	requireSettleRetriable(t, err, "locked (migrate)")
	if e := clk.Elapsed(); e != 90*time.Second-settleDeadlineReserve {
		t.Errorf("waited %v; want the deadline less the %v reserve", e, settleDeadlineReserve)
	}
}

func TestAwaitVMSettled_ScanFailures(t *testing.T) {
	t.Parallel()
	listErr := errors.New("list resources: permission denied")

	// A failed poll is polled again.
	clk := newSettleTestClock()
	calls := 0
	c := &findVMTestClient{backendTestClient: backendTestClient{clusterSvc: &fakeCluster{
		listFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			calls++
			if calls == 1 {
				return nil, listErr
			}
			return clusterResp(map[string]any{"vmid": 7009, "node": "pve-node2", "type": "qemu"}), nil
		},
	}}}
	loc, err := AwaitVMSettled(clk.ctx(context.Background()), c, nil, VMSettleRequest{VMID: 7009})
	if err != nil || loc.Node != "pve-node2" {
		t.Fatalf("loc, err = %+v, %v; want pve-node2 after one failed poll", loc, err)
	}

	// A scan that keeps failing ends after the 60s budget with its last error.
	clk2 := newSettleTestClock()
	c2 := &findVMTestClient{backendTestClient: backendTestClient{clusterSvc: &fakeCluster{
		listFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return nil, listErr
		},
	}}}
	_, err = AwaitVMSettled(clk2.ctx(context.Background()), c2, nil, VMSettleRequest{VMID: 7009})
	requireSettleRetriable(t, err, "the cluster scan failed", "permission denied")
	if e := clk2.Elapsed(); e != settleOtherLockBudget {
		t.Errorf("waited %v; want %v", e, settleOtherLockBudget)
	}
}

// ScanFailureBudget shortens the wait for a scan that keeps failing, and it
// never reaches past the figure the caller set.
func TestAwaitVMSettled_ScanFailureBudgetCapsAFailingScan(t *testing.T) {
	t.Parallel()
	listErr := errors.New("list resources: connection refused")
	clk := newSettleTestClock()
	c := &findVMTestClient{backendTestClient: backendTestClient{clusterSvc: &fakeCluster{
		listFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			return nil, listErr
		},
	}}}
	_, err := AwaitVMSettled(clk.ctx(context.Background()), c, nil, VMSettleRequest{
		VMID: 7009, ScanFailureBudget: 15 * time.Second,
	})
	requireSettleRetriable(t, err, "the cluster scan failed", "connection refused")
	if e := clk.Elapsed(); e != 15*time.Second {
		t.Errorf("waited %v; want the 15s the caller set", e)
	}
}

// IgnoreHAState leaves a service in fence or recovery, and an HA migration,
// out of the wait, while the row's lock still holds it.
func TestAwaitVMSettled_IgnoreHAStateReadsNoHAState(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"fence", "recovery", "migrate"} {
		clk := newSettleTestClock()
		c := scriptedScanClient(clk, 7011, func(time.Duration) *scanRow {
			return &scanRow{node: "pve-node1", haState: state}
		})
		loc, err := AwaitVMSettled(clk.ctx(context.Background()), c, nil, VMSettleRequest{VMID: 7011, IgnoreHAState: true})
		if err != nil || loc.Node != "pve-node1" {
			t.Errorf("HA state %s: loc, err = %+v, %v; want settled at once", state, loc, err)
		}
		if clk.Elapsed() != 0 {
			t.Errorf("HA state %s: waited %v, want no wait", state, clk.Elapsed())
		}
	}

	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 7012, func(time.Duration) *scanRow {
		return &scanRow{node: "pve-node1", lock: "backup", haState: "fence"}
	})
	_, err := AwaitVMSettled(clk.ctx(context.Background()), c, nil, VMSettleRequest{VMID: 7012, IgnoreHAState: true})
	requireSettleRetriable(t, err, "locked (backup)")
}

// A VM the index has not caught up with is found by the config probe, whose
// answer carries no lock or HA state, so the wait polls until the row shows.
func TestAwaitVMSettled_IndexLagPollsUntilTheRowAppears(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 7010, func(e time.Duration) *scanRow {
		if e < 10*time.Second {
			return nil
		}
		return &scanRow{node: "pve-node2"}
	})
	c.qemuSvc = &fakeQEMUService{configFn: func(_ context.Context, node string, _ int) (map[string]any, error) {
		if node == "pve-node2" {
			return map[string]any{"name": "vm"}, nil
		}
		return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
	}}
	loc, err := AwaitVMSettled(clk.ctx(context.Background()), c, nil, VMSettleRequest{VMID: 7010})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !loc.FromScan || loc.Node != "pve-node2" {
		t.Errorf("loc = %+v; want the scan row once it appears", loc)
	}
	if clk.sleeps == 0 {
		t.Error("a config-probe answer carries no lock, so the wait must poll again")
	}
}

func TestAwaitVMSettled_VMNotFoundIsNotAnError(t *testing.T) {
	t.Parallel()
	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 7011, func(time.Duration) *scanRow { return nil })
	loc, err := AwaitVMSettled(clk.ctx(context.Background()), c, nil, VMSettleRequest{VMID: 7011})
	if err != nil || loc.Found {
		t.Fatalf("loc, err = %+v, %v; want Found=false with no error", loc, err)
	}
}

func TestAwaitVMSettled_ContextEndsDuringTheWait(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	clk := newSettleTestClock()
	c := scriptedScanClient(clk, 7012, func(time.Duration) *scanRow {
		cancel()
		return &scanRow{node: "pve-node1", lock: "migrate"}
	})
	_, err := AwaitVMSettled(clk.ctx(ctx), c, nil, VMSettleRequest{VMID: 7012})
	requireSettleRetriable(t, err, "locked (migrate)")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v does not carry the context's end", err)
	}
}

func TestAwaitVMSettled_InvalidVMID(t *testing.T) {
	t.Parallel()
	_, err := AwaitVMSettled(context.Background(), nil, nil, VMSettleRequest{VMID: 0})
	if err == nil || cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("err = %v; want a non-retriable usage error", err)
	}
}

func TestAwaitVMSettled_DefaultClockAndLabel(t *testing.T) {
	t.Parallel()
	if clk := settleClockFrom(context.Background()); clk.now == nil || clk.sleep == nil {
		t.Fatal("the default settle clock must be the wall clock")
	}
	if got := (VMSettleRequest{}).label(); got != "await_vm_settled" {
		t.Errorf("default label = %q", got)
	}
	if got := (VMSettleRequest{}).motionWait(); got != DefaultMotionWait {
		t.Errorf("default motion wait = %v", got)
	}
}

// A VM that only the config probe found, because the index has no row for it
// yet, ends the wait at once when the caller says its next step handles a lock
// or a move itself, and otherwise polls for the row.
func TestAwaitVMSettled_ProbeFoundSettlesOnRequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		settles    bool
		wantSleeps bool
	}{
		{name: "default polls for the row", settles: false, wantSleeps: true},
		{name: "probe found settles", settles: true, wantSleeps: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clk := newSettleTestClock()
			c := scriptedScanClient(clk, 7020, func(e time.Duration) *scanRow {
				if e < 10*time.Second {
					return nil
				}
				return &scanRow{node: "pve-node2"}
			})
			c.qemuSvc = &fakeQEMUService{configFn: func(_ context.Context, node string, _ int) (map[string]any, error) {
				if node == "pve-node2" {
					return map[string]any{"name": "vm"}, nil
				}
				return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
			}}
			loc, err := AwaitVMSettled(clk.ctx(context.Background()), c, nil,
				VMSettleRequest{VMID: 7020, ProbeFoundSettles: tc.settles})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if loc.Node != "pve-node2" || loc.FromScan == tc.settles {
				t.Errorf("loc = %+v; want pve-node2, from the probe only when settling on it", loc)
			}
			if (clk.sleeps > 0) != tc.wantSleeps {
				t.Errorf("slept %d times; wantSleeps = %v", clk.sleeps, tc.wantSleeps)
			}
		})
	}
}
