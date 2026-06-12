// Unit tests for isSDNConflict against the error shapes PVE actually emits.
//
// PVE rejects duplicate SDN object creation with HTTP 500 and a message-level
// "sdn <kind> object ID '<id>' already defined" — NOT an HTTP 409. The
// APIError fixtures below mirror responses captured from a live PVE 9 cluster
// (POST /cluster/sdn/vnets and POST /cluster/sdn/vnets/{vnet}/subnets).
package handlers

import (
	"fmt"
	"testing"

	pveerr "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/errors"
)

func TestIsSDNConflict(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{
			name: "sentinel ErrConflict",
			err:  pveerr.ErrConflict,
			want: true,
		},
		{
			name: "wrapped sentinel ErrConflict",
			err:  fmt.Errorf("subnet conflict: %w", pveerr.ErrConflict),
			want: true,
		},
		{
			name: "APIError 409",
			err:  &pveerr.APIError{Code: 409, Message: "Conflict"},
			want: true,
		},
		{
			name: "APIError HTTPCode 409",
			err:  &pveerr.APIError{HTTPCode: 409, Message: "Conflict"},
			want: true,
		},
		{
			// Live PVE shape: duplicate subnet create. HTTP 500, code 0,
			// "already defined" only in the message.
			name: "live subnet already defined (HTTP 500)",
			err: fmt.Errorf("HTTP POST request failed: %w", &pveerr.APIError{
				Code:     0,
				HTTPCode: 500,
				Message:  "create sdn subnet object failed: sdn subnet object ID 'cpitest-10.252.0.0-24' already defined\n",
			}),
			want: true,
		},
		{
			// Live PVE shape: duplicate vnet create.
			name: "live vnet already defined (HTTP 500)",
			err: fmt.Errorf("HTTP POST request failed: %w", &pveerr.APIError{
				Code:     0,
				HTTPCode: 500,
				Message:  "create sdn vnet object failed: sdn vnet object ID 'cpival55' already defined\n",
			}),
			want: true,
		},
		{
			// Wrapped without APIError in the chain — message-only fallback.
			name: "already defined in message only",
			err:  fmt.Errorf("API request failed: sdn zone object ID 'z1' already defined"),
			want: true,
		},
		{
			name: "unrelated 500",
			err: &pveerr.APIError{
				HTTPCode: 500,
				Message:  "create sdn subnet object failed: ipam error",
			},
			want: false,
		},
		{
			name: "unrelated error",
			err:  fmt.Errorf("connection refused"),
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isSDNConflict(tc.err); got != tc.want {
				t.Fatalf("isSDNConflict(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
