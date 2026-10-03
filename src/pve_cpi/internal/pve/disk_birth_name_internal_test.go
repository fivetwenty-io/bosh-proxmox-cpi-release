// disk_birth_name_internal_test.go covers a stable-ID disk whose birth volume
// name another volume now holds. A move renames the disk off its birth name,
// and a later disk can take that name, so the resolver matches a slot only by
// the disk's serial and refuses the disk when nothing but such a name holder
// is left. A transfer onto a parker can crash after its move lands and before
// its serial write, and the landed volume can then take the disk's own birth
// name back. While the parker keeps the disk's transfer record, resolution
// hands that record to the resume and never refuses the disk.
package pve

import (
	"context"
	"errors"
	"strings"
	"testing"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

const (
	birthNameVolid  = "data:vm-9001-disk-0"
	birthNameToken  = "bpd-0011223344556677"
	birthNameOther  = "bpd-8899aabbccddeeff"
	birthNameParked = "data:vm-90000-disk-0"
)

var birthNameParkerCfg = ParkerConfig{VMIDRangeStart: 90000, VMIDRangeEnd: 90999, FallbackNode: "pve1", ParkedEnabled: true}

// requireBirthNameRefusal fails unless err is the permanent refusal that names
// the birth volume and every entry in want.
func requireBirthNameRefusal(t *testing.T, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("the resolver resolved the disk, want the refusal of an entry that only names its birth volume")
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || typed.Type() != cpierrors.TypeCloud || typed.OkToRetry() {
		t.Fatalf("error = %v, want a permanent CloudError", err)
	}
	for _, text := range append([]string{"its birth volume " + birthNameVolid, birthNameToken}, want...) {
		if !strings.Contains(err.Error(), text) {
			t.Fatalf("error = %v\nwant it to contain %q", err, text)
		}
	}
}

// TestBirthNameGuestOrderResolvesTheParkedDisk puts the disk on a parker with
// its serial and a serial-less volume at its birth name on VM 700, which the
// listing returns first. Before the fix the name match on 700 won.
func TestBirthNameGuestOrderResolvesTheParkedDisk(t *testing.T) {
	t.Parallel()
	c := &scanFakeClient{configs: map[int]map[string]any{
		700:   {"scsi1": birthNameVolid + ",size=5G"},
		90000: {"scsi2": birthNameParked + ",serial=" + birthNameToken + ",size=5G", cfgKeyTags: "bosh-cpi;bosh-parker"},
	}, rows: []map[string]any{clusterRow(700, ""), clusterRow(90000, "bosh-cpi;bosh-parker")}}
	ident, err := ResolveDiskIdentity(context.Background(), c, nil, birthNameVolid, birthNameToken, birthNameParkerCfg)
	if err != nil {
		t.Fatalf("ResolveDiskIdentity: %v", err)
	}
	if !ident.Holder.Found || ident.Holder.VMID != 90000 || !ident.Holder.IsParker || ident.Volid != birthNameParked {
		t.Fatalf("identity = %+v, want the parker that carries the serial", ident)
	}
}

// TestBirthNameMapOrderResolvesBySerial puts the disk, renamed, and a
// serial-less volume at its birth name on two slots of one VM. Go walks the
// disk map in a random order, so before the fix the answer flipped between
// calls, and the loop makes a flip all but certain to show.
func TestBirthNameMapOrderResolvesBySerial(t *testing.T) {
	t.Parallel()
	c := &scanFakeClient{configs: map[int]map[string]any{
		700: {
			"scsi1": birthNameVolid + ",size=5G",
			"scsi2": "data:vm-700-disk-3,serial=" + birthNameToken + ",size=5G",
		},
	}, rows: []map[string]any{clusterRow(700, "")}}
	for i := range 64 {
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, birthNameVolid, birthNameToken, birthNameParkerCfg)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if !ident.Holder.Found || ident.Volid != "data:vm-700-disk-3" || ident.Holder.VMID != 700 {
			t.Fatalf("call %d resolved %+v, want 700's scsi2 by its serial", i, ident)
		}
	}
}

// TestBirthNameForeignSerialRefuses puts another disk's serial on the slot at
// the birth name, with the disk nowhere. Before the fix the resolver returned
// that slot as the disk.
func TestBirthNameForeignSerialRefuses(t *testing.T) {
	t.Parallel()
	c := &scanFakeClient{configs: map[int]map[string]any{
		700: {"scsi1": birthNameVolid + ",serial=" + birthNameOther + ",size=5G"},
	}, rows: []map[string]any{clusterRow(700, "")}}
	_, err := ResolveDiskIdentity(context.Background(), c, nil, birthNameVolid, birthNameToken, birthNameParkerCfg)
	requireBirthNameRefusal(t, err, "slot scsi1 of VM 700 on node pve1 with serial "+birthNameOther)
}

// TestBirthNameSerialLessSlotRefuses puts a serial-less volume at the birth
// name, with the disk nowhere. Nothing tells that volume apart from another
// disk's, so the resolver refuses the disk.
func TestBirthNameSerialLessSlotRefuses(t *testing.T) {
	t.Parallel()
	c := &scanFakeClient{configs: map[int]map[string]any{
		700: {"scsi1": birthNameVolid + ",size=5G"},
	}, rows: []map[string]any{clusterRow(700, "")}}
	_, err := ResolveDiskIdentity(context.Background(), c, nil, birthNameVolid, birthNameToken, birthNameParkerCfg)
	requireBirthNameRefusal(t, err, "slot scsi1 of VM 700 on node pve1 with no serial")
}

// TestBirthNameIntentWinsOverANameHolder keeps a parker's transfer record
// ahead of a slot that only names the birth volume. Before the fix the name
// match on 700 won before the record was ever read.
func TestBirthNameIntentWinsOverANameHolder(t *testing.T) {
	t.Parallel()
	desc := `<!--BOSH:{"bosh_parked_disks":{"` + birthNameToken + `":{"disk_cid":"pvd-x","source_vm_cid":"701",` +
		`"parked_at":"2026-08-20T00:00:00Z","node":"pve1","volid":"data:vm-701-disk-1","slot":"scsi4"}}}-->`
	c := &scanFakeClient{configs: map[int]map[string]any{
		700:   {"scsi1": birthNameVolid + ",size=5G"},
		90000: {cfgKeyTags: "bosh-cpi;bosh-parker", "description": desc},
	}, rows: []map[string]any{clusterRow(700, ""), clusterRow(90000, "bosh-cpi;bosh-parker")}}
	ident, err := ResolveDiskIdentity(context.Background(), c, nil, birthNameVolid, birthNameToken, birthNameParkerCfg)
	if err != nil {
		t.Fatalf("ResolveDiskIdentity: %v", err)
	}
	if ident.Intent == nil || ident.Intent.ParkerVMID != 90000 || ident.Volid != "data:vm-701-disk-1" || ident.Holder.Found {
		t.Fatalf("identity = %+v, want the parker's transfer record", ident)
	}
}

// TestBirthNameUnusedEntryNeedsTheDisksNote resolves an unusedN entry at the
// birth name as the disk only when its guest holds the disk's attached-disk
// note under the stable ID. Without the note the resolver refuses, and before
// the fix it returned the entry as the disk's.
func TestBirthNameUnusedEntryNeedsTheDisksNote(t *testing.T) {
	t.Parallel()
	noted := `<!--BOSH:{"bosh_attached_disks":{"` + birthNameToken + `":"pvd-a"}}-->`
	otherNote := `<!--BOSH:{"bosh_attached_disks":{"` + birthNameOther + `":"pvd-b","` + birthNameVolid + `":"pvd-c"}}-->`
	for _, tc := range []struct {
		name, description string
		resolves          bool
	}{
		{"with the disk's note", noted, true},
		{"with no note", "", false},
		{"with only other keys", otherNote, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := map[string]any{"unused0": birthNameVolid}
			if tc.description != "" {
				cfg["description"] = tc.description
			}
			c := &scanFakeClient{configs: map[int]map[string]any{777: cfg}, rows: []map[string]any{clusterRow(777, "")}}
			ident, err := ResolveDiskIdentity(context.Background(), c, nil, birthNameVolid, birthNameToken, birthNameParkerCfg)
			if !tc.resolves {
				requireBirthNameRefusal(t, err, "unused entry unused0 of VM 777 on node pve1, whose description holds no note for the disk")
				return
			}
			if err != nil {
				t.Fatalf("ResolveDiskIdentity: %v", err)
			}
			if len(ident.Unused) != 1 || ident.Unused[0].VMID != 777 || ident.Unused[0].Slot != "unused0" || ident.Volid != birthNameVolid {
				t.Fatalf("identity = %+v, want 777's unused0", ident)
			}
		})
	}
}

// TestBirthNameFreeFloatingDiskStillResolves is the control. A disk nothing
// names resolves to its birth volume, before the fix and after it.
func TestBirthNameFreeFloatingDiskStillResolves(t *testing.T) {
	t.Parallel()
	c := &scanFakeClient{configs: map[int]map[string]any{
		700: {"scsi1": "data:vm-700-disk-0,size=5G"},
	}, rows: []map[string]any{clusterRow(700, "")}}
	ident, err := ResolveDiskIdentity(context.Background(), c, nil, birthNameVolid, birthNameToken, birthNameParkerCfg)
	if err != nil {
		t.Fatalf("ResolveDiskIdentity: %v", err)
	}
	if ident.Volid != birthNameVolid || ident.Holder.Found || ident.Intent != nil || len(ident.Unused) != 0 {
		t.Fatalf("identity = %+v, want the birth volume with no holder", ident)
	}
}

// birthNameTransferRecord is a parker description holding one transfer
// record, keyed by token, that names slot scsi4 and the given volid.
func birthNameTransferRecord(token, volid string) string {
	return `<!--BOSH:{"bosh_parked_disks":{"` + token + `":{"disk_cid":"pvd-x","source_vm_cid":"700",` +
		`"parked_at":"2026-10-02T00:00:00Z","node":"pve1","volid":"` + volid + `","slot":"scsi4"}}}-->`
}

// birthNameLandedClient is the crash window. The disk was born on parker
// 90000 as vm-90000-disk-0, and the transfer back onto that parker landed it
// on scsi4 under the same name with no serial. Source VM 700 no longer names
// the volume.
func birthNameLandedClient(description string) *scanFakeClient {
	return newScanFakeClient(map[int]map[string]any{
		700: {},
		90000: {
			cfgKeyTags:    "bosh-cpi;bosh-parker",
			"description": description,
			"scsi4":       birthNameParked + ",size=5G",
		},
	})
}

// requireTransferRecordAnswer fails unless resolution came back with the
// parker's transfer record for scsi4 and no holder, which is the answer that
// sends a mutating handler to the resume.
func requireTransferRecordAnswer(t *testing.T, ident DiskIdentity, err error, wantVolid string) {
	t.Helper()
	if err != nil {
		t.Fatalf("ResolveDiskIdentity: %v, want the transfer record and never a refusal", err)
	}
	if ident.Intent == nil || ident.Intent.ParkerVMID != 90000 || ident.Intent.Slot != "scsi4" ||
		ident.Volid != wantVolid || ident.Holder.Found {
		t.Fatalf("identity = %+v, want the transfer record on parker 90000 slot scsi4", ident)
	}
}

// TestBirthNameLandedTransferResolvesTheDisk keeps a transfer record whose
// volid is the volume the recorded slot now holds. Resolution returns the
// disk through that record, and the slot that names the birth volume with no
// serial never turns into a refusal.
func TestBirthNameLandedTransferResolvesTheDisk(t *testing.T) {
	t.Parallel()
	c := birthNameLandedClient(birthNameTransferRecord(birthNameToken, birthNameParked))
	ident, err := ResolveDiskIdentity(context.Background(), c, nil, birthNameParked, birthNameToken, birthNameParkerCfg)
	requireTransferRecordAnswer(t, ident, err, birthNameParked)
}

// TestBirthNameUnprovenTransferDefersToTheResume keeps a transfer record that
// still carries the volume's name from before the move, so nothing recorded
// proves what landed on the slot. Resolution still returns the record rather
// than the refusal, because the resume runs only from the record resolution
// hands back, and its own proof decides what the slot holds.
func TestBirthNameUnprovenTransferDefersToTheResume(t *testing.T) {
	t.Parallel()
	c := birthNameLandedClient(birthNameTransferRecord(birthNameToken, "data:vm-700-disk-1"))
	ident, err := ResolveDiskIdentity(context.Background(), c, nil, birthNameParked, birthNameToken, birthNameParkerCfg)
	requireTransferRecordAnswer(t, ident, err, "data:vm-700-disk-1")
}

// TestBirthNameOtherDisksTransferRecordProvesNothing keeps the same slot and
// the same landed volume, but the record on the parker belongs to another
// disk. That record says nothing about this disk, so the slot is only a name
// holder, and resolution refuses the disk the way it does with no record.
func TestBirthNameOtherDisksTransferRecordProvesNothing(t *testing.T) {
	t.Parallel()
	c := birthNameLandedClient(birthNameTransferRecord(birthNameOther, "data:vm-700-disk-1"))
	_, err := ResolveDiskIdentity(context.Background(), c, nil, birthNameParked, birthNameToken, birthNameParkerCfg)
	if err == nil {
		t.Fatal("the resolver resolved the disk through another disk's transfer record, want the refusal")
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || typed.Type() != cpierrors.TypeCloud || typed.OkToRetry() {
		t.Fatalf("error = %v, want a permanent CloudError", err)
	}
	for _, text := range []string{"its birth volume " + birthNameParked, "slot scsi4 of VM 90000 on node pve1 with no serial"} {
		if !strings.Contains(err.Error(), text) {
			t.Fatalf("error = %v\nwant it to contain %q", err, text)
		}
	}
}

// TestBirthNameResumeSettlesTheTransferWindow runs the resume from the record
// resolution returns. The resume claims the landed slot and writes the serial,
// and the next resolution finds the disk by that serial.
func TestBirthNameResumeSettlesTheTransferWindow(t *testing.T) {
	t.Parallel()
	c := birthNameLandedClient(birthNameTransferRecord(birthNameToken, "data:vm-700-disk-1"))
	ctx := context.Background()
	ident, err := ResolveDiskIdentity(ctx, c, nil, birthNameParked, birthNameToken, birthNameParkerCfg)
	requireTransferRecordAnswer(t, ident, err, "data:vm-700-disk-1")

	landed, err := ResumeDiskTransferToParker(ctx, c, nil, *ident.Intent, birthNameToken, birthNameParkerCfg, ParkContext{})
	if err != nil {
		t.Fatalf("ResumeDiskTransferToParker: %v", err)
	}
	if landed != birthNameParked {
		t.Fatalf("landed = %q, want the volume on the recorded slot", landed)
	}
	if got, _ := c.configs[90000]["scsi4"].(string); !strings.Contains(got, "serial="+birthNameToken) {
		t.Fatalf("parker slot after the resume = %q, want the disk's serial", got)
	}

	ident, err = ResolveDiskIdentity(ctx, c, nil, birthNameParked, birthNameToken, birthNameParkerCfg)
	if err != nil {
		t.Fatalf("ResolveDiskIdentity after the resume: %v", err)
	}
	if !ident.Holder.Found || ident.Holder.VMID != 90000 || ident.Holder.Slot != "scsi4" || !ident.Holder.IsParker ||
		ident.Intent != nil || ident.Volid != birthNameParked {
		t.Fatalf("identity after the resume = %+v, want parker 90000 slot scsi4 by its serial", ident)
	}
}
