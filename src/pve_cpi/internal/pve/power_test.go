package pve_test

import (
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func TestUPIDFromRaw(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     json.RawMessage
		want    string
		wantErr bool
	}{
		{
			name: "bare string UPID",
			raw:  json.RawMessage(`"UPID:pve:00001234:DEADBEEF:67890ABC:qmreboot:101:root@pam:"`),
			want: "UPID:pve:00001234:DEADBEEF:67890ABC:qmreboot:101:root@pam:",
		},
		{
			name: "empty raw",
			raw:  json.RawMessage(nil),
			want: "",
		},
		{
			name: "zero-length raw",
			raw:  json.RawMessage{},
			want: "",
		},
		{
			name: "object with upid field",
			raw:  json.RawMessage(`{"upid":"UPID:x","other":"ignored"}`),
			want: "UPID:x",
		},
		{
			name: "object without upid field",
			raw:  json.RawMessage(`{"status":"running"}`),
			want: "",
		},
		{
			name:    "unparseable garbage",
			raw:     json.RawMessage(`{`),
			wantErr: true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := pve.UPIDFromRaw(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("UPIDFromRaw(%s): expected error, got nil", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("UPIDFromRaw(%s): unexpected error: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("UPIDFromRaw(%s) = %q; want %q", tc.raw, got, tc.want)
			}
		})
	}
}
