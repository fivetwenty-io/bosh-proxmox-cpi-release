package pve

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

// noQuorumPoolText is PVE's message for a pool create without quorum, as
// text that carries no answer from PVE.
const noQuorumPoolText = "create pool failed: cfs-lock 'file-user_cfg' error: no quorum!"

// noQuorumPoolBody is the body PVE sends with the HTTP 500 for a pool create
// whose user.cfg lock it could not take because the cluster has no quorum.
const noQuorumPoolBody = `{"data":null,"message":"create pool failed: cfs-lock 'file-user_cfg' error: no quorum!\n"}`

// sdkPoolCreateError wraps PVE's answer to POST /pools the way the SDK does,
// so the error text matches what the CPI logs.
func sdkPoolCreateError(status int, body string) error {
	return fmt.Errorf("pools.CreatePools: failed to execute POST request to \"/pools\" with context: "+
		"HTTP POST request to \"/pools\" with context failed: API request failed: %w",
		sdkerrors.ParseAPIError(status, []byte(body)))
}

func TestSDKPoolCreateErrorMatchesTheLoggedText(t *testing.T) {
	t.Parallel()
	got := sdkPoolCreateError(500, noQuorumPoolBody).Error()
	// The CPI's log escapes the newline PVE ends its message with.
	want := "API request failed: create pool failed: cfs-lock 'file-user_cfg' error: no quorum!\n (code: 0)"
	if !strings.HasSuffix(got, want) {
		t.Fatalf("the fake SDK error reads %q, want it to end with %q", got, want)
	}
}

func TestIsCfsLockNoQuorum(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"PVE's 500 for a pool create without quorum", sdkPoolCreateError(500, noQuorumPoolBody), true},
		{"the same answer for another cfs lock", sdkPoolCreateError(500,
			`{"data":null,"message":"cfs-lock 'storage-local' error: no quorum!\n"}`), true},
		{"a 595 never counts", sdkPoolCreateError(595, noQuorumPoolBody), false},
		{"a gateway status never counts", sdkPoolCreateError(502, noQuorumPoolBody), false},
		{"the lock wait's own timeout", sdkPoolCreateError(500,
			`{"data":null,"message":"create pool failed: cfs-lock 'file-user_cfg' error: got lock request timeout\n"}`), false},
		{"a write that failed after the lock", sdkPoolCreateError(500,
			`{"data":null,"message":"error writing config, cfs-lock failed - not quorate\n"}`), false},
		{"the text without an answer from PVE", errors.New(noQuorumPoolText), false},
		{"a canceled request", fmt.Errorf("%w: %w", context.Canceled, sdkPoolCreateError(500, noQuorumPoolBody)), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := IsCfsLockNoQuorum(tc.err); got != tc.want {
			t.Errorf("%s: IsCfsLockNoQuorum = %v, want %v (err %v)", tc.name, got, tc.want, tc.err)
		}
	}
}

// TestAcquireClusterLock_NoQuorumCreateIsARefusal covers PVE refusing the
// sentinel create because the cluster has no quorum. The create never ran, so
// the acquire fails with ErrClusterLockNoQuorum, reads and deletes nothing,
// and never claims the outcome is unknown.
func TestAcquireClusterLock_NoQuorumCreateIsARefusal(t *testing.T) {
	t.Parallel()
	f := newFakeLockPools()
	f.createFn = func(_, _ string) error { return sdkPoolCreateError(500, noQuorumPoolBody) }
	clk := fixedClock(time.Unix(1000, 0), time.Second)
	h, err := acquireClusterLockWithClock(context.Background(), f, "vm-210534", "delete_vm/210534@me",
		25*time.Minute, 10*time.Second, clk)
	if h != nil || err == nil {
		t.Fatalf("want no handle and an error, got handle=%v err=%v", h != nil, err)
	}
	if !errors.Is(err, ErrClusterLockNoQuorum) {
		t.Fatalf("a create refused for lack of quorum must match ErrClusterLockNoQuorum: %v", err)
	}
	if errors.Is(err, ErrClusterLockStateUnknown) || errors.Is(err, ErrClusterLockCreateRefused) {
		t.Fatalf("a refusal for lack of quorum is neither unknown nor a refusal of this identity: %v", err)
	}
	if !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
		t.Fatalf("want a retriable error, got %v", err)
	}
	for _, want := range []string{
		`create sentinel pool "bosh-lock-vm-210534" was refused because the cluster has no quorum`,
		"cfs-lock 'file-user_cfg' error: no quorum!",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error does not say %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "unknown outcome") {
		t.Fatalf("the error calls a refused create an unknown outcome: %v", err)
	}
	if got := strings.Join(f.calls, ","); got != "create:bosh-lock-vm-210534" {
		t.Fatalf("calls = %s, want the create alone with no abandon read or delete", got)
	}
}

// TestAcquireClusterLock_NoQuorumOn595StaysUnknown keeps a 595 with the same
// text unknown, because pveproxy sends 595 itself and the backend's answer is
// not what the client read.
func TestAcquireClusterLock_NoQuorumOn595StaysUnknown(t *testing.T) {
	t.Parallel()
	for _, status := range []int{595, 502} {
		f := newFakeLockPools()
		f.createFn = func(_, _ string) error { return sdkPoolCreateError(status, noQuorumPoolBody) }
		clk := fixedClock(time.Unix(1000, 0), time.Second)
		_, err := acquireClusterLockWithClock(context.Background(), f, "vm-210534", "me", 25*time.Minute, 10*time.Second, clk)
		if !errors.Is(err, ErrClusterLockStateUnknown) || errors.Is(err, ErrClusterLockNoQuorum) {
			t.Fatalf("status %d: want an unknown state, got %v", status, err)
		}
		if f.getN != 1 {
			t.Fatalf("status %d: want the abandon read, got %d reads", status, f.getN)
		}
	}
}

// TestAcquireClusterLock_StealRecreateRefusedForNoQuorum covers the create
// that takes over an expired lock. PVE refused the recreate for lack of
// quorum, so the acquire returns the refusal and makes no abandon read.
func TestAcquireClusterLock_StealRecreateRefusedForNoQuorum(t *testing.T) {
	t.Parallel()
	f := newFakeLockPools()
	f.pools["bosh-lock-vm-90000"] = encodeLockComment("holder-a", time.Unix(500, 0))
	creates := 0
	f.createFn = func(_, _ string) error {
		creates++
		if creates == 2 {
			return sdkPoolCreateError(500, noQuorumPoolBody)
		}
		return nil
	}
	clk := fixedClock(time.Unix(1000, 0), time.Second)
	h, err := acquireClusterLockWithClock(context.Background(), f, "vm-90000", "me", 180*time.Second, 10*time.Second, clk)
	if h != nil || !errors.Is(err, ErrClusterLockNoQuorum) || errors.Is(err, ErrClusterLockStateUnknown) {
		t.Fatalf("want no handle and the no-quorum refusal, got handle=%v err=%v", h != nil, err)
	}
	if !strings.Contains(err.Error(), `steal-recreate lock "bosh-lock-vm-90000" was refused because the cluster has no quorum`) {
		t.Fatalf("the error does not name the refused recreate: %v", err)
	}
	if last := f.calls[len(f.calls)-1]; last != "create:bosh-lock-vm-90000" {
		t.Fatalf("calls = %v, want the refused recreate last with no abandon read", f.calls)
	}
}
