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

// TestIsAnsweredConfigRefusalCountsOnlyAnswers pins which failures of a
// configuration update count as PVE refusing it outright. A 4xx status and a
// 500 that says the VM is locked do. A gateway or relay status, a transport
// fault, an ended context, and any other 500 may hide a write that landed, so
// they don't.
func TestIsAnsweredConfigRefusalCountsOnlyAnswers(t *testing.T) {
	locked := sdkerrors.ParseAPIError(500, []byte(`{"message":"VM 777 is locked (backup)\n"}`))
	forbidden := sdkerrors.ParseAPIError(403, []byte(`{"message":"Permission check failed (/vms/777, VM.Config.Options)\n"}`))
	rows := map[string]struct {
		err  error
		want bool
	}{
		"forbidden":          {forbidden, true},
		"forbidden, wrapped": {fmt.Errorf("cannot remove description notes: %w", forbidden), true},
		"bad parameter":      {sdkerrors.ParseAPIError(400, []byte(`{"message":"parameter verification failed\n"}`)), true},
		"locked":             {locked, true},
		"other 500":          {sdkerrors.ParseAPIError(500, []byte(`{"message":"unable to write config\n"}`)), false},
		"gateway":            {sdkerrors.ParseAPIError(502, []byte(`{"message":"VM 777 is locked (backup)\n"}`)), false},
		"unavailable":        {sdkerrors.ParseAPIError(503, nil), false},
		"gateway timeout":    {sdkerrors.ParseAPIError(504, nil), false},
		"relay":              {sdkerrors.ParseAPIError(596, []byte(`{"message":"VM 777 is locked (backup)\n"}`)), false},
		"transport":          {errors.New("VM 777 is locked (backup): connection reset"), false},
		"ended context":      {errors.Join(context.Canceled, forbidden), false},
		"nil":                {nil, false},
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			if got := IsAnsweredConfigRefusal(row.err); got != row.want {
				t.Fatalf("IsAnsweredConfigRefusal(%v) = %t, want %t", row.err, got, row.want)
			}
		})
	}
}
