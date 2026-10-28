package pve

import "testing"

func TestClassifyStorageFormat(t *testing.T) {
	t.Parallel()
	cases := map[string]StorageFormatClass{
		"dir":         StorageFormatFile,
		"nfs":         StorageFormatFile,
		"cifs":        StorageFormatFile,
		"glusterfs":   StorageFormatFile,
		"cephfs":      StorageFormatFile,
		"btrfs":       StorageFormatFile,
		"lvm":         StorageFormatBlock,
		"lvmthin":     StorageFormatBlock,
		"zfspool":     StorageFormatBlock,
		"zfs":         StorageFormatBlock,
		"rbd":         StorageFormatBlock,
		"iscsi":       StorageFormatBlock,
		"iscsidirect": StorageFormatBlock,
		"LVMThin":     StorageFormatBlock,
		"pbs":         StorageFormatUnknown,
		"":            StorageFormatUnknown,
		"esxi":        StorageFormatUnknown,
	}
	for storageType, want := range cases {
		if got := ClassifyStorageFormat(storageType); got != want {
			t.Errorf("ClassifyStorageFormat(%q) = %s; want %s", storageType, got, want)
		}
	}
	// Surrounding whitespace and case do not change the class.
	if got := ClassifyStorageFormat(" NFS "); got != StorageFormatFile {
		t.Errorf("ClassifyStorageFormat(\" NFS \") = %s; want %s", got, StorageFormatFile)
	}
}

func TestResolveDiskFormat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, storageType, requested string
		want                         DiskFormatResolution
	}{
		// Unset (empty, whitespace, or "auto" in any case) picks from the type.
		{"unset on lvmthin is raw", "lvmthin", "", DiskFormatResolution{Format: "raw", Class: StorageFormatBlock, Auto: true}},
		{"auto on rbd is raw", "rbd", "auto", DiskFormatResolution{Format: "raw", Class: StorageFormatBlock, Auto: true}},
		{"AUTO on zfspool is raw", "zfspool", " AUTO ", DiskFormatResolution{Format: "raw", Class: StorageFormatBlock, Auto: true}},
		{"unset on thick lvm is raw", "lvm", "", DiskFormatResolution{Format: "raw", Class: StorageFormatBlock, Auto: true}},
		{"unset on nfs is qcow2", "nfs", "", DiskFormatResolution{Format: "qcow2", Class: StorageFormatFile, Auto: true}},
		{"whitespace on dir is qcow2", "dir", "  ", DiskFormatResolution{Format: "qcow2", Class: StorageFormatFile, Auto: true}},
		{"unset on unknown falls back to qcow2", "pbs", "", DiskFormatResolution{Format: "qcow2", Class: StorageFormatUnknown, Auto: true, Fallback: true}},
		{"unset on failed lookup falls back to qcow2", "", "auto", DiskFormatResolution{Format: "qcow2", Class: StorageFormatUnknown, Auto: true, Fallback: true}},
		// Explicit formats pass through on file storage and unknown types.
		{"explicit raw on nfs", "nfs", "raw", DiskFormatResolution{Format: "raw", Requested: "raw", Class: StorageFormatFile}},
		{"explicit vmdk on dir", "dir", "vmdk", DiskFormatResolution{Format: "vmdk", Requested: "vmdk", Class: StorageFormatFile}},
		{"explicit qcow2 on unknown", "", "qcow2", DiskFormatResolution{Format: "qcow2", Requested: "qcow2", Class: StorageFormatUnknown}},
		{"explicit raw on unknown", "pbs", " raw ", DiskFormatResolution{Format: "raw", Requested: "raw", Class: StorageFormatUnknown}},
		// Block storage holds raw. Thick lvm keeps an explicit qcow2.
		{"explicit raw on lvmthin", "lvmthin", "raw", DiskFormatResolution{Format: "raw", Requested: "raw", Class: StorageFormatBlock}},
		{"explicit qcow2 on lvmthin coerces", "lvmthin", "qcow2", DiskFormatResolution{Format: "raw", Requested: "qcow2", Class: StorageFormatBlock, Coerced: true}},
		{"explicit qcow2 on rbd coerces", "rbd", "qcow2", DiskFormatResolution{Format: "raw", Requested: "qcow2", Class: StorageFormatBlock, Coerced: true}},
		{"explicit vmdk on zfspool coerces", "zfspool", "vmdk", DiskFormatResolution{Format: "raw", Requested: "vmdk", Class: StorageFormatBlock, Coerced: true}},
		{"explicit qcow2 on thick lvm passes", "lvm", "qcow2", DiskFormatResolution{Format: "qcow2", Requested: "qcow2", Class: StorageFormatBlock}},
		{"explicit vmdk on thick lvm coerces", "lvm", "vmdk", DiskFormatResolution{Format: "raw", Requested: "vmdk", Class: StorageFormatBlock, Coerced: true}},
		// An unsupported string is left for PVE to reject, as before.
		{"unsupported string passes through", "lvmthin", "qed", DiskFormatResolution{Format: "qed", Requested: "qed", Class: StorageFormatBlock}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ResolveDiskFormat(tc.storageType, tc.requested); got != tc.want {
				t.Errorf("ResolveDiskFormat(%q, %q) = %+v; want %+v", tc.storageType, tc.requested, got, tc.want)
			}
		})
	}
}

func TestEphemeralVolumeFormat_UnsetPicksFromStorageType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		storageType, preferred, want string
		wantErr                      bool
	}{
		{"nfs", "", "qcow2", false},
		{"dir", "auto", "qcow2", false},
		{"lvmthin", "", "raw", false},
		{"lvm", "Auto", "raw", false},
		{"zfs", "", "raw", false},
		{"nfs", "raw", "raw", false},
		{"lvmthin", "qcow2", "raw", false},
		{"lvm", "qcow2", "raw", false},
		{"", "", "", true},
		{"cephfs", "", "", true},
		{"nfs", "qed", "", true},
	}
	for _, tc := range cases {
		got, err := EphemeralVolumeFormat(tc.storageType, tc.preferred)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("EphemeralVolumeFormat(%q, %q) = (%q, %v); want (%q, error=%v)", tc.storageType, tc.preferred, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestEphemeralVolumeName_UnsetFormatOnFileStorageCarriesQCOW2Extension(t *testing.T) {
	t.Parallel()
	name, suffix, err := EphemeralVolumeName("nfs", "", 321)
	if err != nil {
		t.Fatal(err)
	}
	if name != "vm-321-ephemeral-0.qcow2" || suffix != "321/vm-321-ephemeral-0.qcow2" {
		t.Errorf("EphemeralVolumeName(nfs, \"\") = (%q, %q)", name, suffix)
	}
	name, suffix, err = EphemeralVolumeName("lvmthin", "auto", 321)
	if err != nil {
		t.Fatal(err)
	}
	if name != "vm-321-ephemeral-0" || suffix != name {
		t.Errorf("EphemeralVolumeName(lvmthin, auto) = (%q, %q)", name, suffix)
	}
}
