// disk_opt_overlay_internal_test.go — white-box tests for the durable
// drive-option override store: the VM-description sentinel codec, the
// fail-closed writers, the parker provenance carrier, and the serial
// stripping every path applies.
package pve

import (
	"context"
	"strings"
	"testing"
)

const overlayTestStableID = "bpd-ffeeddcc00112233"

func TestDiskOptOverlayFromDesc(t *testing.T) {
	t.Parallel()

	t.Run("dual-keyed lookup prefers the first matching key", func(t *testing.T) {
		t.Parallel()
		desc := `note<!--BOSH:{"bosh_disk_opt_overlays":{` +
			`"` + overlayTestStableID + `":{"cache":"writeback"},` +
			`"data:vm-9001-disk-0":{"cache":"none"}}}-->`
		got := DiskOptOverlayFromDesc(desc, overlayTestStableID, "data:vm-9001-disk-0")
		if got["cache"] != "writeback" {
			t.Errorf("stable-ID entry must win, got %v", got)
		}
		legacy := DiskOptOverlayFromDesc(desc, "", "data:vm-9001-disk-0")
		if legacy["cache"] != "none" {
			t.Errorf("empty keys are skipped and the volid entry found, got %v", legacy)
		}
	})

	t.Run("absent sentinel, absent entry, and corrupt JSON all yield nil", func(t *testing.T) {
		t.Parallel()
		if got := DiskOptOverlayFromDesc("plain description", "k"); got != nil {
			t.Errorf("no sentinel: got %v", got)
		}
		if got := DiskOptOverlayFromDesc(`<!--BOSH:{"bosh_disk_opt_overlays":{"other":{"a":"1"}}}-->`, "k"); got != nil {
			t.Errorf("no entry: got %v", got)
		}
		if got := DiskOptOverlayFromDesc(`<!--BOSH:{"bosh_disk_opt_overlays":"corrupt"}-->`, "k"); got != nil {
			t.Errorf("corrupt key: got %v", got)
		}
	})

	t.Run("a serial key in the stored entry is stripped on read", func(t *testing.T) {
		t.Parallel()
		desc := `<!--BOSH:{"bosh_disk_opt_overlays":{"k":{"serial":"bpd-evil","cache":"writeback"}}}-->`
		got := DiskOptOverlayFromDesc(desc, "k")
		if _, has := got["serial"]; has {
			t.Errorf("serial must never come out of an overlay, got %v", got)
		}
		if got["cache"] != "writeback" {
			t.Errorf("other keys survive the strip, got %v", got)
		}
	})
}

func TestApplyVMDiskOptOverlay(t *testing.T) {
	t.Parallel()

	t.Run("merges updates, keeps empty-string deletion markers, consolidates legacy keys", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			700: {"description": `keep-me<!--BOSH:{"bosh_attached_disks":{"data:vm-9001-disk-0":"pvd-x"},` +
				`"bosh_disk_opt_overlays":{"data:vm-9001-disk-0":{"cache":"none"}}}-->`},
		})
		merged, err := ApplyVMDiskOptOverlay(context.Background(), c, "pve1", 700,
			overlayTestStableID, []string{"data:vm-9001-disk-0"},
			map[string]string{"cache": "writeback", "mbps_rd": ""})
		if err != nil {
			t.Fatalf("ApplyVMDiskOptOverlay: %v", err)
		}
		if merged["cache"] != "writeback" {
			t.Errorf("update must win over the existing entry, got %v", merged)
		}
		if v, has := merged["mbps_rd"]; !has || v != "" {
			t.Errorf("empty-string update is a deletion marker and must be kept, got %v", merged)
		}
		desc, _ := c.configs[700]["description"].(string)
		if !strings.HasPrefix(desc, "keep-me") {
			t.Errorf("nonBOSH text must survive, got %q", desc)
		}
		if !strings.Contains(desc, "bosh_attached_disks") {
			t.Errorf("unrelated sentinel keys must survive, got %q", desc)
		}
		got := DiskOptOverlayFromDesc(desc, overlayTestStableID)
		if got["cache"] != "writeback" {
			t.Errorf("written entry must live under the new key, got %q", desc)
		}
		if legacy := DiskOptOverlayFromDesc(desc, "data:vm-9001-disk-0"); legacy != nil {
			t.Errorf("legacy-keyed entry must be consolidated away, got %v", legacy)
		}
	})

	t.Run("a serial update key is stripped before the write", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{700: {}})
		merged, err := ApplyVMDiskOptOverlay(context.Background(), c, "pve1", 700,
			"k", nil, map[string]string{"serial": "bpd-evil", "cache": "writeback"})
		if err != nil {
			t.Fatalf("ApplyVMDiskOptOverlay: %v", err)
		}
		if _, has := merged["serial"]; has {
			t.Errorf("serial must be stripped, got %v", merged)
		}
		desc, _ := c.configs[700]["description"].(string)
		if strings.Contains(desc, "bpd-evil") {
			t.Errorf("serial must not reach the description, got %q", desc)
		}
	})

	t.Run("config read failure is returned, not swallowed", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{})
		if _, err := ApplyVMDiskOptOverlay(context.Background(), c, "pve1", 700,
			"k", nil, map[string]string{"cache": "writeback"}); err == nil {
			t.Fatal("expected the fail-closed error for an unreadable config")
		}
	})
}

func TestSetAndRemoveVMDiskOptOverlay(t *testing.T) {
	t.Parallel()

	c := newScanFakeClient(map[int]map[string]any{700: {}})
	if err := SetVMDiskOptOverlay(context.Background(), c, "pve1", 700,
		overlayTestStableID, map[string]string{"cache": "writeback"}, "data:vm-9001-disk-0"); err != nil {
		t.Fatalf("SetVMDiskOptOverlay: %v", err)
	}
	desc, _ := c.configs[700]["description"].(string)
	if got := DiskOptOverlayFromDesc(desc, overlayTestStableID); got["cache"] != "writeback" {
		t.Fatalf("entry not recorded: %q", desc)
	}

	RemoveVMDiskOptOverlay(context.Background(), c, nil, "pve1", 700, overlayTestStableID, "data:vm-9001-disk-0")
	desc, _ = c.configs[700]["description"].(string)
	if got := DiskOptOverlayFromDesc(desc, overlayTestStableID); got != nil {
		t.Errorf("entry must be removed, got %v (desc %q)", got, desc)
	}
}

func TestParkerDiskOverlay(t *testing.T) {
	t.Parallel()

	t.Run("park context opts land in the provenance entry and read back", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			90000: {"tags": "bosh-cpi;bosh-parker", paramProtection: true},
		})
		pctx := ParkContext{
			DiskCID:  "pvd-x",
			StableID: overlayTestStableID,
			Opts:     map[string]string{"cache": "writeback", "serial": "bpd-evil"},
		}
		entry := buildParkerProvEntry("pve1", "data:vm-90000-disk-0", "scsi0", transferTestCfg, pctx)
		if err := writeParkerProvenance(context.Background(), c, nil, "pve1", 90000, overlayTestStableID, entry, parkerTestCfgInternal()); err != nil {
			t.Fatalf("writeParkerProvenance: %v", err)
		}
		got, err := ReadParkerDiskOverlay(context.Background(), c, "pve1", 90000, "data:vm-90000-disk-0", overlayTestStableID)
		if err != nil {
			t.Fatalf("ReadParkerDiskOverlay: %v", err)
		}
		if got["cache"] != "writeback" {
			t.Errorf("opts must round-trip through the provenance entry, got %v", got)
		}
		if _, has := got["serial"]; has {
			t.Errorf("serial must be stripped at both ends, got %v", got)
		}
	})

	t.Run("read matches the entry by its recorded volid too", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			90000: {"tags": "bosh-cpi;bosh-parker"},
		})
		entry := buildParkerProvEntry("pve1", "data:vm-90000-disk-3", "scsi3", transferTestCfg, ParkContext{
			DiskCID: "pvd-x", StableID: overlayTestStableID, Opts: map[string]string{"iothread": "0"},
		})
		if err := writeParkerProvenance(context.Background(), c, nil, "pve1", 90000, overlayTestStableID, entry, parkerTestCfgInternal()); err != nil {
			t.Fatalf("writeParkerProvenance: %v", err)
		}
		// Look up with no stable ID and the current volid alone.
		got, err := ReadParkerDiskOverlay(context.Background(), c, "pve1", 90000, "data:vm-90000-disk-3", "")
		if err != nil {
			t.Fatalf("ReadParkerDiskOverlay: %v", err)
		}
		if got["iothread"] != "0" {
			t.Errorf("volid-field match must find the entry, got %v", got)
		}
	})

	t.Run("apply updates an existing entry without disturbing its identity fields", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			90000: {"tags": "bosh-cpi;bosh-parker"},
		})
		entry := buildParkerProvEntry("pve1", "data:vm-90000-disk-0", "scsi0", transferTestCfg, ParkContext{
			DiskCID: "pvd-x", SourceVMCID: "700", StableID: overlayTestStableID,
			Opts: map[string]string{"cache": "none"},
		})
		if err := writeParkerProvenance(context.Background(), c, nil, "pve1", 90000, overlayTestStableID, entry, parkerTestCfgInternal()); err != nil {
			t.Fatalf("writeParkerProvenance: %v", err)
		}
		merged, err := ApplyParkerDiskOverlay(context.Background(), c, "pve1", 90000,
			"data:vm-90000-disk-0", overlayTestStableID, "pvd-x",
			map[string]string{"cache": "writeback", "mbps_rd": ""}, transferTestCfg)
		if err != nil {
			t.Fatalf("ApplyParkerDiskOverlay: %v", err)
		}
		if merged["cache"] != "writeback" || merged["mbps_rd"] != "" {
			t.Errorf("merge semantics: got %v", merged)
		}
		disks := c.parkedEntries(t)
		got := disks[overlayTestStableID]
		if got.SourceVMCID != "700" || got.DiskCID != "pvd-x" || got.Slot != "scsi0" {
			t.Errorf("identity fields must survive the overlay update, got %+v", got)
		}
		if got.Opts["cache"] != "writeback" {
			t.Errorf("entry opts not updated: %+v", got)
		}
	})

	t.Run("apply creates the entry when the best-effort park never recorded one", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			90000: {"tags": "bosh-cpi;bosh-parker", "scsi2": "data:vm-90000-disk-0,serial=" + overlayTestStableID},
		})
		merged, err := ApplyParkerDiskOverlay(context.Background(), c, "pve1", 90000,
			"data:vm-90000-disk-0", overlayTestStableID, "pvd-x",
			map[string]string{"cache": "writeback"}, transferTestCfg)
		if err != nil {
			t.Fatalf("ApplyParkerDiskOverlay: %v", err)
		}
		if merged["cache"] != "writeback" {
			t.Errorf("merged = %v", merged)
		}
		disks := c.parkedEntries(t)
		got, ok := disks[overlayTestStableID]
		if !ok {
			t.Fatalf("entry must be created under the stable ID, got %v", disks)
		}
		if got.Slot != "scsi2" || got.Volid != "data:vm-90000-disk-0" {
			t.Errorf("created entry must record the live slot and volid, got %+v", got)
		}
	})
}

func TestResumeDiskTransferKeepsOverlay(t *testing.T) {
	t.Parallel()

	// Crash window: everything landed, only the finalize was lost. The intent
	// record carries the opts; the resumed finalize must re-persist them
	// rather than rewriting the entry without.
	c := newScanFakeClient(map[int]map[string]any{
		90000: {
			"tags":  "bosh-cpi;bosh-parker",
			"scsi0": "data:vm-90000-disk-0,serial=" + transferStableID,
		},
	})
	intentEntry := buildParkerProvEntry("pve1", "data:vm-9001-disk-0", "scsi0", transferTestCfg, ParkContext{
		DiskCID: "pvd-x", SourceVMCID: "700", StableID: transferStableID,
		Opts: map[string]string{"cache": "writeback"},
	})
	if err := writeParkerProvenance(context.Background(), c, nil, "pve1", 90000, transferStableID, intentEntry, parkerTestCfgInternal()); err != nil {
		t.Fatalf("writeParkerProvenance: %v", err)
	}

	intent := DiskTransferIntent{
		ParkerVMID: 90000, ParkerNode: "pve1", Slot: "scsi0",
		Volid: "data:vm-9001-disk-0", SourceVMCID: "700",
		Opts: map[string]string{"cache": "writeback"},
	}
	landed, err := ResumeDiskTransferToParker(context.Background(), c, nil, intent, transferStableID,
		transferTestCfg, ParkContext{DiskCID: "pvd-x", SourceVMCID: "700"})
	if err != nil {
		t.Fatalf("ResumeDiskTransferToParker: %v", err)
	}
	if landed != "data:vm-90000-disk-0" {
		t.Errorf("landed = %q", landed)
	}
	disks := c.parkedEntries(t)
	if disks[transferStableID].Opts["cache"] != "writeback" {
		t.Errorf("resume must re-persist the recorded overrides, got %+v", disks[transferStableID])
	}
}
