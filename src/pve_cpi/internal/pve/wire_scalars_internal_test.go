package pve

import (
	"context"
	"testing"
	"time"
)

// TestStorageInfoCache_SharedFlagWireShapes feeds parseStorageEntry every
// encoding PVE has been seen to use for the "shared" flag. A plain *int field
// rejected the quoted and boolean forms, and the rejected row was skipped, so
// the pool read as unknown.
func TestStorageInfoCache_SharedFlagWireShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		shared     any
		wantShared bool
	}{
		{"integer-one", 1, true},
		{"quoted-one", "1", true},
		{"boolean-true", true, true},
		{"integer-zero", 0, false},
		{"quoted-zero", "0", false},
		{"boolean-false", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lister := &fakeLister{entries: []map[string]any{
				{"storage": "vg0", "type": "lvm", "shared": c.shared},
			}}
			cache := NewStorageInfoCache(lister, time.Minute)
			info, err := cache.Get(context.Background(), "vg0")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got := info.IsShared(); got != c.wantShared {
				t.Fatalf("IsShared() = %v, want %v for shared=%v", got, c.wantShared, c.shared)
			}
		})
	}
}

func TestListGuestsAuthoritative_QuotedVMIDIsStillAGuest(t *testing.T) {
	t.Parallel()
	c := &lgClient{
		cluster: &lgCluster{nodes: nil},
		nodes: &lgNodes{
			listNodesFn: mnSoloNodesList,
			listings: map[string][]string{"solo": {
				`{"vmid": "596", "name": "quoted"}`,
				`{"vmid": 597, "name": "bare"}`,
			}},
		},
	}
	guests, err := ListGuestsAuthoritative(lgCtx(), c, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(guests) != 2 || guests[0].VMID != 596 || guests[1].VMID != 597 {
		t.Fatalf("guests = %+v; want both the quoted and the bare vmid rows", guests)
	}
}
