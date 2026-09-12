package pve

import "testing"

func TestManagedEphemeralBirthIdentity(t *testing.T) {
	id := "12345678-1234-4234-8234-123456789abc"
	for _, plugin := range []string{"nfs", "dir", "zfspool", "lvmthin"} {
		t.Run(plugin, func(t *testing.T) {
			name, suffix, err := ManagedEphemeralVolumeName(plugin, "qcow2", 123, "namespace", id)
			if err != nil {
				t.Fatal(err)
			}
			if name == "" {
				t.Fatal("missing filename")
			}
			locator, got, ok := ParseManagedEphemeralVolumeID("store:" + suffix)
			if !ok || got != id || locator != AllocationNamespaceLocator("namespace") {
				t.Fatal("birth identity lost")
			}
			if _, _, ok := ParseAllocationVolumeID("store:" + suffix); ok {
				t.Fatal("VM ephemeral confused with persistent birth")
			}
		})
	}
	for _, volume := range []string{"s:123/vm-123-ephemeral-0.qcow2", "s:124/vm-123-bosh-0123456789abcdef-ephemeral-" + id + ".qcow2", "s:123/../123/vm-123-bosh-0123456789abcdef-ephemeral-" + id + ".qcow2", "s:vm-123-bosh-0123456789abcdef-ephemeral-" + id + ".qcow2", "s:123/vm-123-bosh-0123456789abcdef-ephemeral-" + id} {
		if _, _, ok := ParseManagedEphemeralVolumeID(volume); ok {
			t.Fatal("noncanonical identity accepted")
		}
	}
}
