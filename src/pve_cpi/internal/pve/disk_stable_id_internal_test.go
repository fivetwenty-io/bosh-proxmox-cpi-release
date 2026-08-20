// disk_stable_id_internal_test.go — white-box tests for the stable disk
// identity helpers and the identity resolver (D13).
package pve

import (
	"context"
	"strings"
	"testing"
)

func TestGenerateDiskStableID_Format(t *testing.T) {
	t.Parallel()
	seen := make(map[string]bool)
	for i := 0; i < 32; i++ {
		id, err := GenerateDiskStableID()
		if err != nil {
			t.Fatalf("GenerateDiskStableID: %v", err)
		}
		if len(id) != DiskStableIDLen {
			t.Fatalf("len(%q) = %d, want %d (PVE drive-serial cap)", id, len(id), DiskStableIDLen)
		}
		if !strings.HasPrefix(id, DiskStableIDPrefix) {
			t.Fatalf("%q lacks the %q prefix", id, DiskStableIDPrefix)
		}
		for _, r := range id[len(DiskStableIDPrefix):] {
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
				t.Fatalf("%q carries a non-lowercase-hex character %q", id, r)
			}
		}
		if seen[id] {
			t.Fatalf("duplicate token %q in 32 draws", id)
		}
		seen[id] = true
	}
}

func TestStableIDFromDriveOptStr(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		optStr string
		want   string
		ok     bool
	}{
		{"local-lvm:vm-100-disk-0", "", false},
		{"local-lvm:vm-100-disk-0,size=10G", "", false},
		{"local-lvm:vm-100-disk-0,serial=bpd-0123456789abcdef", "bpd-0123456789abcdef", true},
		{"local-lvm:vm-100-disk-0,serial=bpd-0123456789abcdef,size=10G", "bpd-0123456789abcdef", true},
		{"local-lvm:vm-100-disk-0,iothread=1,serial=bpd-0123456789abcdef", "bpd-0123456789abcdef", true},
		// An operator- or guest-assigned serial without the prefix is not a
		// CPI identity.
		{"local-lvm:vm-100-disk-0,serial=WD-1234", "", false},
		// A serial= substring inside the volid itself must not match.
		{"weird:serial=bpd-notanoption", "", false},
		{"", "", false},
	} {
		got, ok := StableIDFromDriveOptStr(tc.optStr)
		if got != tc.want || ok != tc.ok {
			t.Errorf("StableIDFromDriveOptStr(%q) = (%q, %v), want (%q, %v)", tc.optStr, got, ok, tc.want, tc.ok)
		}
	}
}

func TestMatchDiskIdentity(t *testing.T) {
	t.Parallel()
	disks := map[string]string{
		"scsi1": "data:vm-700-disk-1,serial=bpd-aaaabbbbccccdddd,size=10G",
		"scsi2": "data:vm-9001-disk-0,size=5G",
	}
	// Volid match wins the entry it names.
	slot, current, ok := matchDiskIdentity(disks, "data:vm-9001-disk-0", "")
	if !ok || slot != "scsi2" || current != "data:vm-9001-disk-0" {
		t.Errorf("volid match = (%q, %q, %v)", slot, current, ok)
	}
	// Serial match finds the renamed volume and reports its CURRENT volid.
	slot, current, ok = matchDiskIdentity(disks, "data:vm-9001-disk-9", "bpd-aaaabbbbccccdddd")
	if !ok || slot != "scsi1" || current != "data:vm-700-disk-1" {
		t.Errorf("serial match = (%q, %q, %v)", slot, current, ok)
	}
	// No serial matching without a stable ID.
	if _, _, ok := matchDiskIdentity(disks, "data:vm-9001-disk-9", ""); ok {
		t.Error("matched without volid or stable ID")
	}
}

func TestResolveDiskIdentity_Matrix(t *testing.T) {
	t.Parallel()

	const birth = "data:vm-9001-disk-0"
	const stableID = "bpd-0011223344556677"
	cfg := ParkerConfig{VMIDRangeStart: 90000, VMIDRangeEnd: 90999, FallbackNode: "pve1", ParkedEnabled: true}

	newClient := func(configs map[int]map[string]any, rows []map[string]any) Client {
		return &scanFakeClient{configs: configs, rows: rows}
	}

	t.Run("legacy CID resolves to birth volid with no API calls", func(t *testing.T) {
		t.Parallel()
		// A nil client proves no call can have been made.
		ident, err := ResolveDiskIdentity(context.Background(), nil, nil, birth, "", cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ident.Volid != birth || ident.Holder.Found || ident.Intent != nil {
			t.Errorf("legacy identity = %+v", ident)
		}
	})

	t.Run("serial hit on a renamed volume", func(t *testing.T) {
		t.Parallel()
		c := newClient(map[int]map[string]any{
			700: {"scsi1": "data:vm-700-disk-1,serial=" + stableID + ",size=10G"},
		}, []map[string]any{clusterRow(700, "")})
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, birth, stableID, cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ident.Volid != "data:vm-700-disk-1" {
			t.Errorf("volid = %q, want the renamed name", ident.Volid)
		}
		if !ident.Holder.Found || ident.Holder.VMID != 700 || ident.Holder.IsParker {
			t.Errorf("holder = %+v", ident.Holder)
		}
	})

	t.Run("serial hit on a parker classifies it as parker with slot", func(t *testing.T) {
		t.Parallel()
		c := newClient(map[int]map[string]any{
			90000: {
				"scsi3":    "data:vm-90000-disk-2,serial=" + stableID,
				cfgKeyTags: "bosh-cpi;bosh-parker",
			},
		}, []map[string]any{clusterRow(90000, "")})
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, birth, stableID, cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !ident.Holder.IsParker || ident.Holder.Slot != "scsi3" || ident.Volid != "data:vm-90000-disk-2" {
			t.Errorf("identity = %+v", ident)
		}
	})

	t.Run("provenance intent fallback when nothing references the volume", func(t *testing.T) {
		t.Parallel()
		desc := `<!--BOSH:{"bosh_parked_disks":{"` + stableID + `":{"disk_cid":"pvd-x","source_vm_cid":"700",` +
			`"parked_at":"2026-08-20T00:00:00Z","node":"pve1","volid":"data:vm-700-disk-1","slot":"scsi4"}}}-->`
		c := newClient(map[int]map[string]any{
			90000: {cfgKeyTags: "bosh-cpi;bosh-parker", "description": desc},
		}, []map[string]any{clusterRow(90000, "")})
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, birth, stableID, cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ident.Intent == nil {
			t.Fatalf("intent = nil, want the recorded transfer intent")
		}
		if ident.Intent.ParkerVMID != 90000 || ident.Intent.Slot != "scsi4" || ident.Intent.SourceVMCID != "700" {
			t.Errorf("intent = %+v", ident.Intent)
		}
		if ident.Volid != "data:vm-700-disk-1" {
			t.Errorf("volid = %q, want the recorded volid", ident.Volid)
		}
	})

	t.Run("birth fallback when nothing matches anywhere", func(t *testing.T) {
		t.Parallel()
		c := newClient(map[int]map[string]any{
			90000: {cfgKeyTags: "bosh-cpi;bosh-parker"},
		}, []map[string]any{clusterRow(90000, "")})
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, birth, stableID, cfg)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ident.Volid != birth || ident.Holder.Found || ident.Intent != nil {
			t.Errorf("identity = %+v, want the birth fallback", ident)
		}
	})
}

func TestFindForeignActiveDiskDetails_SerialAware(t *testing.T) {
	t.Parallel()
	cfg := map[string]any{
		// Renamed persistent disk: named for the owner, but carrying a
		// stable-ID serial — MUST be foreign (delete_vm preserves it).
		"scsi1": "data:vm-700-disk-1,serial=bpd-aaaabbbbccccdddd,size=10G",
		// The VM's own root disk: owner-named, no serial — not foreign.
		"virtio0": "data:vm-700-disk-0,size=20G",
		// Legacy persistent disk: foreign VMID label, no serial.
		"scsi2": "data:vm-9001-disk-0,size=5G",
		// Cloudinit drive: no VMID label at all.
		"ide2": "data:vm-700-cloudinit,media=cdrom",
	}
	got := FindForeignActiveDiskDetails(cfg, 700)
	if len(got) != 2 {
		t.Fatalf("foreign entries = %v, want scsi1 and scsi2", got)
	}
	if e := got["scsi1"]; e.Volid != "data:vm-700-disk-1" || e.StableID != "bpd-aaaabbbbccccdddd" {
		t.Errorf("scsi1 = %+v", e)
	}
	if e := got["scsi2"]; e.Volid != "data:vm-9001-disk-0" || e.StableID != "" {
		t.Errorf("scsi2 = %+v", e)
	}
	// The volid-only wrapper stays in sync.
	flat := FindForeignActiveDisks(cfg, 700)
	if flat["scsi1"] != "data:vm-700-disk-1" || flat["scsi2"] != "data:vm-9001-disk-0" {
		t.Errorf("FindForeignActiveDisks = %v", flat)
	}
}

func TestDiskCIDMeta_IDRoundTripAndCap(t *testing.T) {
	t.Parallel()
	id := "bpd-0123456789abcdef"
	cid, err := EncodeDiskCID("data:vm-1-disk-0", &DiskCIDMeta{ID: id})
	if err != nil {
		t.Fatalf("EncodeDiskCID: %v", err)
	}
	bare, meta, err := ParseEncodedDiskCID(cid)
	if err != nil {
		t.Fatalf("ParseEncodedDiskCID: %v", err)
	}
	if bare != "data:vm-1-disk-0" || meta == nil || meta.ID != id {
		t.Errorf("round trip = (%q, %+v)", bare, meta)
	}

	// An over-cap ID is a hard decode error: no drive entry can carry it, so
	// no scan could ever resolve it.
	over, err := EncodeDiskCID("data:vm-1-disk-0", &DiskCIDMeta{ID: "bpd-00112233445566778899"})
	if err != nil {
		t.Fatalf("EncodeDiskCID: %v", err)
	}
	if _, _, err := ParseEncodedDiskCID(over); err == nil {
		t.Error("expected a decode error for a stable ID over the 20-byte cap")
	}
}
