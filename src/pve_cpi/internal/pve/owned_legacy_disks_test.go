package pve_test

import (
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func ownedLegacyConfig(t *testing.T, recorded map[string]string, drives map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(recorded)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := pve.RenderSentinel("", map[string]json.RawMessage{"bosh_attached_disks": raw})
	if err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{"description": desc}
	for k, v := range drives {
		cfg[k] = v
	}
	return cfg
}

func mustCID(t *testing.T, volid string, meta *pve.DiskCIDMeta) string {
	t.Helper()
	cid, err := pve.EncodeDiskCID(volid, meta)
	if err != nil {
		t.Fatal(err)
	}
	return cid
}

// TestFindOwnedLegacyPersistentDisks pins which entries delete_vm treats as a
// persistent disk named for the VM. The entry is a recorded attach on an
// active slot, named for this VMID, with no bpd- serial on the drive, and it
// is not the VM's own ephemeral volume that legacy retention recorded before
// a cut-short transfer.
func TestFindOwnedLegacyPersistentDisks(t *testing.T) {
	t.Parallel()
	const vmid = 777
	stable := &pve.DiskCIDMeta{ID: "bpd-0123456789abcdef"}
	cases := []struct {
		name     string
		recorded map[string]string
		drives   map[string]any
		want     map[string]string
	}{
		{
			name:     "recorded legacy disk named for the VM",
			recorded: map[string]string{"data:vm-777-disk-0": mustCID(t, "data:vm-777-disk-0", nil)},
			drives:   map[string]any{"scsi0": "local-lvm:vm-777-disk-0", "scsi1": "data:vm-777-disk-0,size=2G"},
			want:     map[string]string{"scsi1": "data:vm-777-disk-0"},
		},
		{
			name:     "recorded legacy disk in the file-backed form",
			recorded: map[string]string{"nfs:777/vm-777-disk-0.qcow2": mustCID(t, "nfs:777/vm-777-disk-0.qcow2", nil)},
			drives:   map[string]any{"scsi1": "nfs:777/vm-777-disk-0.qcow2"},
			want:     map[string]string{"scsi1": "nfs:777/vm-777-disk-0.qcow2"},
		},
		{
			name:     "same name with no record is the VM's own system disk",
			recorded: map[string]string{"other:vm-9001-disk-0": mustCID(t, "other:vm-9001-disk-0", nil)},
			drives:   map[string]any{"scsi0": "local-lvm:vm-777-disk-0"},
			want:     map[string]string{},
		},
		{
			name:     "a disk named for another VMID is foreign, not owned",
			recorded: map[string]string{"data:vm-9001-disk-0": mustCID(t, "data:vm-9001-disk-0", nil)},
			drives:   map[string]any{"scsi1": "data:vm-9001-disk-0"},
			want:     map[string]string{},
		},
		{
			name:     "no serial but a stable-ID CID is still freed by the destroy, so it counts",
			recorded: map[string]string{"data:vm-777-disk-0": mustCID(t, "data:vm-777-disk-0", stable)},
			drives:   map[string]any{"scsi1": "data:vm-777-disk-0,size=2G"},
			want:     map[string]string{"scsi1": "data:vm-777-disk-0"},
		},
		{
			name:     "a stable-ID drive is left to the parker transfer",
			recorded: map[string]string{"data:vm-777-disk-0": mustCID(t, "data:vm-777-disk-0", stable)},
			drives:   map[string]any{"scsi1": "data:vm-777-disk-0,serial=bpd-0123456789abcdef"},
			want:     map[string]string{},
		},
		{
			name:     "ephemeral recorded by a cut-short retention, block form",
			recorded: map[string]string{"a:vm-777-ephemeral-0": mustCID(t, "a:vm-777-ephemeral-0", stable)},
			drives:   map[string]any{"scsi1": "a:vm-777-ephemeral-0,size=5G"},
			want:     map[string]string{},
		},
		{
			name:     "ephemeral recorded by a cut-short retention, file-backed form",
			recorded: map[string]string{"a:777/vm-777-ephemeral-0.raw": mustCID(t, "a:777/vm-777-ephemeral-0.raw", stable)},
			drives:   map[string]any{"scsi1": "a:777/vm-777-ephemeral-0.raw,size=5G"},
			want:     map[string]string{},
		},
		{
			// IsOwnEphemeralVolume needs a storage prefix, so it does not
			// match this value. VolumeNamedForVM does not match it either,
			// so the entry is still left out.
			name:     "ephemeral name with no storage prefix",
			recorded: map[string]string{"vm-777-ephemeral-0": mustCID(t, "a:vm-777-ephemeral-0", stable)},
			drives:   map[string]any{"scsi1": "vm-777-ephemeral-0,size=5G"},
			want:     map[string]string{},
		},
		{
			name:     "a record that no longer decodes still counts",
			recorded: map[string]string{"data:vm-777-disk-0": "not-an-envelope"},
			drives:   map[string]any{"scsi1": "data:vm-777-disk-0"},
			want:     map[string]string{"scsi1": "data:vm-777-disk-0"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := pve.FindOwnedLegacyPersistentDisks(ownedLegacyConfig(t, tc.recorded, tc.drives), vmid)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for slot, volid := range tc.want {
				if got[slot] != volid {
					t.Errorf("slot %s = %q, want %q (all: %v)", slot, got[slot], volid, got)
				}
			}
		})
	}
}
