// disk_stable_id_handlers_test.go — handler-level behavior for stable-ID
// disks (D13): the identity resolution rides in front of every disk handler,
// update_disk's read-modify-write preserves the identity serial, and
// get_disks reads the stable-ID-keyed sentinel.
package handlers_test

import (
	"context"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

const stableIDTestToken = "bpd-1122334455667788"

func TestHandleUpdateDisk_PreservesStableIDSerial(t *testing.T) {
	t.Parallel()

	// The disk was created as vm-9001 and later renamed by a reassignment;
	// the VM config carries the CURRENT name plus the identity serial.
	const birthVolid = "local-lvm:vm-9001-disk-0"
	const currentVal = "local-lvm:vm-100-disk-2,serial=" + stableIDTestToken + ",size=10G"

	var capturedOptStr string
	qemuSvc := &updateDiskQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"scsi2": currentVal}, nil
		},
		attachDiskFn: func(_ context.Context, _ string, _ int, optStr string, _ string, opts *qemu.AttachOpts) (string, error) {
			capturedOptStr = optStr
			if opts == nil || opts.DiskID != "scsi2" {
				t.Errorf("expected AttachDisk with DiskID=scsi2, got %v", opts)
			}
			return "scsi2", nil
		},
	}

	cid := mustEncodeDiskCID(t, birthVolid, &pve.DiskCIDMeta{ID: stableIDTestToken})
	h := handlers.HandleUpdateDisk(updateDiskDeps(qemuSvc, updateClusterWith(100), nil))
	if _, err := h.Handle(context.Background(), marshalArgs(cid, map[string]any{
		"cache": "writeback",
	}), jsonrpc.Context{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The identity resolution found the renamed volume via its serial, and
	// the option merge (which reads the live config) kept the serial intact.
	if !strings.HasPrefix(capturedOptStr, "local-lvm:vm-100-disk-2,") {
		t.Errorf("update must address the CURRENT volid, got %q", capturedOptStr)
	}
	if !strings.Contains(capturedOptStr, "serial="+stableIDTestToken) {
		t.Errorf("update dropped the identity serial: %q", capturedOptStr)
	}
	if !strings.Contains(capturedOptStr, "cache=writeback") || !strings.Contains(capturedOptStr, "size=10G") {
		t.Errorf("merged option string incomplete: %q", capturedOptStr)
	}
}

func TestHandleGetDisks_StableIDKeyedSentinel(t *testing.T) {
	t.Parallel()

	const recorded = "pvd-recorded-by-attach"
	desc := `<!--BOSH:{"bosh_attached_disks":{"` + stableIDTestToken + `":"` + recorded + `"}}-->`
	qemuSvc := &getDisksQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
				"virtio0":     "local-lvm:vm-100-disk-0,size=20G",
				"scsi1":       "local-lvm:vm-100-disk-2,serial=" + stableIDTestToken + ",size=10G",
				"description": desc,
			}, nil
		},
	}

	h := handlers.HandleGetDisks(getDisksDeps(qemuSvc))
	result, err := h.Handle(context.Background(), marshalArgs("100"), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cids, ok := result.([]string)
	if !ok || len(cids) != 1 {
		t.Fatalf("result = %#v, want exactly one disk CID", result)
	}
	if cids[0] != recorded {
		t.Errorf("get_disks = %q, want the stable-ID-keyed recorded CID %q", cids[0], recorded)
	}
}

func TestHandleHasDisk_ResolvedByIdentityScan(t *testing.T) {
	t.Parallel()

	// The volume was renamed by a reassignment: the birth volid exists
	// nowhere on storage, but a VM's drive entry carries the serial. has_disk
	// must answer true from the scan alone (no storage probe).
	const birthVolid = "local-lvm:vm-9001-disk-0"
	qemuSvc := &getDisksQEMUService{
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{
				"scsi1": "local-lvm:vm-100-disk-2,serial=" + stableIDTestToken,
			}, nil
		},
	}
	// No storage service wired: a storage probe would panic, proving the
	// answer came from the identity scan.
	deps := testDepsFoundVM(100, qemuSvc, nil, nil, &mockAgentService{})

	cid := mustEncodeDiskCID(t, birthVolid, &pve.DiskCIDMeta{ID: stableIDTestToken})
	h := handlers.HandleHasDisk(deps)
	result, err := h.Handle(context.Background(), marshalArgs(cid), jsonrpc.Context{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists, ok := result.(bool); !ok || !exists {
		t.Errorf("has_disk = %#v, want true from the identity scan", result)
	}
}
