package handlers

import (
	"context"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// TestAnchorMissingRefusal_Matrix covers the decision table of the
// anchor-missing invariant: the refusal fires only for a disk whose CID
// promises a parker anchor, with no holder found, under the parked strategy,
// with strict enforcement (the default). Every other combination is
// permissive.
func TestAnchorMissingRefusal_Matrix(t *testing.T) {
	t.Parallel()

	anchorMeta := &pve.DiskCIDMeta{Anchor: true}
	noAnchorMeta := &pve.DiskCIDMeta{}
	holderFound := pve.DiskHolder{Found: true, VMID: 90000, Node: "pve1", IsParker: true, Slot: "scsi0"}
	noHolder := pve.DiskHolder{}
	strictOff := false

	cases := []struct {
		name       string
		meta       *pve.DiskCIDMeta
		holder     pve.DiskHolder
		strategy   string
		strict     *bool
		wantRefuse bool
	}{
		{"promise, no holder, parked, strict default", anchorMeta, noHolder, "parked", nil, true},
		{"promise, no holder, parked, strict off", anchorMeta, noHolder, "parked", &strictOff, false},
		{"promise, no holder, free strategy", anchorMeta, noHolder, "free", nil, false},
		{"promise, holder found", anchorMeta, holderFound, "parked", nil, false},
		{"no promise, no holder", noAnchorMeta, noHolder, "parked", nil, false},
		{"nil meta (legacy CID), no holder", nil, noHolder, "parked", nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps := Deps{
				Config: &config.CPIConfig{
					Node:                     "pve1",
					DetachedDiskStrategy:     tc.strategy,
					ParkedDiskVMIDRangeStart: 90000,
					ParkedDiskVMIDRangeEnd:   90999,
					ParkedAnchorStrict:       tc.strict,
				},
				Logger: log.NewNopLogger(),
			}
			err := anchorMissingRefusal(context.Background(), deps, "attach_disk", "pvd-test", tc.meta, tc.holder)
			if tc.wantRefuse {
				if err == nil {
					t.Fatal("expected the anchor-missing refusal, got nil")
				}
				if !strings.Contains(err.Error(), "parked_anchor_strict") {
					t.Errorf("refusal must name the escape hatch, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected permissive nil, got: %v", err)
			}
		})
	}
}
