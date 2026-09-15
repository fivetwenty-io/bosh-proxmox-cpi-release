// Tests for the name match that keeps the volume under proof out of the
// journal's count. The three shapes a volume name arrives in have to resolve to
// one volume, or a completed delete reads as a contradiction and the disk is
// never forgotten.
package handlers

import "testing"

func TestSameStorageVolume(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		storage string
		left    string
		right   string
		want    bool
	}{
		{
			name:    "identical qualified volids",
			storage: "nfs-images",
			left:    "nfs-images:604/vm-604-disk-0.qcow2",
			right:   "nfs-images:604/vm-604-disk-0.qcow2",
			want:    true,
		},
		{
			name:    "qualified against the bare name the proof was handed",
			storage: "nfs-images",
			left:    "nfs-images:604/vm-604-disk-0.qcow2",
			right:   "604/vm-604-disk-0.qcow2",
			want:    true,
		},
		{
			name:    "bare against qualified, the other way round",
			storage: "nfs-images",
			left:    "604/vm-604-disk-0.qcow2",
			right:   "nfs-images:604/vm-604-disk-0.qcow2",
			want:    true,
		},
		{
			name:    "file name against the VMID directory form",
			storage: "nfs-images",
			left:    "nfs-images:604/vm-604-disk-0.qcow2",
			right:   "vm-604-disk-0.qcow2",
			want:    true,
		},
		{
			name:    "block storage names carry no directory at all",
			storage: "local-lvm",
			left:    "local-lvm:vm-604-disk-0",
			right:   "vm-604-disk-0",
			want:    true,
		},
		{
			name:    "a different volume on the same storage",
			storage: "nfs-images",
			left:    "nfs-images:700/vm-700-disk-0.qcow2",
			right:   "nfs-images:604/vm-604-disk-0.qcow2",
			want:    false,
		},
		{
			name:    "a second disk of the same VM",
			storage: "nfs-images",
			left:    "nfs-images:604/vm-604-disk-1.qcow2",
			right:   "nfs-images:604/vm-604-disk-0.qcow2",
			want:    false,
		},
		{
			name:    "the same name qualified with another storage stays another storage's",
			storage: "nfs-images",
			left:    "other-nfs:604/vm-604-disk-0.qcow2",
			right:   "nfs-images:604/vm-604-disk-0.qcow2",
			want:    false,
		},
		{
			name:    "whitespace around a recorded name does not hide the match",
			storage: "nfs-images",
			left:    "  nfs-images:604/vm-604-disk-0.qcow2  ",
			right:   "604/vm-604-disk-0.qcow2",
			want:    true,
		},
		{
			name:    "an empty name matches nothing, including another empty one",
			storage: "nfs-images",
			left:    "",
			right:   "",
			want:    false,
		},
		{
			name:    "an empty volume under proof excludes nothing",
			storage: "nfs-images",
			left:    "nfs-images:604/vm-604-disk-0.qcow2",
			right:   "",
			want:    false,
		},
		{
			name:    "an unnamed storage compares the names as they stand",
			storage: "",
			left:    "nfs-images:604/vm-604-disk-0.qcow2",
			right:   "nfs-images:604/vm-604-disk-0.qcow2",
			want:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := sameStorageVolume(tc.storage, tc.left, tc.right); got != tc.want {
				t.Errorf("sameStorageVolume(%q, %q, %q) = %t, want %t",
					tc.storage, tc.left, tc.right, got, tc.want)
			}
		})
	}
}
