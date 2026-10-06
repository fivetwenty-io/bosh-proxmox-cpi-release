package pve

import (
	"context"
	"errors"
	"testing"
	"time"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// retryHelper is one of the RetryOn* loops, as the context-end tests drive it.
type retryHelper struct {
	name string
	run  func(ctx context.Context, logger *log.Logger, label string, maxAttempts int, op func() error) error
	// retried is an error the helper backs off on and tries again.
	retried error
}

func retryHelpersUnderTest() []retryHelper {
	return []retryHelper{
		{name: "RetryOnTransient", run: RetryOnTransient,
			retried: &sdkerrors.ConnectionError{Host: "pve1", Port: 8006, Message: "connection refused"}},
		{name: "RetryOnTransientOrLock", run: RetryOnTransientOrLock,
			retried: &sdkerrors.APIError{HTTPCode: 500, Message: "can't lock file '/var/lock/qemu-server/lock-100.conf' - got timeout"}},
		{name: "RetryOnTransientOrUnplugBusy", run: RetryOnTransientOrUnplugBusy,
			retried: errors.New("scsi1: hotplug problem - error on hot-unplugging device 'virtioscsi1' - still busy in guest?")},
	}
}

// assertReadsAsTheContextError checks that err, which a retry loop returned
// when its context ended during a backoff, reads exactly as the bare context
// error ctxErr did to every caller that doesn't ask for the last attempt. Its
// text, its errors.Is and errors.As answers, every classifier, and the CPI
// type each wrapper gives it all match ctxErr's.
func assertReadsAsTheContextError(t *testing.T, err, ctxErr error) {
	t.Helper()
	if err.Error() != ctxErr.Error() {
		t.Fatalf("error text = %q, want the context error's %q", err.Error(), ctxErr.Error())
	}
	if !errors.Is(err, ctxErr) {
		t.Fatalf("errors.Is(%v, %v) = false", err, ctxErr)
	}
	classifiers := map[string]func(error) bool{
		"IsStorageLockTimeout":      IsStorageLockTimeout,
		"IsPVEPushback":             IsPVEPushback,
		"IsTransientTransport":      IsTransientTransport,
		"IsClusterNotQuorate":       IsClusterNotQuorate,
		"IsHotUnplugBusy":           IsHotUnplugBusy,
		"IsTransportConnectionDrop": IsTransportConnectionDrop,
		"IsRetryableOrLockFault":    IsRetryableOrLockFault,
		"IsNotFound":                IsNotFound,
		"ProtectionWriteRefused":    ProtectionWriteRefused,
	}
	for name, classify := range classifiers {
		if got, want := classify(err), classify(ctxErr); got != want {
			t.Errorf("%s = %t, want %t as for the bare context error", name, got, want)
		}
	}
	if _, ok := apiHTTPCode(err); ok {
		t.Errorf("apiHTTPCode resolved a status on %v; the bare context error has none", err)
	}
	var apiErr *sdkerrors.APIError
	if errors.As(err, &apiErr) {
		t.Errorf("errors.As found the last attempt's API error on %v", err)
	}
	var connErr *sdkerrors.ConnectionError
	if errors.As(err, &connErr) {
		t.Errorf("errors.As found the last attempt's connection error on %v", err)
	}
	wrappers := map[string]func(error) error{
		"WrapError":           WrapError,
		"WrapMutationError":   WrapMutationError,
		"WrapConfigReadError": WrapConfigReadError,
	}
	for name, wrap := range wrappers {
		got, want := wrap(err), wrap(ctxErr)
		if cpierrors.IsType(got, cpierrors.TypeRetriableCloud) != cpierrors.IsType(want, cpierrors.TypeRetriableCloud) {
			t.Errorf("%s gave %q, want the class it gives the bare context error, %q", name, got, want)
		}
	}
}

// TestRetryHelpers_CancelDuringBackoffKeepsTheLastAttempt cancels the context
// during the backoff after a retried failure. Each helper returns the
// context's error, which still carries the attempt it had just made and reads
// as the bare context error to everyone else.
func TestRetryHelpers_CancelDuringBackoffKeepsTheLastAttempt(t *testing.T) {
	t.Parallel()
	for _, h := range retryHelpersUnderTest() {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(WithTestBackoff(context.Background(), func(int) time.Duration { return time.Hour }))
			defer cancel()
			calls := 0
			err := h.run(ctx, nil, "test", 5, func() error {
				calls++
				cancel()
				return h.retried
			})
			if calls != 1 {
				t.Fatalf("calls = %d, want one attempt before the cancel ended the backoff", calls)
			}
			if err == nil {
				t.Fatal("a cancel during the backoff returned no error")
			}
			assertReadsAsTheContextError(t, err, context.Canceled)
			if last := lastAttemptBeforeContextEnded(err); last != h.retried { //nolint:errorlint // Identity, since the wrapped result also wraps the attempt.
				t.Fatalf("lastAttemptBeforeContextEnded = %v, want %v", last, h.retried)
			}
		})
	}
}

// TestRetryHelpers_DeadlineDuringBackoffKeepsTheLastAttempt is the same check
// for a deadline that fires during the backoff.
func TestRetryHelpers_DeadlineDuringBackoffKeepsTheLastAttempt(t *testing.T) {
	t.Parallel()
	for _, h := range retryHelpersUnderTest() {
		t.Run(h.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(WithTestBackoff(context.Background(), func(int) time.Duration { return time.Hour }), 20*time.Millisecond)
			defer cancel()
			calls := 0
			err := h.run(ctx, nil, "test", 5, func() error {
				calls++
				return h.retried
			})
			if calls != 1 {
				t.Fatalf("calls = %d, want one attempt before the deadline ended the backoff", calls)
			}
			if err == nil {
				t.Fatal("a deadline during the backoff returned no error")
			}
			assertReadsAsTheContextError(t, err, context.DeadlineExceeded)
			if last := lastAttemptBeforeContextEnded(err); last != h.retried { //nolint:errorlint // Identity, since the wrapped result also wraps the attempt.
				t.Fatalf("lastAttemptBeforeContextEnded = %v, want %v", last, h.retried)
			}
		})
	}
}

// TestRetryOnTransientOrLock_LastAttemptKeepsItsOwnOutcome checks that the
// last attempt's error decides whether its outcome is known. PVE's lock
// timeout refusal stays a refusal, and a transport failure with no answer
// stays unknown.
func TestRetryOnTransientOrLock_LastAttemptKeepsItsOwnOutcome(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		attempt error
		refused bool
	}{
		{name: "refused", attempt: &sdkerrors.APIError{HTTPCode: 500, Message: "can't lock file '/var/lock/qemu-server/lock-100.conf' - got timeout"}, refused: true},
		{name: "unanswered", attempt: &sdkerrors.ConnectionError{Host: "pve1", Port: 8006, Message: "connection reset by peer"}, refused: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(WithTestBackoff(context.Background(), func(int) time.Duration { return time.Hour }), 20*time.Millisecond)
			defer cancel()
			err := RetryOnTransientOrLock(ctx, nil, "test", 5, func() error { return tc.attempt })
			if ProtectionWriteRefused(err) {
				t.Fatalf("the returned error %v reads as a refusal; only the last attempt may", err)
			}
			last := lastAttemptBeforeContextEnded(err)
			if last == nil {
				t.Fatalf("lastAttemptBeforeContextEnded(%v) found no last attempt", err)
			}
			if got := ProtectionWriteRefused(last); got != tc.refused {
				t.Fatalf("ProtectionWriteRefused(last attempt %v) = %t, want %t", last, got, tc.refused)
			}
		})
	}
}

// TestLastAttemptBeforeContextEnded_OnlyForABackoffTheContextEnded checks
// that only a loop whose context ended during a backoff reports a last
// attempt. A context error an attempt returned itself, an exhausted loop, and
// an error the loop doesn't retry all report none.
func TestLastAttemptBeforeContextEnded_OnlyForABackoffTheContextEnded(t *testing.T) {
	t.Parallel()
	ctx := WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
	lockErr := &sdkerrors.APIError{HTTPCode: 500, Message: "can't lock file '/var/lock/qemu-server/lock-100.conf' - got timeout"}
	cases := map[string]error{
		"attempt returned the context error": RetryOnTransientOrLock(ctx, nil, "test", 3, func() error { return context.Canceled }),
		"attempts exhausted":                 RetryOnTransientOrLock(ctx, nil, "test", 3, func() error { return lockErr }),
		"error not retried":                  RetryOnTransientOrLock(ctx, nil, "test", 3, func() error { return errors.New("permanent") }),
	}
	for name, err := range cases {
		if last := lastAttemptBeforeContextEnded(err); last != nil {
			t.Errorf("%s: lastAttemptBeforeContextEnded(%v) = %v, want nil", name, err, last)
		}
	}
}
