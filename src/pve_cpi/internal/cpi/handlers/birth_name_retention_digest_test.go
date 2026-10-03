package handlers

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestRetainedOwnSlotRequiresTheConfigItRead pins legacy retention's own-slot
// acceptance to the config retention read. The scan reads every guest's config
// again, so retention refuses when an entry's digest differs from that read,
// or when the read has no digest at all, instead of trusting a list built
// from another config.
func TestRetainedOwnSlotRequiresTheConfigItRead(t *testing.T) {
	const volume = "a:vm-777-ephemeral-0"
	meta := &pve.DiskCIDMeta{ID: "bpd-0011223344556677"}
	held := func(digest string) *pve.DiskBirthNameHeldError {
		return &pve.DiskBirthNameHeldError{StableID: meta.ID, BirthVolid: volume, Holders: []pve.BirthNameHolder{{
			VolumeReference: pve.VolumeReference{VMID: 777, Node: "n1", Slot: "scsi2"}, Digest: digest,
		}}}
	}
	cfg := map[string]any{"digest": "5", "scsi2": volume + ",size=5G"}
	rd, err := retainedOwnSlotIdentity(held("5"), cfg, "n1", 777, "cid", volume, meta)
	if err != nil || rd.holder == nil || rd.holder.VMID != 777 || rd.holder.Node != "n1" {
		t.Fatalf("retainedOwnSlotIdentity = %+v, %v; want 777's own slot", rd, err)
	}
	for name, tc := range map[string]struct {
		digest string
		cfg    map[string]any
	}{
		"a later config":          {"6", cfg},
		"an entry with no digest": {"", cfg},
		"a read with no digest":   {"", map[string]any{"scsi2": volume + ",size=5G"}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := retainedOwnSlotIdentity(held(tc.digest), tc.cfg, "n1", 777, "cid", volume, meta)
			if err == nil || !strings.Contains(err.Error(), "retained ephemeral config changed while its volume was resolved") {
				t.Fatalf("retainedOwnSlotIdentity = %v, want the changed-config refusal", err)
			}
		})
	}
}
