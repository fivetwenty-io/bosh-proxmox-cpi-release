package handlers_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// sentVolume records the format and name create_disk passed to CreateVolume.
type sentVolume struct {
	mu           sync.Mutex
	format, name string
}

func capturingFormatStorageSvc(sent *sentVolume) *mockStorageService {
	return &mockStorageService{
		createVolumeFn: func(_ context.Context, _, storage string, _ int, format string, vmid int, name string) (string, error) {
			sent.mu.Lock()
			sent.format, sent.name = format, name
			sent.mu.Unlock()
			if strings.Contains(name, ".") {
				return fmt.Sprintf("%s:%d/%s", storage, vmid, name), nil
			}
			return fmt.Sprintf("%s:%s", storage, name), nil
		},
	}
}

// create_disk resolves the format from the target pool's type. These cases pin
// what reaches PVE (the format parameter and the volume-name extension) and
// what the CID records, including an empty or "auto" value at the call layer,
// a disk_type profile, a vm_type profile, and the job property.
func TestHandleCreateDisk_FormatFromStorageType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		storageType string
		job         string
		cloudProps  map[string]any
		wantSent    string
		wantExt     string
		wantRecord  string
	}{
		{"unset on lvmthin sends nothing and records raw", "lvmthin", "", nil, "", "", "raw"},
		{"call auto on lvmthin sends nothing", "lvmthin", "", map[string]any{"disk_format": "auto"}, "", "", "raw"},
		{"call qcow2 on lvmthin sends raw", "lvmthin", "", map[string]any{"disk_format": "qcow2"}, "raw", "", "raw"},
		{"job qcow2 on lvmthin sends nothing", "lvmthin", "qcow2", nil, "", "", "raw"},
		{"job qcow2 on thick lvm sends nothing", "lvm", "qcow2", nil, "", "", "raw"},
		{"call qcow2 on thick lvm passes through", "lvm", "", map[string]any{"disk_format": "qcow2"}, "qcow2", "", "raw"},
		{"unset on nfs sends qcow2", "nfs", "", nil, "qcow2", ".qcow2", "qcow2"},
		{"job auto on dir sends qcow2", "dir", "auto", nil, "qcow2", ".qcow2", "qcow2"},
		{"call empty falls to job raw on nfs", "nfs", "raw", map[string]any{"disk_format": ""}, "raw", ".raw", "raw"},
		{"disk_type auto falls to job raw on nfs", "nfs", "raw", map[string]any{"disk_type": "auto-dt"}, "raw", ".raw", "raw"},
		{"vm_type auto and job auto on nfs give qcow2", "nfs", "AUTO", map[string]any{"vm_type": "auto-vt"}, "qcow2", ".qcow2", "qcow2"},
		{"disk_type raw beats job qcow2 on cifs", "cifs", "qcow2", map[string]any{"disk_type": "raw-dt"}, "raw", ".raw", "raw"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sent := &sentVolume{}
			deps := depsForCreateDiskFormatRecord(capturingFormatStorageSvc(sent), tc.storageType, tc.job)
			deps.Config.DiskTypes = map[string]config.TypeProfile{
				"auto-dt": {CloudProperties: map[string]any{"disk_format": "auto"}},
				"raw-dt":  {CloudProperties: map[string]any{"disk_format": "raw"}},
			}
			deps.Config.VMTypes = map[string]config.TypeProfile{
				"auto-vt": {CloudProperties: map[string]any{"disk_format": ""}},
			}
			meta := createDiskMetaWith(t, deps, tc.cloudProps)
			sent.mu.Lock()
			defer sent.mu.Unlock()
			if sent.format != tc.wantSent {
				t.Errorf("format sent to PVE = %q; want %q", sent.format, tc.wantSent)
			}
			if tc.wantExt == "" && strings.Contains(sent.name, ".") || tc.wantExt != "" && !strings.HasSuffix(sent.name, tc.wantExt) {
				t.Errorf("volume name = %q; want extension %q", sent.name, tc.wantExt)
			}
			if meta.Format != tc.wantRecord {
				t.Errorf("recorded format = %q; want %q", meta.Format, tc.wantRecord)
			}
		})
	}
}
