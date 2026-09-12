package handlers

import "testing"

func TestManagedConfigDriveReadbackGeneratedSize(t *testing.T) {
	const volume = "nfs-ephemeral-3:8190/vm-8190-disk-0.qcow2"
	const requested = volume + ",discard=on,iothread=1"
	for _, tc := range []struct {
		name, key, want, got string
		reject               bool
	}{
		{name: "PVE generated size", key: "virtio0", want: requested, got: requested + ",size=5G"},
		{name: "property order", key: "scsi0", want: requested, got: volume + ",size=5G,iothread=1,discard=on"},
		{name: "changed identity", key: "virtio0", want: requested, got: "other:8190/disk.qcow2,discard=on,iothread=1,size=5G", reject: true},
		{name: "missing requested option", key: "virtio0", want: requested, got: volume + ",discard=on,size=5G", reject: true},
		{name: "changed requested option", key: "virtio0", want: requested, got: volume + ",discard=ignore,iothread=1,size=5G", reject: true},
		{name: "unrequested option", key: "virtio0", want: requested, got: requested + ",cache=unsafe,size=5G", reject: true},
		{name: "requested size differs", key: "virtio0", want: requested + ",size=8G", got: requested + ",size=5G", reject: true},
		{name: "duplicate size", key: "virtio0", want: requested, got: requested + ",size=5G,size=8G", reject: true},
		{name: "malformed size", key: "virtio0", want: requested, got: requested + ",size=unknown", reject: true},
		{name: "non disk property", key: "net0", want: requested, got: requested + ",size=5G", reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := managedConfigFieldsMatch(map[string]any{tc.key: tc.got}, map[string]any{tc.key: tc.want})
			if (err != nil) != tc.reject {
				t.Fatalf("unexpected comparison: %v", err)
			}
		})
	}
}
