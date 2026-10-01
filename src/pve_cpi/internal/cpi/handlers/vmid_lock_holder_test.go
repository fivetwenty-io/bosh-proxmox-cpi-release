package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// shortenVMIDLockWait lowers the VMID lock wait for one test.
func shortenVMIDLockWait(t *testing.T, d time.Duration) {
	t.Helper()
	previous := vmidLockTimeout
	vmidLockTimeout = d
	t.Cleanup(func() { vmidLockTimeout = previous })
}

// TestVMIDLockTimeoutNamesTheHolder plants a live claim from another process
// on a VM's lock and checks that the wait that runs out prints that claim in
// full, owner token and expiry, with the expiry also given as a UTC time.
func TestVMIDLockTimeoutNamesTheHolder(t *testing.T) {
	shortenVMIDLockWait(t, 300*time.Millisecond)
	pools := newVMIDLockPools(nil)
	exp := time.Now().Add(20 * time.Minute).Unix()
	owner := "set_vm_metadata/4242@director-0/1234-9f2c1a7e-7"
	pools.pools["bosh-lock-vm-4242"] = fmt.Sprintf("owner=%s exp=%d", owner, exp)

	ran := false
	err := withVMIDLock(t.Context(), pools, 4242, "set_vm_metadata/4242", nil, func() error {
		ran = true
		return nil
	})
	if ran {
		t.Fatal("fn ran while another process held the lock")
	}
	if err == nil {
		t.Fatal("the wait ran out without an error")
	}
	t.Logf("error: %s", err)
	want := fmt.Sprintf(`withVMIDLock: lock "bosh-lock-vm-4242" is held by owner=%s exp=%d, which lapses at %s`,
		owner, exp, time.Unix(exp, 0).UTC().Format(time.RFC3339))
	if !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("error = %q, want it to start with %q", err, want)
	}
	if !errors.Is(err, pve.ErrClusterLockTimeout) {
		t.Fatalf("error %q no longer matches ErrClusterLockTimeout", err)
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || !typed.OkToRetry() {
		t.Fatalf("error %q is not retriable", err)
	}
}

// TestVMIDLockNoTimeToWaitSaysSo covers a request whose deadline leaves less
// than the lock's margin. The acquire gives up before it waits at all, so the
// error must not claim a wait. It says the deadline left no time to wait and
// quotes the holder's claim as a timed-out wait does, or says the lock is free
// when nobody holds it.
func TestVMIDLockNoTimeToWaitSaysSo(t *testing.T) {
	exp := time.Now().Add(20 * time.Minute).Unix()
	owner := "set_vm_metadata/4242@director-0/1234-9f2c1a7e-7"
	const prefix = `withVMIDLock: lock "bosh-lock-vm-4242" was not waited for, because the request's deadline left no time to wait`
	for name, tc := range map[string]struct {
		claim string
		want  string
	}{
		"held": {
			claim: fmt.Sprintf("owner=%s exp=%d", owner, exp),
			want: fmt.Sprintf(prefix+`, and it is held by owner=%s exp=%d, which lapses at %s`,
				owner, exp, time.Unix(exp, 0).UTC().Format(time.RFC3339)),
		},
		"free": {want: prefix + `, and it is free now, so a retry can take it`},
	} {
		t.Run(name, func(t *testing.T) {
			pools := newVMIDLockPools(nil)
			if tc.claim != "" {
				pools.pools["bosh-lock-vm-4242"] = tc.claim
			}
			// Twelve seconds is inside the lock's margin, so the acquire does
			// not wait, and it still leaves the holder read its time.
			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			ran := false
			err := withVMIDLock(ctx, pools, 4242, "set_vm_metadata/4242", nil, func() error {
				ran = true
				return nil
			})
			if ran || err == nil {
				t.Fatalf("the lock was taken with no time to wait: ran=%t err=%v", ran, err)
			}
			if !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to start with %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "throughout the wait") {
				t.Fatalf("error %q claims a wait that never happened", err)
			}
			if !errors.Is(err, pve.ErrClusterLockTimeout) || !cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
				t.Fatalf("error %q lost its retriable timeout cause", err)
			}
		})
	}
}

// failingCommentPools answers every pool comment read with an error.
type failingCommentPools struct{ *vmidLockPools }

func (failingCommentPools) GetPoolComment(context.Context, string) (string, bool, error) {
	return "", false, errors.New("pool read failed")
}

// TestVMIDLockHeldErrorWhenTheClaimCannotBeRead covers the two answers other
// than a readable claim: a sentinel gone by the time we read it, and a read
// that fails. Both keep the timeout as their retriable cause.
func TestVMIDLockHeldErrorWhenTheClaimCannotBeRead(t *testing.T) {
	timeout := cpierrors.WrapAs(pve.ErrClusterLockTimeout, cpierrors.TypeRetriableCloud, "AcquireClusterLock: timed out")
	cases := []struct {
		name  string
		pools pve.PoolService
		want  string
	}{
		{"released", newVMIDLockPools(nil), `withVMIDLock: lock "bosh-lock-vm-7" was held throughout the wait and released after it; a retry can take it`},
		{"unreadable", failingCommentPools{newVMIDLockPools(nil)}, `withVMIDLock: lock "bosh-lock-vm-7" is held by another process, and its claim could not be read`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := vmidLockHeldError(t.Context(), tc.pools, "vm-7", timeout)
			if !strings.HasPrefix(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to start with %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "pool read failed") {
				t.Fatalf("error %q carries the read failure's text", err)
			}
			if !errors.Is(err, pve.ErrClusterLockTimeout) {
				t.Fatalf("error %q lost its timeout cause", err)
			}
		})
	}
}

// hangingCommentPools stands for a PVE node that stops answering. Every
// comment read holds until its context ends, and it records the deadline the
// read carried. So that a read nobody bounded cannot stall the test, it also
// gives up a moment after the request's own deadline.
type hangingCommentPools struct {
	pve.PoolService
	requestDeadline time.Time
	creates         int
	deadlines       []time.Time
}

func (p *hangingCommentPools) CreatePool(context.Context, string, string) error {
	p.creates++
	return errors.New("create pool failed: pool 'bosh-lock-vm-4242' already exists")
}

func (p *hangingCommentPools) GetPoolComment(ctx context.Context, _ string) (string, bool, error) {
	deadline, _ := ctx.Deadline()
	p.deadlines = append(p.deadlines, deadline)
	select {
	case <-ctx.Done():
		return "", false, ctx.Err()
	case <-time.After(time.Until(p.requestDeadline.Add(100 * time.Millisecond))):
		return "", false, errors.New("the read outlived the request")
	}
}

// TestVMIDLockHolderReadEndsBeforeTheRequestDeadline covers the read that
// names a VM lock's holder after the request's deadline left no time to wait.
// That read runs on a detached context, and it must still end the completion
// allowance before the request's deadline, so the error that explains the
// timeout reaches the Director before the dispatcher gives up on the handler.
// With less than the allowance left, the read is skipped.
func TestVMIDLockHolderReadEndsBeforeTheRequestDeadline(t *testing.T) {
	for _, tc := range []struct {
		name     string
		left     time.Duration
		wantRead bool
	}{
		{"less than the allowance left", 3 * time.Second, false},
		{"a moment more than the allowance left", pve.ClusterLockCompletionAllowance + 200*time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), tc.left)
			defer cancel()
			deadline, _ := ctx.Deadline()
			pools := &hangingCommentPools{requestDeadline: deadline}
			err := withVMIDLock(ctx, pools, 4242, "set_vm_metadata/4242", nil, func() error {
				t.Fatal("the body ran without the lock")
				return nil
			})
			returned := time.Now()
			if !returned.Before(deadline) {
				t.Fatalf("the handler returned %v after the request's deadline", returned.Sub(deadline))
			}
			if pools.creates != 0 {
				t.Fatalf("the acquire created the sentinel %d times with no time to wait", pools.creates)
			}
			switch {
			case !tc.wantRead && len(pools.deadlines) != 0:
				t.Fatalf("the holder was read %d times with less than the allowance left", len(pools.deadlines))
			case tc.wantRead && len(pools.deadlines) != 1:
				t.Fatalf("the holder was read %d times, want once", len(pools.deadlines))
			case tc.wantRead && pools.deadlines[0].After(deadline.Add(-pve.ClusterLockCompletionAllowance)):
				t.Fatalf("the holder read ran to %v before the request's deadline, want at least %v",
					deadline.Sub(pools.deadlines[0]), pve.ClusterLockCompletionAllowance)
			}
			want := `withVMIDLock: lock "bosh-lock-vm-4242" was not waited for, because the request's deadline left no time to wait, and its claim could not be read`
			if err == nil || !strings.HasPrefix(err.Error(), want) {
				t.Fatalf("error = %v, want it to start with %q", err, want)
			}
			if !errors.Is(err, pve.ErrClusterLockNoTimeToWait) || !errors.Is(err, pve.ErrClusterLockTimeout) ||
				!cpierrors.IsType(err, cpierrors.TypeRetriableCloud) {
				t.Fatalf("error %q lost its retriable timeout cause", err)
			}
		})
	}
}

// TestDescribeVMIDLockClaimFlattensForeignComments checks that a comment the
// CPI did not write is quoted flat and bounded rather than printed raw.
func TestDescribeVMIDLockClaimFlattensForeignComments(t *testing.T) {
	got := describeVMIDLockClaim("held by hand\n\x1b[31m" + strings.Repeat("x", 400))
	if strings.ContainsAny(got, "\n\x1b") {
		t.Fatalf("claim text carries control characters: %q", got)
	}
	if !strings.HasPrefix(got, "a claim with no readable owner or expiry: held by hand??[31m") {
		t.Fatalf("claim text = %q", got)
	}
	if len(got) > vmidLockClaimLimit+len("a claim with no readable owner or expiry: ")+len("...") {
		t.Fatalf("claim text is %d bytes, over the cap", len(got))
	}
}

// missFirstReadPools reports the sentinel missing on the first read, the way a
// lagging or proxied read can miss a pool that was just created, and answers
// every later read from the store.
type missFirstReadPools struct {
	*vmidLockPools
	reads int
}

func (p *missFirstReadPools) GetPoolComment(ctx context.Context, poolID string) (string, bool, error) {
	p.reads++
	if p.reads == 1 {
		return "", false, nil
	}
	return p.vmidLockPools.GetPoolComment(ctx, poolID)
}

// TestWithVMIDLock_ReadbackThatMissesOurCreateStillRunsTheBody covers a VM's
// metadata lock whose read after its own create misses the new sentinel. The
// next read finds our own live claim, so the lock is ours, the body runs, and
// the sentinel is released afterwards, rather than the wait running out on our
// own claim and leaving it standing for a whole TTL.
func TestWithVMIDLock_ReadbackThatMissesOurCreateStillRunsTheBody(t *testing.T) {
	shortenVMIDLockWait(t, 300*time.Millisecond)
	pools := &missFirstReadPools{vmidLockPools: newVMIDLockPools(nil)}
	ran := false
	err := withVMIDLock(t.Context(), pools, 4242, "set_vm_metadata/4242", nil, func() error {
		ran = true
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("the body did not run under our own lock: ran=%t err=%v", ran, err)
	}
	if left, stands := pools.pools["bosh-lock-vm-4242"]; stands {
		t.Fatalf("the sentinel %q was not released after the body ran", left)
	}
}
