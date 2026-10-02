package pve

import (
	"context"
	"errors"
	"fmt"
	"testing"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// TestIsConfigDigestRefusalPinsPVEText pins the text qemu-server's
// update_vm_api dies with for a stale digest, "checksum mismatch (file change
// by other user?)" in every 9.x release. If PVE rewords it, the answered row
// fails here, instead of every stale description write quietly poisoning the
// guard. Only an answer counts, so the same text behind a gateway status, a
// transport fault, or an ended context doesn't match, and neither does the
// move handler's refusal.
func TestIsConfigDigestRefusalPinsPVEText(t *testing.T) {
	answered := sdkerrors.ParseAPIError(500, []byte(`{"message":"checksum mismatch (file change by other user?)\n"}`))
	rows := map[string]struct {
		err  error
		want bool
	}{
		"answered":             {answered, true},
		"answered, wrapped":    {fmt.Errorf("cannot persist managed disk provenance: %w", answered), true},
		"gateway":              {sdkerrors.ParseAPIError(502, []byte(`{"message":"checksum mismatch (file change by other user?)\n"}`)), false},
		"relay":                {sdkerrors.ParseAPIError(596, []byte(`{"message":"checksum mismatch (file change by other user?)\n"}`)), false},
		"transport":            {errors.New("checksum mismatch (file change by other user?): connection reset"), false},
		"ended context":        {errors.Join(context.DeadlineExceeded, answered), false},
		"move refusal":         {sdkerrors.ParseAPIError(500, []byte(`{"message":"VM 777: detected modified configuration - file changed by other user? Try again.\n"}`)), false},
		"other answer":         {sdkerrors.ParseAPIError(500, []byte(`{"message":"VM is locked (backup)\n"}`)), false},
		"nil":                  {nil, false},
		"changed wording":      {sdkerrors.ParseAPIError(500, []byte(`{"message":"checksum mismatch (file changed by another user?)\n"}`)), false},
		"changed case":         {sdkerrors.ParseAPIError(500, []byte(`{"message":"Checksum mismatch (file change by other user?)\n"}`)), false},
		"answered 400 with it": {sdkerrors.ParseAPIError(400, []byte(`{"message":"checksum mismatch (file change by other user?)\n"}`)), true},
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			if got := IsConfigDigestRefusal(row.err); got != row.want {
				t.Fatalf("IsConfigDigestRefusal(%v) = %t, want %t", row.err, got, row.want)
			}
		})
	}
}
