// disk_birth_name_variant_internal_test.go pins the typed refusal and the
// name-matching variant that the birth-name resolution added.
package pve

import (
	"context"
	"testing"
)

// TestBirthNameRefusalIsTyped shows that a caller can tell the refusal apart
// through the wraps, and that it lists every entry in VMID and slot order.
func TestBirthNameRefusalIsTyped(t *testing.T) {
	t.Parallel()
	c := &scanFakeClient{configs: map[int]map[string]any{
		701: {"scsi3": birthNameVolid + ",serial=" + birthNameOther},
		700: {"unused1": birthNameVolid},
	}, rows: []map[string]any{clusterRow(701, ""), clusterRow(700, "")}}
	_, err := ResolveDiskIdentity(context.Background(), c, nil, birthNameVolid, birthNameToken, birthNameParkerCfg)
	held, ok := IsDiskBirthNameHeld(err)
	if !ok {
		t.Fatalf("error = %v, want a DiskBirthNameHeldError", err)
	}
	if held.StableID != birthNameToken || held.BirthVolid != birthNameVolid || len(held.Holders) != 2 {
		t.Fatalf("refusal = %+v", held)
	}
	first, second := held.Holders[0], held.Holders[1]
	if first.VMID != 700 || first.Slot != "unused1" || !first.Unused || second.VMID != 701 || second.Slot != "scsi3" || second.Serial != birthNameOther {
		t.Fatalf("holders = %+v, want 700's unused1 and then 701's scsi3", held.Holders)
	}
}

// TestBirthNameMatchingNameVariantKeepsNameSemantics shows that the variant
// the allocation collision check uses still matches a slot by name under any
// serial, reports an unused entry with no note, and never refuses.
func TestBirthNameMatchingNameVariantKeepsNameSemantics(t *testing.T) {
	t.Parallel()
	slot := &scanFakeClient{configs: map[int]map[string]any{
		700: {"scsi1": birthNameVolid + ",serial=" + birthNameOther},
	}, rows: []map[string]any{clusterRow(700, "")}}
	ident, err := ResolveDiskIdentityMatchingName(context.Background(), slot, nil, birthNameVolid, birthNameToken, birthNameParkerCfg)
	if err != nil || !ident.Holder.Found || ident.Holder.VMID != 700 || ident.Volid != birthNameVolid {
		t.Fatalf("slot by name = %+v, %v, want 700 as the holder", ident, err)
	}
	unused := &scanFakeClient{configs: map[int]map[string]any{
		777: {"unused0": birthNameVolid},
	}, rows: []map[string]any{clusterRow(777, "")}}
	ident, err = ResolveDiskIdentityMatchingName(context.Background(), unused, nil, birthNameVolid, birthNameToken, birthNameParkerCfg)
	if err != nil || len(ident.Unused) != 1 || ident.Unused[0].VMID != 777 {
		t.Fatalf("unused by name = %+v, %v, want 777's unused0", ident, err)
	}
}

// TestMatchDiskIdentityIgnoresTheNameOfAStableIDDisk pins the matcher itself.
// With a stable ID only the serial matches, and the name matches only in the
// name-matching mode.
func TestMatchDiskIdentityIgnoresTheNameOfAStableIDDisk(t *testing.T) {
	t.Parallel()
	disks := map[string]string{"scsi1": birthNameVolid + ",serial=" + birthNameOther}
	if slot, _, ok := matchDiskIdentityAs(disks, birthNameVolid, birthNameToken, matchSerial); ok {
		t.Fatalf("matched %s by name for a stable-ID disk", slot)
	}
	if slot, current, ok := matchDiskIdentityAs(disks, birthNameVolid, birthNameToken, matchNameOrSerial); !ok || slot != "scsi1" || current != birthNameVolid {
		t.Fatalf("name-matching mode = (%q, %q, %v)", slot, current, ok)
	}
}

// TestBirthNameHoldersCarryTheConfigDigest shows that each entry in the
// refusal carries the digest of the config the scan read it from, a slot
// and an unused entry alike, so a caller can check it against its own read.
func TestBirthNameHoldersCarryTheConfigDigest(t *testing.T) {
	t.Parallel()
	c := &scanFakeClient{configs: map[int]map[string]any{
		700: {"digest": "d700", "scsi1": birthNameVolid + ",size=5G"},
		701: {"digest": "d701", "unused0": birthNameVolid},
	}, rows: []map[string]any{clusterRow(700, ""), clusterRow(701, "")}}
	_, err := ResolveDiskIdentity(context.Background(), c, nil, birthNameVolid, birthNameToken, birthNameParkerCfg)
	held, ok := IsDiskBirthNameHeld(err)
	if !ok || len(held.Holders) != 2 {
		t.Fatalf("error = %v, want a refusal listing two entries", err)
	}
	if held.Holders[0].Digest != "d700" || held.Holders[1].Digest != "d701" {
		t.Fatalf("holders = %+v, want 700's entry with d700 and 701's with d701", held.Holders)
	}
}
