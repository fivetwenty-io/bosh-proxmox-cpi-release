package hooks

import (
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/vmcid"
)

func TestHooksReadStampedCIDs(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal("101-0a1b2c3d-4e5f6a7b")
	if err != nil {
		t.Fatal(err)
	}
	if vmid, ok := vmidFromArg(raw); !ok || vmid != 101 {
		t.Errorf("vmidFromArg stamped = %d, %v; want 101, true", vmid, ok)
	}
	if vmid, ok := vmidFromResult([]any{"101-0a1b2c3d-4e5f6a7b", nil}); !ok || vmid != 101 {
		t.Errorf("vmidFromResult stamped = %d, %v; want 101, true", vmid, ok)
	}
	if _, ok := vmidFromResult([]any{"not-a-cid"}); ok {
		t.Error("vmidFromResult accepted a malformed CID")
	}
}

// TestLBServerName pins the server label. A bare CID keeps the label earlier
// releases registered, and a stamped CID adds its cluster stamp, so a VMID two
// clusters both use behind one backend names two servers. A CID with the
// unknown stamp adds its token too.
func TestLBServerName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want string
	}{
		{"101", "vm-101"},
		{"101-0a1b2c3d-4e5f6a7b", "vm-101-0a1b2c3d"},
		{"101-99aabbcc-4e5f6a7b", "vm-101-99aabbcc"},
		{"101-0a1b2c3d-deadbeef", "vm-101-0a1b2c3d"},
		// The unknown stamp names no cluster, so the token keeps two
		// clusters' VMs at one VMID apart.
		{"101-00000000-4e5f6a7b", "vm-101-00000000-4e5f6a7b"},
		{"101-00000000-deadbeef", "vm-101-00000000-deadbeef"},
	}
	for _, tc := range cases {
		cid, err := vmcid.Parse(tc.raw)
		if err != nil {
			t.Fatalf("vmcid.Parse(%q): %v", tc.raw, err)
		}
		if got := lbServerName(cid); got != tc.want {
			t.Errorf("lbServerName(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}

	raw, err := json.Marshal("101-0a1b2c3d-4e5f6a7b")
	if err != nil {
		t.Fatal(err)
	}
	if cid, ok := cidFromArg(raw); !ok || lbServerName(cid) != "vm-101-0a1b2c3d" {
		t.Errorf("cidFromArg stamped = %+v, %v", cid, ok)
	}
	for _, bad := range []string{`101`, `"101-0A1B2C3D-4e5f6a7b"`, `""`} {
		if _, ok := cidFromArg(json.RawMessage(bad)); ok {
			t.Errorf("cidFromArg(%s) accepted a malformed CID", bad)
		}
	}
}
