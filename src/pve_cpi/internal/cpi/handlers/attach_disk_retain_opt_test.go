package handlers_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// TestHandleAttachDisk_RetainOnDeleteOptNeverReachesDriveString verifies that
// the CPI-internal retain_on_delete CID opt is stripped before the drive
// option string is built. PVE validates drive options against a closed
// schema and rejects the whole config write on any unknown key, so a
// retain-created disk would otherwise fail its first attach.
func TestHandleAttachDisk_RetainOnDeleteOptNeverReachesDriveString(t *testing.T) {
	t.Parallel()
	const bareCID = "local-lvm:vm-9001-disk-0"

	cid := mustEncodeDiskCID(t, bareCID, &pve.DiskCIDMeta{
		Opts: map[string]string{
			"retain_on_delete": "1",
			"cache":            "none",
			"iothread":         "1",
			"ssd":              "1",
		},
	})

	qemuSvc := &attachQEMUService{
		attachReturnDiskID: "scsi1",
		configCfg:          map[string]any{"scsi1": bareCID},
	}
	cfg := &config.CPIConfig{Node: testNode, VMDiskFormat: "qcow2"}
	deps := attachDepsPerfWithStorageType(qemuSvc, cfg, "zfspool")
	h := handlers.HandleAttachDisk(deps)

	_, err := h.Handle(context.Background(), attachArgs(t, "100", cid), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(qemuSvc.attachLastVolid, "retain_on_delete") {
		t.Errorf("AttachDisk volid carries CPI-internal key: %q", qemuSvc.attachLastVolid)
	}
	if !strings.Contains(qemuSvc.attachLastVolid, "cache=none") {
		t.Errorf("AttachDisk volid lost a real drive option: %q", qemuSvc.attachLastVolid)
	}
}
