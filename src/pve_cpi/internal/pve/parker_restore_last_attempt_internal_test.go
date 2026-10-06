package pve

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// lastAttemptRestoreTimeout is the protection restore deadline these tests
// set. lastAttemptRestoreBackoff is far longer, so the deadline always ends
// the restore while its retry loop waits out the backoff after a failed
// attempt, never during an attempt.
const (
	lastAttemptRestoreTimeout = 100 * time.Millisecond
	lastAttemptRestoreBackoff = time.Minute
)

// parkerLockTimeout is PVE's answer when the parker's config lock is held:
// a 500 whose text IsStorageLockTimeout matches, so the retry loop backs off
// and tries again.
func parkerLockTimeout() error {
	return &sdkerrors.APIError{HTTPCode: 500, Message: "can't lock file '/var/lock/qemu-server/lock-90000.conf' - got timeout"}
}

// lastAttemptRestoreContext returns a request context whose protection
// restore ends at lastAttemptRestoreTimeout while its retry loop sleeps.
func lastAttemptRestoreContext() context.Context {
	ctx := WithParkerProtectionRestoreTimeoutForTest(context.Background(), lastAttemptRestoreTimeout)
	return WithTestBackoff(ctx, func(int) time.Duration { return lastAttemptRestoreBackoff })
}

// TestRestoreParkerProtection_RefusalBeforeTheDeadlineStaysARefusal checks a
// restore that PVE refuses with a lock timeout, whose deadline then ends the
// retry loop during the backoff. No write went out after the refusal, so the
// restore's outcome is that refusal. It warns that protection is off and
// returns nil, as any restore PVE refused does, and never says the outcome is
// unknown.
func TestRestoreParkerProtection_RefusalBeforeTheDeadlineStaysARefusal(t *testing.T) {
	t.Parallel()
	c := &hangingRestoreClient{parker: 90000, refuse: parkerLockTimeout(),
		scanFakeClient: newScanFakeClient(map[int]map[string]any{90000: {cfgKeyTags: "bosh-parker"}})}
	logger, logged := newRestoreTestLogger(t)
	ctx := lastAttemptRestoreContext()
	err, elapsed := callWithCeiling(t, func() error {
		return restoreParkerProtection(ctx, c, logger, "delete parked", "pve1", 90000, `the deletion of "data:vm-90000-disk-2" completed`)
	})
	if elapsed < lastAttemptRestoreTimeout {
		t.Fatalf("returned after %s, before the %s restore deadline ended the backoff", elapsed, lastAttemptRestoreTimeout)
	}
	if err != nil {
		t.Fatalf("a restore PVE refused before its deadline returned %v, want nil", err)
	}
	lines := restoreLogLines(logged.String())
	if len(lines) != 1 {
		t.Fatalf("logged %d restore warnings, want one: %s", len(lines), logged.String())
	}
	for _, want := range []string{"delete parked: could not restore protection on parker", "got timeout"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("restore warning %s does not contain %q", lines[0], want)
		}
	}
	if strings.Contains(lines[0], "outcome is unknown") {
		t.Fatalf("restore warning %s calls a refused write's outcome unknown", lines[0])
	}
}

// TestRestoreParkerProtectionLogged_RefusalBeforeTheDeadlineStaysARefusal is
// the same check for the restore whose contract is to log its result.
func TestRestoreParkerProtectionLogged_RefusalBeforeTheDeadlineStaysARefusal(t *testing.T) {
	t.Parallel()
	c := &hangingRestoreClient{parker: 90000, refuse: parkerLockTimeout(),
		scanFakeClient: newScanFakeClient(map[int]map[string]any{90000: {cfgKeyTags: "bosh-parker"}})}
	logger, logged := newRestoreTestLogger(t)
	ctx := lastAttemptRestoreContext()
	_, elapsed := callWithCeiling(t, func() struct{} {
		restoreParkerProtectionLogged(ctx, c, logger, "UnparkDisk", "pve1", 90000, parkerWindowLockCheck(ctx, 90000))
		return struct{}{}
	})
	if elapsed < lastAttemptRestoreTimeout {
		t.Fatalf("returned after %s, before the %s restore deadline ended the backoff", elapsed, lastAttemptRestoreTimeout)
	}
	lines := restoreLogLines(logged.String())
	if len(lines) != 1 {
		t.Fatalf("logged %d restore warnings, want one: %s", len(lines), logged.String())
	}
	for _, want := range []string{"UnparkDisk: could not restore protection on parker", "got timeout"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("restore warning %s does not contain %q", lines[0], want)
		}
	}
	if strings.Contains(lines[0], "outcome is unknown") {
		t.Fatalf("restore warning %s calls a refused write's outcome unknown", lines[0])
	}
}

// TestRestoreParkerProtection_UnansweredAttemptBeforeTheDeadlineStaysUnknown
// checks the other side. A restore whose last attempt failed in transport,
// with no answer from PVE, may have applied, so when the deadline ends the
// backoff after it, the restore is still cut off and its outcome is unknown.
func TestRestoreParkerProtection_UnansweredAttemptBeforeTheDeadlineStaysUnknown(t *testing.T) {
	t.Parallel()
	c := &hangingRestoreClient{parker: 90000, dropped: true,
		scanFakeClient: newScanFakeClient(map[int]map[string]any{90000: {cfgKeyTags: "bosh-parker"}})}
	logger, logged := newRestoreTestLogger(t)
	ctx := lastAttemptRestoreContext()
	err, elapsed := callWithCeiling(t, func() error {
		return restoreParkerProtection(ctx, c, logger, "delete parked", "pve1", 90000, `the deletion of "data:vm-90000-disk-2" completed`)
	})
	if elapsed < lastAttemptRestoreTimeout {
		t.Fatalf("returned after %s, before the %s restore deadline ended the backoff", elapsed, lastAttemptRestoreTimeout)
	}
	if restores, _ := c.outcome(); restores != 1 {
		t.Fatalf("restore attempts = %d, want one before the deadline ended the backoff", restores)
	}
	var cutOff *ProtectionRestoreCutOffError
	if !errors.As(err, &cutOff) || cutOff.ParkerVMID != 90000 {
		t.Fatalf("error %v is not a cut-off restore for parker 90000", err)
	}
	if !strings.Contains(err.Error(), "did not finish within") || !strings.Contains(err.Error(), "outcome is unknown") {
		t.Fatalf("error %q does not say the restore's outcome is unknown", err)
	}
	lines := restoreLogLines(logged.String())
	if len(lines) != 1 || !strings.Contains(lines[0], "outcome is unknown") {
		t.Fatalf("want one warning that the restore's outcome is unknown, got: %s", logged.String())
	}
}
