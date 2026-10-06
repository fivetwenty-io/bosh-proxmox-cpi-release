package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestUpdateParkedDisk_NamesTheSizeWhenTheOverridesFail is update_disk on a
// parked disk with a size and an option, where recording the option fails
// after the size step. The disk already has the requested size by then, so
// the error says so and doesn't claim that nothing was changed.
func TestUpdateParkedDisk_NamesTheSizeWhenTheOverridesFail(t *testing.T) {
	t.Parallel()

	parkedStr := "data:vm-90000-disk-0,serial=" + idTestToken + ",size=10G"
	c := newIDFakeClient(map[int]map[string]any{
		90000: {"tags": "bosh-cpi;bosh-parker", "protection": true, "digest": "digest-0", diskKeyScsi0: parkedStr},
	})
	c.descWriteErr = errors.New("description write refused")
	deps := overlayTestDeps(c)
	diskCID := overlayCID(t, "data:vm-9001-disk-0", &pve.DiskCIDMeta{ID: idTestToken, Anchor: true})
	rd := resolvedDisk{diskCID: diskCID, volid: "data:vm-90000-disk-0", stableID: idTestToken}
	holder := pve.DiskHolder{Node: "pve1", VMID: 90000, Slot: diskKeyScsi0}

	err := updateParkedDisk(context.Background(), deps, diskCID, rd, holder, map[string]any{"size": 10240, "cache": "writeback"})
	if err == nil {
		t.Fatal("update_disk must fail when the override record can't be written")
	}
	if strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("error %q claims nothing was changed after the size step", err)
	}
	if !strings.Contains(err.Error(), "requested size of 10240 MiB") || !strings.Contains(err.Error(), "option overrides were not recorded") {
		t.Errorf("error %q must name the size the disk has and the overrides that are missing", err)
	}
}

// TestUpdateParkedDisk_OptionOnlyFailureChangedNothing is the control. An
// update with no size changes nothing before the override record fails, so
// the error still says nothing was changed.
func TestUpdateParkedDisk_OptionOnlyFailureChangedNothing(t *testing.T) {
	t.Parallel()

	parkedStr := "data:vm-90000-disk-0,serial=" + idTestToken + ",size=10G"
	c := newIDFakeClient(map[int]map[string]any{
		90000: {"tags": "bosh-cpi;bosh-parker", "protection": true, "digest": "digest-0", diskKeyScsi0: parkedStr},
	})
	c.descWriteErr = errors.New("description write refused")
	deps := overlayTestDeps(c)
	diskCID := overlayCID(t, "data:vm-9001-disk-0", &pve.DiskCIDMeta{ID: idTestToken, Anchor: true})
	rd := resolvedDisk{diskCID: diskCID, volid: "data:vm-90000-disk-0", stableID: idTestToken}
	holder := pve.DiskHolder{Node: "pve1", VMID: 90000, Slot: diskKeyScsi0}

	err := updateParkedDisk(context.Background(), deps, diskCID, rd, holder, map[string]any{"cache": "writeback"})
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("err = %v, want the fail-closed message that nothing was changed", err)
	}
}
