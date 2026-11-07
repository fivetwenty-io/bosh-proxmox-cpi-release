package handlers

import (
	"testing"
	"time"
)

// TestVMIDLockTTLCoversSetDiskMetadataMotionWait checks that set_disk_metadata,
// which waits out a migration while it holds the VM-ID lock, claims the lock
// for long enough to cover both of its config mutations at the motion wait.
func TestVMIDLockTTLCoversSetDiskMetadataMotionWait(t *testing.T) {
	t.Parallel()
	for _, wait := range []time.Duration{time.Second, time.Minute, 10 * time.Minute} {
		var body vmidLockBody
		for _, b := range vmidLockBodies(wait) {
			if b.operation == "set_disk_metadata" {
				body = b
			}
		}
		if body.operation == "" {
			t.Fatal("set_disk_metadata is missing from vmidLockBodies")
		}
		if body.bounded != setDiskMetadataMutationTime(wait) {
			t.Fatalf("motion wait %s: bounded = %s, want %s", wait, body.bounded, setDiskMetadataMutationTime(wait))
		}
		if floor := 2 * 2 * wait; body.bounded < floor {
			t.Fatalf("motion wait %s: bounded = %s, want at least both mutations' budgets, %s", wait, body.bounded, floor)
		}
		if got, want := vmidLockTTLFor(wait), body.budget()+vmidLockTTLMargin; got < want {
			t.Fatalf("motion wait %s: TTL = %s, want at least the body budget plus margin, %s", wait, got, want)
		}
	}
}

// TestSetDiskMetadataMutationTimeIsZeroWithoutAMotionWait checks that a body
// that waits for no motion adds nothing to the lock budget.
func TestSetDiskMetadataMutationTimeIsZeroWithoutAMotionWait(t *testing.T) {
	t.Parallel()
	if got := setDiskMetadataMutationTime(0); got != 0 {
		t.Fatalf("setDiskMetadataMutationTime(0) = %s, want 0", got)
	}
	if got := setDiskMetadataMutationTime(-time.Second); got != 0 {
		t.Fatalf("setDiskMetadataMutationTime(-1s) = %s, want 0", got)
	}
}
