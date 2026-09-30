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
