package handlers

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestParkerLockGapNamesTheCauseOfEveryLockFailure renders the gap for each
// lock failure the CPI raises itself. Each carries no PVE answer, so its
// reason is the whole explanation, and the text must not end in a cause the
// audit description can only call unclassified.
func TestParkerLockGapNamesTheCauseOfEveryLockFailure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"no time to wait", fmt.Errorf("claim: %w", pve.ErrClusterLockNoTimeToWait),
			"the parker's lock bosh-lock-vm-90000 could not be taken to read parker 90000, because the request had no time left to wait for it; retry"},
		{"claim too short", fmt.Errorf("claim: %w", pve.ErrClusterLockClaimTooShort),
			"the parker's lock bosh-lock-vm-90000 could not be taken to read parker 90000, because the CPI confirmed its claim too late to use it; retry"},
		{"create not attempted", fmt.Errorf("claim: %w", pve.ErrMutationNotAttempted),
			"the parker's lock bosh-lock-vm-90000 could not be taken to read parker 90000, because the CPI did not send the lock's create to PVE; retry"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gap := parkerLockGap(90000, tc.err)
			if got := gap.Error(); got != tc.want {
				t.Fatalf("gap text = %q, want %q", got, tc.want)
			}
			if strings.Contains(gap.Error(), "unclassified") {
				t.Fatalf("gap text says unclassified: %q", gap.Error())
			}
			if !errors.Is(gap, tc.err) {
				t.Fatalf("gap does not unwrap to the lock error %v", tc.err)
			}
		})
	}
}
