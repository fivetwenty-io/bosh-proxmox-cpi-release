package pve_test

import (
	"testing"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// TestWrapError_4xxTypedWrappersArePermanent drives WrapError with the real
// ParseAPIError output for the three codes the SDK does NOT return as
// *APIError: 400 dispatches to *ParameterError, 401 to *AuthenticationError,
// and 403 to *PermissionError, each embedding APIError by value. A bare
// errors.As against *APIError misses all three, so before the fix these fell
// through to the text classifiers, where a body containing a pushback phrase
// ("got timeout", "unable to acquire lock", "worker busy") silently flipped a
// request verdict to retriable. Each case here carries exactly such a body:
// on unfixed code the classification comes back retriable.
func TestWrapError_4xxTypedWrappersArePermanent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"400 parameter error with pushback phrase", 400, `{"message":"parameter verification failed: got timeout"}`},
		{"401 auth error with lock phrase", 401, `{"message":"authentication failure: unable to acquire lock"}`},
		{"403 permission error with worker phrase", 403, `{"message":"permission check failed: worker busy"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := sdkerrors.ParseAPIError(tc.status, []byte(tc.body))

			wrapped := pve.WrapError(err)
			if wrapped == nil {
				t.Fatalf("WrapError(%d) = nil, want an error", tc.status)
			}

			if cpierrors.IsType(wrapped, cpierrors.TypeRetriableCloud) {
				t.Errorf("WrapError(%d %s) = retriable %v, want permanent: a 4xx is a verdict about the request, whatever its body says",
					tc.status, tc.body, wrapped)
			}

			if !cpierrors.IsType(wrapped, cpierrors.TypeCloud) {
				t.Errorf("WrapError(%d) = %v, want TypeCloud", tc.status, wrapped)
			}
		})
	}
}

// TestWrapError_StatusClassesUnchanged pins the surrounding behavior the 4xx
// fix must not disturb: 404 stays a permanent not-found, 5xx stays retriable,
// and 429 stays retriable pushback.
func TestWrapError_StatusClassesUnchanged(t *testing.T) {
	t.Parallel()

	notFound := pve.WrapError(sdkerrors.ParseAPIError(404, []byte(`{"message":"no such vm"}`)))
	if cpierrors.IsType(notFound, cpierrors.TypeRetriableCloud) {
		t.Errorf("404 = %v, want non-retriable", notFound)
	}

	server := pve.WrapError(sdkerrors.ParseAPIError(503, []byte(`{"message":"service unavailable"}`)))
	if !cpierrors.IsType(server, cpierrors.TypeRetriableCloud) {
		t.Errorf("503 = %v, want retriable", server)
	}

	pushback := pve.WrapError(sdkerrors.ParseAPIError(429, []byte(`{"message":"too many requests"}`)))
	if !cpierrors.IsType(pushback, cpierrors.TypeRetriableCloud) {
		t.Errorf("429 = %v, want retriable", pushback)
	}
}
