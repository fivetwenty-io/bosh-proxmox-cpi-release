package handlers_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
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
		{"call qcow2 on thick lvm sends raw", "lvm", "", map[string]any{"disk_format": "qcow2"}, "raw", "", "raw"},
		{"call QCOW2 on nfs is lower-cased", "nfs", "", map[string]any{"disk_format": "QCOW2"}, "qcow2", ".qcow2", "qcow2"},
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

// On a storage type the CPI does not classify, create_disk sends no format
// unless cloud_properties set one, so the warning says that PVE applies the
// storage's own default rather than claiming a qcow2 fallback.
func TestHandleCreateDisk_UnknownTypeWarnsThatPVEDecides(t *testing.T) {
	t.Parallel()
	sent := &sentVolume{}
	deps := depsForCreateDiskFormatRecord(capturingFormatStorageSvc(sent), "esxi", "")
	logger, obs := log.NewObservedLogger(log.LevelWarn)
	deps.Logger = logger
	createDiskMetaWith(t, deps, nil)
	sent.mu.Lock()
	defer sent.mu.Unlock()
	if sent.format != "" {
		t.Errorf("format sent to PVE = %q; want none", sent.format)
	}
	var warnings []string
	for _, e := range obs.All() {
		if strings.Contains(e.Message, "storage type is unknown") {
			warnings = append(warnings, e.Message)
		}
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "sends no disk format and PVE applies the storage's default") {
		t.Errorf("unknown-type warnings = %v; want one saying PVE applies the storage's default", warnings)
	}
}
