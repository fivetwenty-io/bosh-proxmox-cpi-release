package pve

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// windowBoundConfigClient is a hangingRestoreClient whose config reads honor
// their context the way a real API call does, so a read on a context that
// has already ended fails at once instead of answering from the fake's maps.
// newWindowBoundConfigClient wraps the config read once, when the client is
// built, and QEMU hands back that one service on every call.
type windowBoundConfigClient struct {
	*hangingRestoreClient
	qemu *fakeQEMUService
}

func newWindowBoundConfigClient(t *testing.T, h *hangingRestoreClient) *windowBoundConfigClient {
	t.Helper()
	inner, ok := h.QEMU().(*fakeQEMUService)
	if !ok {
		t.Fatal("scanFakeClient.QEMU is not a *fakeQEMUService")
	}
	read := inner.configFn
	inner.configFn = func(ctx context.Context, node string, vmid int) (map[string]any, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return read(ctx, node, vmid)
	}
	return &windowBoundConfigClient{hangingRestoreClient: h, qemu: inner}
}

func (c *windowBoundConfigClient) QEMU() qemu.Service { return c.qemu }

// TestTransferDiskFromParker_RestoreOutlastsWindowKeepsLandedName moves a disk
// off a parker under the protection-window lock and hangs the protection
// restore until after the window's deadline. The disk landed before the
// restore started, so the caller still gets its new name beside the cut-off,
// and can record the disk on its new VM. The read of that name must not run on
// the window's context, which has ended by the time the restore gives up.
func TestTransferDiskFromParker_RestoreOutlastsWindowKeepsLandedName(t *testing.T) {
	defer SetClusterLockGraceForTest(0)()
	// A test-sized TTL puts the window's deadline on its one-second floor, and
	// the restore's own deadline runs half a second past it.
	const restoreTimeout = 1500 * time.Millisecond
	ctx := withTestParkerLockTimeouts(context.Background(), 10*time.Second, time.Second)
	ctx = WithParkerProtectionRestoreTimeoutForTest(ctx, restoreTimeout)
	scan := newScanFakeClient(map[int]map[string]any{
		90000: {
			cfgKeyTags:      "bosh-cpi;bosh-parker",
			paramProtection: true,
			"scsi0":         "data:vm-90000-disk-0,serial=" + transferStableID,
		},
		700: {},
	})
	pools := newFakeLockPools()
	scan.pools = pools
	c := newWindowBoundConfigClient(t, &hangingRestoreClient{parker: 90000, scanFakeClient: scan})
	logger, _ := newRestoreTestLogger(t)
	parker := DiskHolder{Found: true, VMID: 90000, Node: "pve1", IsParker: true, Slot: "scsi0"}
	start := time.Now()
	landed, err := TransferDiskFromParker(ctx, c, logger, parker, 700, "scsi1",
		"data:vm-90000-disk-0", "data:vm-90000-disk-0,serial="+transferStableID, transferTestCfg)
	if elapsed := time.Since(start); elapsed < restoreTimeout {
		t.Fatalf("returned after %s, before the restore's %s deadline; the restore did not outlast the window", elapsed, restoreTimeout)
	}
	if restores, endedBy := c.outcome(); restores == 0 || !errors.Is(endedBy, context.DeadlineExceeded) {
		t.Fatalf("the restore was not cut off by its deadline: attempts=%d ended by %v", restores, endedBy)
	}
	var cutOff *ProtectionRestoreCutOffError
	if !errors.As(err, &cutOff) || cutOff.ParkerVMID != 90000 || !cutOff.WorkCompleted {
		t.Fatalf("error %v is not a cut-off restore for parker 90000 after completed work", err)
	}
	if strings.Contains(err.Error(), "config read for target vm 700") {
		t.Fatalf("the landed-name read failed on the ended window: %v", err)
	}
	slot, _ := c.configs[700]["scsi1"].(string)
	if !strings.HasPrefix(slot, "data:vm-700-disk-") {
		t.Fatalf("target slot = %q, want the disk moved before the restore was cut off", slot)
	}
	if landed == "" || !strings.HasPrefix(slot, landed) {
		t.Fatalf("landed volid = %q, want the name on the target slot %q", landed, slot)
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("error %v is not retriable", err)
	}
}

// TestTransferDiskFromParker_HangingLandedNameReadIsCutOff moves a disk off a
// parker and then hangs the read that finds its landed name. The read runs on
// a detached context, so only its own bound can end it. The call must come
// back once that bound passes, well before the API client's timeout, and say
// that the read failed, beside the restore's cut-off.
func TestTransferDiskFromParker_HangingLandedNameReadIsCutOff(t *testing.T) {
	defer SetClusterLockGraceForTest(0)()
	const readTimeout = 200 * time.Millisecond
	ctx := withTestParkerLockTimeouts(context.Background(), 10*time.Second, time.Second)
	ctx = WithParkerProtectionRestoreTimeoutForTest(ctx, 300*time.Millisecond)
	ctx = withTestLandedNameReadTimeout(ctx, readTimeout)
	scan := newScanFakeClient(map[int]map[string]any{
		90000: {
			cfgKeyTags:      "bosh-cpi;bosh-parker",
			paramProtection: true,
			"scsi0":         "data:vm-90000-disk-0,serial=" + transferStableID,
		},
		700: {},
	})
	scan.pools = newFakeLockPools()
	c := newWindowBoundConfigClient(t, &hangingRestoreClient{parker: 90000, scanFakeClient: scan})
	read := c.qemu.configFn
	var (
		mu     sync.Mutex
		hungBy error
	)
	c.qemu.configFn = func(ctx context.Context, node string, vmid int) (map[string]any, error) {
		scan.mu.Lock()
		landed, _ := scan.configs[700]["scsi1"].(string)
		scan.mu.Unlock()
		if vmid != 700 || landed == "" {
			return read(ctx, node, vmid)
		}
		<-ctx.Done()
		mu.Lock()
		hungBy = ctx.Err()
		mu.Unlock()
		return nil, ctx.Err()
	}
	logger, _ := newRestoreTestLogger(t)
	parker := DiskHolder{Found: true, VMID: 90000, Node: "pve1", IsParker: true, Slot: "scsi0"}
	start := time.Now()
	landed, err := TransferDiskFromParker(ctx, c, logger, parker, 700, "scsi1",
		"data:vm-90000-disk-0", "data:vm-90000-disk-0,serial="+transferStableID, transferTestCfg)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("returned after %s; the hanging read was not cut off by its %s bound", elapsed, readTimeout)
	}
	mu.Lock()
	cutBy := hungBy
	mu.Unlock()
	if !errors.Is(cutBy, context.DeadlineExceeded) {
		t.Fatalf("the landed-name read ended by %v, want its own deadline", cutBy)
	}
	if landed != "" {
		t.Fatalf("landed volid = %q from a read that never answered", landed)
	}
	if err == nil || !strings.Contains(err.Error(), "config read for target vm 700 after move") {
		t.Fatalf("error %v does not name the landed-name read that failed", err)
	}
	var cutOff *ProtectionRestoreCutOffError
	if !errors.As(err, &cutOff) || cutOff.ParkerVMID != 90000 {
		t.Fatalf("error %v lost the restore's cut-off for parker 90000", err)
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("error %v is not retriable", err)
	}
}

// parkerRestoreWorstCase is the longest a protection restore can take on the
// retry curves configured now: every sleep between parkerWindowMaxAttempts
// attempts at the top of its jitter window, and one round trip per attempt.
func parkerRestoreWorstCase() time.Duration {
	return RetryOnTransientOrLockSleepBudget(parkerWindowMaxAttempts) + time.Duration(parkerWindowMaxAttempts)*clusterLockRoundTrip
}

// parkerSweepWorstCase is the longest the deferred sweep can take on the
// retry curves configured now. The clear and the removal retry on the
// transient-or-lock curves, the verify retries on the transient curves, each
// gets parkerWindowMaxAttempts attempts with a round trip apiece, and the
// first config read is one more round trip.
func parkerSweepWorstCase() time.Duration {
	return 2*RetryOnTransientOrLockSleepBudget(parkerWindowMaxAttempts) +
		RetryOnTransientSleepBudget(parkerWindowMaxAttempts) +
		time.Duration(3*parkerWindowMaxAttempts+1)*clusterLockRoundTrip
}

// TestParkerRestoreReserve_ShippedCurvesKeep35Seconds pins the restore's
// deadline, the sweep's deadline, and the window arithmetic on the shipped
// retry curves. The restore's worst case is about 32.9 seconds, so its
// deadline stays 35. The sweep's worst case is about 99.1 seconds, past its
// 45-second floor, so its deadline is 100 seconds and the TTL carries the
// 55 seconds of excess, which leaves the body its 90 seconds.
func TestParkerRestoreReserve_ShippedCurvesKeep35Seconds(t *testing.T) {
	if worst := parkerRestoreWorstCase(); worst > 35*time.Second {
		t.Fatalf("the restore's worst case on the shipped curves is %v, over 35s", worst)
	}
	if got := parkerRestoreTimeout(context.Background()); got != 35*time.Second {
		t.Fatalf("restore deadline on the shipped curves = %v, want 35s", got)
	}
	if worst, got := parkerSweepWorstCase(), parkerDemotedSweepTimeoutNow(); got != 100*time.Second || got < worst {
		t.Fatalf("sweep deadline on the shipped curves = %v for a worst case of %v, want 100s", got, worst)
	}
	ttl, _ := parkerLockTimeoutsFrom(context.Background())
	if ttl != 235*time.Second {
		t.Fatalf("parker lock TTL on the shipped curves = %v, want 235s", ttl)
	}
	now := time.Unix(10_000, 0)
	h := &ClusterLockHandle{expiry: now.Add(ttl)}
	if body := parkerWindowDeadline(h, now).Sub(now); body != 90*time.Second {
		t.Fatalf("window body on the shipped curves = %v, want 90s", body)
	}
}

// TestParkerRestoreReserve_FollowsALengthenedRetryCurve lengthens the pushback
// curve the way pve.retry.pushback does. The restore and the deferred sweep
// both retry on that curve, so their deadlines have to grow to cover the
// curve's worst case, or a restore that needs its last attempt is cut off
// with protection still off, and a sweep is cut off with the unusedN key
// still on the parker. The lock's TTL grows with them, so the window's body
// keeps the time it has on the shipped curves, and the body, the restore, the
// sweep, and the release still end inside the claim.
func TestParkerRestoreReserve_FollowsALengthenedRetryCurve(t *testing.T) {
	shippedTTL, _ := parkerLockTimeoutsFrom(context.Background())
	shippedSweep := parkerDemotedSweepTimeoutNow()
	now := time.Unix(10_000, 0)
	shippedBody := parkerWindowDeadline(&ClusterLockHandle{expiry: now.Add(shippedTTL)}, now).Sub(now)

	defer SetPushbackBackoffForTest(20_000, 120_000)()
	worst := parkerRestoreWorstCase()
	if worst <= 35*time.Second {
		t.Fatalf("the lengthened curve's worst case is %v, which does not exercise the derivation", worst)
	}
	restore := parkerRestoreTimeout(context.Background())
	if restore < worst {
		t.Fatalf("restore deadline = %v on a curve whose worst case is %v; a restore that needs its last attempt is cut off", restore, worst)
	}
	sweep := parkerDemotedSweepTimeoutNow()
	if sweepWorst := parkerSweepWorstCase(); sweep < sweepWorst || sweep <= shippedSweep {
		t.Fatalf("sweep deadline = %v on a curve whose sweep worst case is %v, and %v on the shipped curves; "+
			"a sweep that needs its last attempts is cut off", sweep, sweepWorst, shippedSweep)
	}
	ttl, _ := parkerLockTimeoutsFrom(context.Background())
	if ttl-shippedTTL < sweep-shippedSweep {
		t.Fatalf("the TTL grew by %v while the sweep's deadline grew by %v", ttl-shippedTTL, sweep-shippedSweep)
	}
	body := parkerWindowDeadline(&ClusterLockHandle{expiry: now.Add(ttl)}, now).Sub(now)
	if body < shippedBody {
		t.Fatalf("window body = %v under a %v TTL, shorter than the %v it has on the shipped curves", body, ttl, shippedBody)
	}
	if total := body + restore + sweep + parkerLockReleaseTimeout; total > ttl {
		t.Fatalf("body %v, restore %v, sweep %v, and release %v add up to %v, past the TTL %v",
			body, restore, sweep, parkerLockReleaseTimeout, total, ttl)
	}
}

// TestConfirm_LateConfirmationInsideReleaseMarginIsRefused covers a create
// whose confirming reads fail until the claim is inside its release margin. A
// handle that late would be stolen before the caller could use it, and its own
// release would refuse to delete it, so the acquire refuses it, retriably, and
// says why. The claim is too close to its expiry to delete safely, so it is
// left for the TTL steal, which a retry makes within seconds.
func TestConfirm_LateConfirmationInsideReleaseMarginIsRefused(t *testing.T) {
	f := newFakeLockPools()
	clk, _ := sleepLog(time.Unix(1000, 0))
	answersFrom := clk.now().Add(8 * time.Second)
	confirmReads(f, func(int) bool { return clk.now().Before(answersFrom) })
	h, err := acquireClusterLockWithClock(context.Background(), f, "web", "me", 10*time.Second, 10*time.Second, clk)
	if h != nil {
		t.Fatalf("the acquire returned a handle with %v left on its claim", h.expiry.Sub(clk.now()))
	}
	if err == nil || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("want a retriable refusal, got %v", err)
	}
	if !strings.Contains(err.Error(), `confirmed lock "bosh-lock-web" too late to use it`) {
		t.Fatalf("the refusal does not say what happened: %v", err)
	}
	if f.deleteN != 0 {
		t.Fatal("a claim inside its release margin was deleted; a steal may already have replaced it")
	}
	if !strings.Contains(f.pools["bosh-lock-web"], "owner=me ") {
		t.Fatalf("the sentinel = %q, want our claim left for its TTL steal", f.pools["bosh-lock-web"])
	}
}
