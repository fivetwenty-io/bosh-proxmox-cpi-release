package pve

import (
	"regexp"
	"testing"
)

func TestEphemeralVolumeNameMatchesFilePluginFormatAndOwner(t *testing.T) {
	pattern := regexp.MustCompile(`^vm-123-ephemeral-0\.(raw|qcow2|vmdk)$`)
	for _, backend := range []string{"dir", "nfs", "cifs", "glusterfs", "btrfs"} {
		for _, format := range []string{"raw", "qcow2", "vmdk"} {
			name, suffix, err := EphemeralVolumeName(backend, format, 123)
			if err != nil || !pattern.MatchString(name) || name != "vm-123-ephemeral-0."+format || suffix != "123/"+name {
				t.Fatalf("%s/%s: filename=%q suffix=%q error=%v", backend, format, name, suffix, err)
			}
		}
	}
}

func TestEphemeralVolumeNamePreservesBlockIdentityAndRejectsUnknown(t *testing.T) {
	for _, backend := range []string{"lvm", "lvmthin", "zfspool", "rbd"} {
		name, suffix, err := EphemeralVolumeName(backend, "raw", 123)
		if err != nil || name != "vm-123-ephemeral-0" || suffix != name {
			t.Fatalf("%s changed block identity: %q %q %v", backend, name, suffix, err)
		}
	}
	for _, input := range []struct {
		backend, format string
		vmid            int
	}{{"", "raw", 123}, {"unknown", "raw", 123}, {"nfs", "vhd", 123}, {"nfs", "../raw", 123}, {"nfs", "raw", 0}} {
		if _, _, err := EphemeralVolumeName(input.backend, input.format, input.vmid); err == nil {
			t.Fatalf("invalid ephemeral identity accepted: %+v", input)
		}
	}
}

func TestEphemeralVolumeFormatUsesRawForBlockCompanion(t *testing.T) {
	for _, input := range []struct{ backend, preferred, expected string }{{"nfs", "qcow2", "qcow2"}, {"lvmthin", "qcow2", "raw"}, {"rbd", "vmdk", "raw"}} {
		actual, err := EphemeralVolumeFormat(input.backend, input.preferred)
		if err != nil || actual != input.expected {
			t.Fatalf("%+v: format=%q error=%v", input, actual, err)
		}
	}
}
