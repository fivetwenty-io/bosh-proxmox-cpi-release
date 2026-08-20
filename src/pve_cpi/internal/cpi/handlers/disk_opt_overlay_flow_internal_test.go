// disk_opt_overlay_flow_internal_test.go — white-box tests for durable disk
// option updates: the merge order at attach (global < CID opts < recorded
// overrides), the invariant baseline that absorbs operator updates, the
// full update → park → attach round trip with its write ordering, the
// record-only path for parked disks, and the fail-closed record write.
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// ListSnapshots gives the identity fake the one read the detach path's
// snapshot guard needs; no test here stages snapshots.
func (q *idFakeQEMU) ListSnapshots(_ context.Context, _ string, _ int) ([]map[string]any, error) {
	return nil, nil
}

// overlayTestDeps builds Deps whose global perf resolution contributes
// nothing on its own (iothread off, no cache, ssd/discard unresolvable), so
// every option in an asserted drive string is attributable to the layer a
// test placed it in.
func overlayTestDeps(c *idFakeClient) Deps {
	off := false
	return Deps{
		Config: &config.CPIConfig{
			Node:            "pve1",
			DiskStorage:     "data",
			DiskPerformance: &config.DiskPerformanceDefaults{Iothread: &off},
		},
		PVE: c,
	}
}

// overlayCID encodes a disk CID for the flow tests.
func overlayCID(t *testing.T, bare string, meta *pve.DiskCIDMeta) string {
	t.Helper()
	cid, err := pve.EncodeDiskCID(bare, meta)
	if err != nil {
		t.Fatalf("EncodeDiskCID: %v", err)
	}
	return cid
}

// overlayArgs marshals update_disk's positional arguments.
func overlayArgs(t *testing.T, diskCID string, spec map[string]any) []json.RawMessage {
	t.Helper()
	rawCID, err := json.Marshal(diskCID)
	if err != nil {
		t.Fatalf("marshal disk_cid: %v", err)
	}
	rawSpec, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal update_spec: %v", err)
	}
	return []json.RawMessage{rawCID, rawSpec}
}

// TestAttachDiskCore_MergeOrder pins the three-layer merge: global config,
// then CID-recorded options, then recorded operator overrides, rightmost
// winning, with an empty-string value at any layer deleting the key.
func TestAttachDiskCore_MergeOrder(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		global     func(*config.DiskPerformanceDefaults)
		metaOpts   map[string]string
		overlay    map[string]string
		wantInStr  []string
		wantAbsent []string
	}{
		{
			name:      "CID opts override global",
			global:    func(d *config.DiskPerformanceDefaults) { d.Cache = cmNone },
			metaOpts:  map[string]string{"cache": "writeback"},
			wantInStr: []string{"cache=writeback"},
		},
		{
			name:      "overrides win over CID opts and global",
			global:    func(d *config.DiskPerformanceDefaults) { d.Cache = cmNone },
			metaOpts:  map[string]string{"cache": "writeback"},
			overlay:   map[string]string{"cache": "unsafe", "mbps_rd": "50"},
			wantInStr: []string{"cache=unsafe", "mbps_rd=50"},
		},
		{
			// A throttle key, deliberately: an empty-string value recorded in
			// CID opts stays present-but-empty in the invariant baseline, so
			// exercising CID-level deletion on an invariant key would trip the
			// (pre-existing) presence check rather than the merge under test.
			name:       "empty-string CID value deletes the global key",
			global:     func(d *config.DiskPerformanceDefaults) { v := 100.0; d.MBpsRd = &v },
			metaOpts:   map[string]string{"mbps_rd": ""},
			wantAbsent: []string{"mbps_rd="},
		},
		{
			name:       "empty-string override deletes the CID key",
			global:     func(d *config.DiskPerformanceDefaults) { d.Cache = cmNone },
			metaOpts:   map[string]string{"cache": "writeback"},
			overlay:    map[string]string{"cache": ""},
			wantAbsent: []string{"cache="},
		},
		{
			name:      "override applies to a legacy CID with no recorded opts",
			global:    func(d *config.DiskPerformanceDefaults) { d.Cache = cmNone },
			overlay:   map[string]string{"cache": "writeback"},
			wantInStr: []string{"cache=writeback"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newIDFakeClient(map[int]map[string]any{700: {}})
			deps := overlayTestDeps(c)
			tc.global(deps.Config.DiskPerformance)
			var meta *pve.DiskCIDMeta
			if tc.metaOpts != nil {
				meta = &pve.DiskCIDMeta{Opts: tc.metaOpts}
			}
			rd := resolvedDisk{
				diskCID: "pvd-x",
				birth:   "data:vm-9001-disk-0",
				volid:   "data:vm-9001-disk-0",
				meta:    meta,
			}
			if _, _, err := attachDiskCore(context.Background(), deps, "attach_disk", "700", "pve1", 700,
				"pvd-x", rd, attachPlan{overlay: tc.overlay}); err != nil {
				t.Fatalf("attachDiskCore: %v", err)
			}
			got, _ := c.configs[700]["scsi1"].(string)
			for _, want := range tc.wantInStr {
				if !strings.Contains(got, want) {
					t.Errorf("drive string %q must contain %q", got, want)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("drive string %q must not contain %q", got, absent)
				}
			}
		})
	}
}

// TestEnforceDiskPerfInvariants_OverlayBaseline pins the invariant baseline:
// an invariant key an operator updated (and the CPI recorded) is intended,
// while an unrecorded divergence still trips enforce mode.
func TestEnforceDiskPerfInvariants_OverlayBaseline(t *testing.T) {
	t.Parallel()
	cfg := &config.CPIConfig{} // empty → enforce
	meta := &pve.DiskCIDMeta{Opts: map[string]string{"cache": cmNone}}

	t.Run("recorded operator update passes enforce mode", func(t *testing.T) {
		t.Parallel()
		overlay := map[string]string{"cache": "writeback"}
		effective := map[string]string{"cache": "writeback"}
		if err := enforceDiskPerfInvariants(cfg, nil, "attach_disk", "100", "cid", meta, overlay, effective); err != nil {
			t.Errorf("recorded update must not read as a divergence, got: %v", err)
		}
	})

	t.Run("unrecorded divergence still fails with an overlay present", func(t *testing.T) {
		t.Parallel()
		overlay := map[string]string{"mbps_rd": "50"}
		// cache diverges from both the creation record and the overlay.
		effective := map[string]string{"cache": "writeback", "mbps_rd": "50"}
		err := enforceDiskPerfInvariants(cfg, nil, "attach_disk", "100", "cid", meta, overlay, effective)
		if err == nil {
			t.Fatal("unrecorded cache divergence must still reject")
		}
		if !cpierrors.IsType(err, cpierrors.TypeCloud) {
			t.Errorf("want TypeCloud, got %v", err)
		}
	})

	t.Run("override deletion of an invariant key passes", func(t *testing.T) {
		t.Parallel()
		overlay := map[string]string{"cache": ""}
		effective := map[string]string{}
		if err := enforceDiskPerfInvariants(cfg, nil, "attach_disk", "100", "cid", meta, overlay, effective); err != nil {
			t.Errorf("recorded deletion must not read as a divergence, got: %v", err)
		}
	})
}

// TestUpdateDiskOverlay_RoundTrip drives the full durable-update cycle on a
// stable-ID disk: update while attached (record + in-place rewrite), detach
// (the record rides the transfer into the parker's provenance), attach to
// another VM (the record merges into the drive string and is re-recorded on
// the receiver before the parker's record is removed).
func TestUpdateDiskOverlay_RoundTrip(t *testing.T) {
	t.Parallel()

	const birth = "data:vm-9001-disk-0"
	attachedVolid := "data:vm-700-disk-1"
	c := newIDFakeClient(map[int]map[string]any{
		700:   {"scsi1": attachedVolid + ",serial=" + idTestToken + ",size=10G"},
		701:   {},
		90000: {"tags": "bosh-cpi;bosh-parker", "protection": true},
	})
	deps := overlayTestDeps(c)
	diskCID := overlayCID(t, birth, &pve.DiskCIDMeta{ID: idTestToken, Anchor: true, Opts: map[string]string{"cache": cmNone}})
	ctx := context.Background()

	// 1. update_disk while attached: the override is recorded on the holder
	// and the live drive string updated in place, serial preserved.
	h := HandleUpdateDisk(deps)
	if _, err := h.Handle(ctx, overlayArgs(t, diskCID, map[string]any{"cache": "writeback", "mbps_rd": 50}), jsonrpc.Context{}); err != nil {
		t.Fatalf("update_disk (attached): %v", err)
	}
	drive, _ := c.configs[700]["scsi1"].(string)
	for _, want := range []string{"cache=writeback", "mbps_rd=50", "serial=" + idTestToken, "size=10G"} {
		if !strings.Contains(drive, want) {
			t.Errorf("holder drive string %q must contain %q", drive, want)
		}
	}
	desc, _ := c.configs[700]["description"].(string)
	recorded := pve.DiskOptOverlayFromDesc(desc, idTestToken)
	if recorded["cache"] != "writeback" || recorded["mbps_rd"] != "50" {
		t.Fatalf("holder must record the override map, got %v (desc %q)", recorded, desc)
	}

	// 2. detach_disk: the record rides the transfer into the parker's
	// provenance entry, and the ex-holder's record comes off.
	bare, meta, decErr := decodeDiskCID(ctx, deps, "detach_disk", diskCID)
	if decErr != nil {
		t.Fatalf("decode: %v", decErr)
	}
	rd, resolveErr := resolveDiskForOp(ctx, deps, "detach_disk", diskCID, bare, meta)
	if resolveErr != nil {
		t.Fatalf("resolve: %v", resolveErr)
	}
	if err := handleDetachStableID(ctx, deps, "700", 700, rd); err != nil {
		t.Fatalf("detach: %v", err)
	}
	parkerDesc, _ := c.configs[90000]["description"].(string)
	parkedOverlay := diskOptOverlayFromParkerDesc(parkerDesc, idTestToken)
	if parkedOverlay["cache"] != "writeback" || parkedOverlay["mbps_rd"] != "50" {
		t.Fatalf("parker provenance must carry the override map, got %v (desc %q)", parkedOverlay, parkerDesc)
	}
	exDesc, _ := c.configs[700]["description"].(string)
	if got := pve.DiskOptOverlayFromDesc(exDesc, idTestToken, attachedVolid, birth); got != nil {
		t.Errorf("ex-holder must drop the record, got %v", got)
	}
	// The parker slot's drive string stays CPI-owned: volid plus serial only.
	parkedSlot, parkedStr := findSlotWithSerial(t, c.configs[90000], idTestToken)
	if strings.Contains(parkedStr, "cache=") || strings.Contains(parkedStr, "mbps_rd=") {
		t.Errorf("parker slot %s carries baked options: %q", parkedSlot, parkedStr)
	}

	// 3. attach to another VM: the override merges into the drive string over
	// the CID's recorded cache=none, and the receiver records it.
	rd3, resolveErr3 := resolveDiskForOp(ctx, deps, "attach_disk", diskCID, bare, meta)
	if resolveErr3 != nil {
		t.Fatalf("re-resolve: %v", resolveErr3)
	}
	plan, guardErr := guardAndUnparkBeforeAttach(ctx, deps, "attach_disk", &rd3, "pve1", 701)
	if guardErr != nil {
		t.Fatalf("guard: %v", guardErr)
	}
	if _, _, err := attachDiskCore(ctx, deps, "attach_disk", "701", "pve1", 701, diskCID, rd3, plan); err != nil {
		t.Fatalf("attach: %v", err)
	}
	_, landedStr := findSlotWithSerial(t, c.configs[701], idTestToken)
	for _, want := range []string{"cache=writeback", "mbps_rd=50", "serial=" + idTestToken} {
		if !strings.Contains(landedStr, want) {
			t.Errorf("landed drive string %q must contain %q", landedStr, want)
		}
	}
	newDesc, _ := c.configs[701]["description"].(string)
	if got := pve.DiskOptOverlayFromDesc(newDesc, idTestToken); got["cache"] != "writeback" {
		t.Errorf("receiver must re-record the override map, got %v", got)
	}
	finalParkerDesc, _ := c.configs[90000]["description"].(string)
	if got := diskOptOverlayFromParkerDesc(finalParkerDesc, idTestToken); got != nil {
		t.Errorf("parker provenance entry must be gone after the attach, got %v", got)
	}

	// Write ordering: the receiver's record landed before the parker write
	// that removed the provenance entry.
	receiverIdx := -1
	for i, w := range c.descWrites {
		if w.vmid == 701 && strings.Contains(w.desc, "bosh_disk_opt_overlays") && strings.Contains(w.desc, "writeback") {
			receiverIdx = i
			break
		}
	}
	removalIdx := -1
	sawEntry := false
	for i, w := range c.descWrites {
		if w.vmid != 90000 {
			continue
		}
		if strings.Contains(w.desc, idTestToken) {
			sawEntry = true
			continue
		}
		if sawEntry {
			removalIdx = i
			break
		}
	}
	if receiverIdx < 0 || removalIdx < 0 {
		t.Fatalf("write log missing the receiver record (%d) or the parker removal (%d): %+v",
			receiverIdx, removalIdx, c.descWrites)
	}
	if receiverIdx > removalIdx {
		t.Errorf("receiving-side record (index %d) must land before the parker record removal (index %d)",
			receiverIdx, removalIdx)
	}
}

// diskOptOverlayFromParkerDesc reads the opts field of a parker provenance
// entry the way the external tooling does: raw sentinel JSON, no CPI codec.
func diskOptOverlayFromParkerDesc(desc, key string) map[string]string {
	start := strings.Index(desc, "<!--BOSH:")
	end := strings.LastIndex(desc, "-->")
	if start < 0 || end < 0 {
		return nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(desc[start+len("<!--BOSH:"):end]), &top); err != nil {
		return nil
	}
	var disks map[string]struct {
		Opts map[string]string `json:"opts"`
	}
	if err := json.Unmarshal(top["bosh_parked_disks"], &disks); err != nil {
		return nil
	}
	entry, ok := disks[key]
	if !ok {
		return nil
	}
	return entry.Opts
}

// findSlotWithSerial returns the one active slot whose drive string carries
// the given stable-ID serial.
func findSlotWithSerial(t *testing.T, cfg map[string]any, token string) (string, string) {
	t.Helper()
	for slot, optStr := range qemu.ParseDisks(cfg) {
		if serial, ok := pve.StableIDFromDriveOptStr(optStr); ok && serial == token {
			return slot, optStr
		}
	}
	t.Fatalf("no slot carries serial %s in %v", token, cfg)
	return "", ""
}

// TestUpdateDisk_WhileParked_RecordsWithoutRewriting pins the parked path:
// the override is recorded in the parker's provenance entry and the parker
// slot's drive string is not touched.
func TestUpdateDisk_WhileParked_RecordsWithoutRewriting(t *testing.T) {
	t.Parallel()

	t.Run("stable-ID disk", func(t *testing.T) {
		t.Parallel()
		parkedStr := "data:vm-90000-disk-0,serial=" + idTestToken
		c := newIDFakeClient(map[int]map[string]any{
			90000: {"tags": "bosh-cpi;bosh-parker", "protection": true, diskKeyScsi0: parkedStr},
		})
		deps := overlayTestDeps(c)
		diskCID := overlayCID(t, "data:vm-9001-disk-0", &pve.DiskCIDMeta{ID: idTestToken, Anchor: true})

		h := HandleUpdateDisk(deps)
		if _, err := h.Handle(context.Background(), overlayArgs(t, diskCID, map[string]any{"cache": "writeback"}), jsonrpc.Context{}); err != nil {
			t.Fatalf("update_disk (parked): %v", err)
		}
		if got, _ := c.configs[90000][diskKeyScsi0].(string); got != parkedStr {
			t.Errorf("parker drive string must stay untouched: %q -> %q", parkedStr, got)
		}
		desc, _ := c.configs[90000]["description"].(string)
		if got := diskOptOverlayFromParkerDesc(desc, idTestToken); got["cache"] != "writeback" {
			t.Errorf("provenance entry must record the override, got %v (desc %q)", got, desc)
		}
	})

	t.Run("legacy disk parked by config edit", func(t *testing.T) {
		t.Parallel()
		const volid = "local-lvm:vm-9001-disk-0"
		c := newIDFakeClient(map[int]map[string]any{
			90000: {"tags": "bosh-cpi;bosh-parker", "protection": true, diskKeyScsi0: volid},
		})
		deps := overlayTestDeps(c)
		diskCID := overlayCID(t, volid, nil)

		h := HandleUpdateDisk(deps)
		if _, err := h.Handle(context.Background(), overlayArgs(t, diskCID, map[string]any{"iothread": true}), jsonrpc.Context{}); err != nil {
			t.Fatalf("update_disk (legacy parked): %v", err)
		}
		if got, _ := c.configs[90000][diskKeyScsi0].(string); got != volid {
			t.Errorf("parker drive string must stay untouched: %q -> %q", volid, got)
		}
		desc, _ := c.configs[90000]["description"].(string)
		if got := diskOptOverlayFromParkerDesc(desc, volid); got["iothread"] != "1" {
			t.Errorf("volid-keyed provenance entry must record the override, got %v (desc %q)", got, desc)
		}
	})
}

// TestUpdateDisk_RecordWriteFails_FailClosed pins the fail-closed contract:
// when the override record cannot be written, update_disk errors and the
// drive string is untouched.
func TestUpdateDisk_RecordWriteFails_FailClosed(t *testing.T) {
	t.Parallel()

	const driveStr = "data:vm-700-disk-1,serial=" + idTestToken + ",size=10G"
	c := newIDFakeClient(map[int]map[string]any{
		700: {"scsi1": driveStr},
	})
	c.descWriteErr = errors.New("description write refused")
	deps := overlayTestDeps(c)
	diskCID := overlayCID(t, "data:vm-9001-disk-0", &pve.DiskCIDMeta{ID: idTestToken})

	h := HandleUpdateDisk(deps)
	_, err := h.Handle(context.Background(), overlayArgs(t, diskCID, map[string]any{"cache": "writeback"}), jsonrpc.Context{})
	if err == nil {
		t.Fatal("update_disk must fail when the record write fails")
	}
	if got, _ := c.configs[700]["scsi1"].(string); got != driveStr {
		t.Errorf("drive string must stay untouched on a failed record write: %q -> %q", driveStr, got)
	}
}

// TestUpdateDisk_SerialCannotBeRewritten pins serial ownership: an update_spec
// smuggling a serial key neither rewrites the live drive serial nor records a
// serial override.
func TestUpdateDisk_SerialCannotBeRewritten(t *testing.T) {
	t.Parallel()

	c := newIDFakeClient(map[int]map[string]any{
		700: {"scsi1": "data:vm-700-disk-1,serial=" + idTestToken},
	})
	deps := overlayTestDeps(c)
	diskCID := overlayCID(t, "data:vm-9001-disk-0", &pve.DiskCIDMeta{ID: idTestToken})

	h := HandleUpdateDisk(deps)
	if _, err := h.Handle(context.Background(), overlayArgs(t, diskCID, map[string]any{
		"serial": "bpd-evil0000000000",
		"cache":  "writeback",
	}), jsonrpc.Context{}); err != nil {
		t.Fatalf("update_disk: %v", err)
	}
	drive, _ := c.configs[700]["scsi1"].(string)
	if !strings.Contains(drive, "serial="+idTestToken) || strings.Contains(drive, "bpd-evil") {
		t.Errorf("drive serial must be preserved verbatim, got %q", drive)
	}
	desc, _ := c.configs[700]["description"].(string)
	if got := pve.DiskOptOverlayFromDesc(desc, idTestToken); got == nil || got["cache"] != "writeback" {
		t.Fatalf("cache override must still be recorded, got %v", got)
	} else if _, has := got["serial"]; has {
		t.Errorf("a serial must never be recorded as an override, got %v", got)
	}
}

// TestDetachForeignActiveDisks_ThreadsOverlay extends the delete_vm
// preservation guarantee to the override record: a stable-ID disk's recorded
// overrides move to the parker with the disk, before the VM (and the record's
// carrier description) is destroyed.
func TestDetachForeignActiveDisks_ThreadsOverlay(t *testing.T) {
	t.Parallel()

	overlayDesc := `<!--BOSH:{"bosh_disk_opt_overlays":{"` + idTestToken + `":{"cache":"writeback"}}}-->`
	c := newIDFakeClient(map[int]map[string]any{
		700: {
			"virtio0":     "data:vm-700-disk-0,size=20G",
			"scsi1":       "data:vm-700-disk-1,serial=" + idTestToken + ",size=10G",
			"description": overlayDesc,
		},
		90000: {"tags": "bosh-cpi;bosh-parker", "protection": true},
	})
	deps := idTestDeps(c)
	if err := detachForeignActiveDisks(context.Background(), deps, "pve1", "700", 700, deps.Log(context.Background())); err != nil {
		t.Fatalf("detachForeignActiveDisks: %v", err)
	}
	parkerDesc, _ := c.configs[90000]["description"].(string)
	if got := diskOptOverlayFromParkerDesc(parkerDesc, idTestToken); got["cache"] != "writeback" {
		t.Errorf("parker provenance must carry the doomed VM's recorded overrides, got %v (desc %q)", got, parkerDesc)
	}
}

// TestGuardAndUnpark_LegacyParkedOverlay covers the legacy config-edit unpark
// path: a volid-keyed provenance entry's opts reach the attach merge and the
// receiving VM's record, with no stable ID anywhere.
func TestGuardAndUnpark_LegacyParkedOverlay(t *testing.T) {
	t.Parallel()

	const volid = "local-lvm:vm-9001-disk-0"
	parkerDesc := fmt.Sprintf(
		`<!--BOSH:{"bosh_parked_disks":{%q:{"disk_cid":%q,"parked_at":"2026-08-20T00:00:00Z","node":"pve1","opts":{"cache":"writeback"}}}}-->`,
		volid, volid)
	c := newIDFakeClient(map[int]map[string]any{
		701:   {},
		90000: {"tags": "bosh-cpi;bosh-parker", "protection": true, diskKeyScsi0: volid, "description": parkerDesc},
	})
	deps := overlayTestDeps(c)

	rd := resolvedDisk{diskCID: volid, birth: volid, volid: volid}
	plan, err := guardAndUnparkBeforeAttach(context.Background(), deps, "attach_disk", &rd, "pve1", 701)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if plan.overlay["cache"] != "writeback" {
		t.Fatalf("plan must carry the provenance opts, got %+v", plan)
	}
	if _, _, err := attachDiskCore(context.Background(), deps, "attach_disk", "701", "pve1", 701, volid, rd, plan); err != nil {
		t.Fatalf("attachDiskCore: %v", err)
	}
	got, _ := c.configs[701]["scsi1"].(string)
	if !strings.Contains(got, "cache=writeback") {
		t.Errorf("legacy disk's recorded override must reach the drive string, got %q", got)
	}
	desc, _ := c.configs[701]["description"].(string)
	if o := pve.DiskOptOverlayFromDesc(desc, volid); o["cache"] != "writeback" {
		t.Errorf("receiving VM must record the override under the volid key, got %v", o)
	}
}
