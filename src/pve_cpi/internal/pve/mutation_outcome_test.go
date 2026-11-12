package pve_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve/endpointpool"
)

// TestIsMutationRefusedOrUnsent pins which mutation errors prove PVE did not
// act on the request. An answer from PVE or a failure that proves nothing was
// sent counts, and every error that leaves the outcome unknown does not.
func TestIsMutationRefusedOrUnsent(t *testing.T) {
	t.Parallel()

	dial := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	read := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}
	dnsErr := &net.DNSError{Err: "no such host", Name: "pve1"}
	connectPhase := &endpointpool.ConnectPhaseError{Phase: endpointpool.PhaseDial, Endpoint: "pve1:8006", Err: dial}
	// sdkConn is the shape the SDK returns for a request whose transport
	// failed: a ConnectionError around the url.Error that net/http built.
	sdkConn := func(cause error) error {
		return &sdkerrors.ConnectionError{Host: "pve1", Port: 8006, Message: "failed to establish a connection",
			Cause: &url.Error{Op: "Post", URL: "https://pve1:8006/api2/json/nodes/pve1/qemu/101/status/reboot",
				Err: cause}}
	}
	unreachable := endpointpool.NewEntryUnreachableError([]endpointpool.EndpointFailure{
		{Endpoint: "pve1:8006", Err: connectPhase},
	})
	// The TLS phases wrap an error that is no dial error, so only the pool's
	// connect-phase tag proves that nothing was sent. A walk whose every hop
	// failed verification unwraps to those errors rather than to a dial error.
	tlsHandshake := &endpointpool.ConnectPhaseError{Phase: endpointpool.PhaseTLSHandshake, Endpoint: "pve1:8006",
		Err: errors.New("tls: handshake failure")}
	tlsVerify := func(endpoint string) error {
		return &endpointpool.ConnectPhaseError{Phase: endpointpool.PhaseTLSVerify, Endpoint: endpoint,
			Err: errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority")}
	}
	unreachableTLS := endpointpool.NewEntryUnreachableError([]endpointpool.EndpointFailure{
		{Endpoint: "pve1:8006", Err: tlsVerify("pve1:8006")},
		{Endpoint: "pve2:8006", Err: tlsVerify("pve2:8006")},
	})
	// loginFailed is the SDK's shape for a ticket login that failed before
	// the request was sent. The login request's own error may say that the
	// login reached PVE, which says nothing about the mutation.
	loginFailed := func(cause error) error {
		return fmt.Errorf("failed to execute QEMU operation: %w", fmt.Errorf("auto-login failed: %w", cause))
	}
	// refreshFailed is the SDK's shape for the forced login after PVE
	// answered the request with a 401, which the SDK does not send again.
	refreshFailed := func(cause error) error {
		return fmt.Errorf("failed to execute QEMU operation: %w", fmt.Errorf("failed to refresh authentication: %w",
			fmt.Errorf("forced re-authentication failed: %w", cause)))
	}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "answered 400", err: &sdkerrors.APIError{HTTPCode: 400, Message: "bad parameter"}, want: true},
		{name: "answered 500", err: &sdkerrors.APIError{HTTPCode: 500, Message: "acpi not available"}, want: true},
		{name: "answered 595 no next hop", err: &sdkerrors.APIError{HTTPCode: 595, Message: "no route"}, want: true},
		{name: "relay 596 broken backend", err: &sdkerrors.APIError{HTTPCode: 596, Message: "broken"}, want: false},
		{name: "gateway 502", err: &sdkerrors.APIError{HTTPCode: 502, Message: "bad gateway"}, want: false},
		{name: "connect phase", err: fmt.Errorf("Post: %w", connectPhase), want: true},
		{name: "dial without the pool", err: fmt.Errorf("Post: %w", dial), want: true},
		{name: "dns failure", err: fmt.Errorf("Post: %w", dnsErr), want: true},
		{name: "maybe sent read", err: &endpointpool.MaybeSentError{Err: fmt.Errorf("Post: %w", read)}, want: false},
		{name: "maybe sent connect phase", err: &endpointpool.MaybeSentError{Err: connectPhase}, want: false},
		{name: "untagged read fault", err: fmt.Errorf("Post: %w", read), want: false},
		{name: "plain error", err: errors.New("something broke"), want: false},
		{name: "deadline", err: fmt.Errorf("Post: %w", context.DeadlineExceeded), want: false},
		{name: "cancelled", err: context.Canceled, want: false},
		{name: "sdk connection error around a dial", err: sdkConn(dial), want: true},
		{name: "sdk connection error around an unreachable pool", err: sdkConn(unreachable), want: true},
		{name: "sdk connection error around a maybe sent connect phase",
			err: sdkConn(&endpointpool.MaybeSentError{Err: connectPhase}), want: false},
		{name: "sdk connection error", err: &sdkerrors.ConnectionError{Host: "pve1", Port: 8006, Message: "EOF"},
			want: false},
		{name: "connect phase tls handshake", err: fmt.Errorf("Post: %w", tlsHandshake), want: true},
		{name: "connect phase tls verify", err: fmt.Errorf("Post: %w", tlsVerify("pve1:8006")), want: true},
		{name: "sdk connection error around an unreachable pool whose every hop failed verification",
			err: sdkConn(unreachableTLS), want: true},
		{name: "ticket login failed on a maybe sent login",
			err: loginFailed(&endpointpool.MaybeSentError{Err: fmt.Errorf("Post: %w", read)}), want: true},
		{name: "ticket login timed out", err: loginFailed(&sdkerrors.TimeoutError{Operation: "POST", Duration: "30s"}),
			want: true},
		{name: "ticket login after a 401 failed", err: refreshFailed(fmt.Errorf("Post: %w", read)), want: true},
		{name: "login text inside a maybe sent error",
			err: &endpointpool.MaybeSentError{Err: errors.New("auto-login failed: x")}, want: false},
		{name: "login prefix in the middle of the text", err: errors.New("Post: auto-login failed: x"), want: false},
		{name: "ticket login failure joined with a maybe sent request",
			err: errors.Join(fmt.Errorf("auto-login failed: %w", read),
				&endpointpool.MaybeSentError{Err: fmt.Errorf("Post: %w", read)}), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pve.IsMutationRefusedOrUnsent(tc.err); got != tc.want {
				t.Errorf("IsMutationRefusedOrUnsent(%v) = %v; want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestAwaitTaskWithLogger_LevelFollowsOutcome checks that only a task that
// resolved with a failing exit status logs "pve: task failed" at Error, and an
// await that ended without an exit status logs at Warn that the outcome is
// unknown and is not a failure.
func TestAwaitTaskWithLogger_LevelFollowsOutcome(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		status    *sdktasks.Status
		waitErr   error
		wantLevel slog.Level
		wantMsg   string
	}{
		{
			name:      "failed task",
			status:    &sdktasks.Status{Status: "stopped", ExitStatus: "ERROR: acpi reboot timed out"},
			wantLevel: slog.LevelError,
			wantMsg:   "pve: task failed",
		},
		{
			name:      "poll ended with no exit status",
			status:    &sdktasks.Status{Status: "running", ExitStatus: ""},
			wantLevel: slog.LevelWarn,
			wantMsg:   "pve: task outcome unknown",
		},
		{
			name:      "poll window ran out",
			waitErr:   fmt.Errorf("task polling canceled: %w", context.DeadlineExceeded),
			wantLevel: slog.LevelWarn,
			wantMsg:   "pve: task outcome unknown",
		},
		{
			name:      "poll 404",
			waitErr:   &sdkerrors.APIError{HTTPCode: 404, Message: "no such task"},
			wantLevel: slog.LevelWarn,
			wantMsg:   "pve: task outcome unknown",
		},
		{
			name:      "poll 403",
			waitErr:   &sdkerrors.APIError{HTTPCode: 403, Message: "Permission check failed"},
			wantLevel: slog.LevelWarn,
			wantMsg:   "pve: task outcome unknown",
		},
		{
			name: "poll fault",
			waitErr: fmt.Errorf("failed to get task status: %w",
				errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority")),
			wantLevel: slog.LevelWarn,
			wantMsg:   "pve: task outcome unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := &mockTasksService{
				waitFn: func(_ context.Context, _, _ string, _ *sdktasks.WaitOptions) (*sdktasks.Status, error) {
					return tc.status, tc.waitErr
				},
			}
			obsLogger, obs := log.NewObservedLogger(log.LevelDebug)
			ctx := pve.WithTestBackoff(context.Background(), func(int) time.Duration { return 0 })
			err := pve.AwaitTaskWithLogger(ctx, newMockClient(svc), "node1", "UPID:node1:abc", obsLogger)
			if err == nil {
				t.Fatal("expected an await error")
			}
			var found bool
			for _, e := range obs.All() {
				if e.Level < slog.LevelWarn {
					continue
				}
				if found {
					t.Errorf("more than one Warn or Error entry: %q", e.Message)
				}
				found = true
				if e.Level != tc.wantLevel || !strings.HasPrefix(e.Message, tc.wantMsg) {
					t.Errorf("entry = %v %q; want %v %q", e.Level, e.Message, tc.wantLevel, tc.wantMsg)
				}
				if tc.wantLevel == slog.LevelWarn && !strings.Contains(e.Message, "not a task failure") {
					t.Errorf("unknown-outcome entry %q does not say it is not a failure", e.Message)
				}
			}
			if !found {
				t.Error("expected a Warn or Error entry for the await error")
			}
		})
	}
}
