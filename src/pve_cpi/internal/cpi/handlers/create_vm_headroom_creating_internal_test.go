package handlers

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// creatingRow is a guest that PVE lists before it reports the guest's memory,
// as it does for a sibling create still in flight, on node pve02. extra adds the row's lock
// or status.
func creatingRow(vmid int, extra map[string]any) map[string]any {
	row := map[string]any{"type": "qemu", "vmid": vmid, "node": "pve02", "template": 0}
	for k, v := range extra {
		row[k] = v
	}
	return row
}

// TestHeadroom_AGuestStillBeingCreatedDoesNotBlockTheEstimate pins that a
// guest another call is still creating leaves the estimate running. Such a
// guest carries a create or clone lock, or PVE reports its status as unknown
// until its status daemon first reports it (on a node that reports other
// guests), and in both cases PVE lists no
// memory for it yet. The estimate leaves it out with a Debug line, so warn
// mode logs no warning and enforce mode places the VM. A guest that PVE
// reports as running or stopped with no memory still makes the estimate
// unavailable.
func TestHeadroom_AGuestStillBeingCreatedDoesNotBlockTheEstimate(t *testing.T) {
	t.Parallel()
	pending := map[string]map[string]any{
		"clone lock":     {"lock": "clone", "status": "stopped"},
		"create lock":    {"lock": "create"},
		"unknown status": {"status": "unknown"},
	}
	for name, extra := range pending {
		for _, mode := range []string{config.HAHeadroomWarn, config.HAHeadroomEnforce} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				t.Parallel()
				rig := newHeadroomRig(t, config.HAModeAuto, mode,
					[]map[string]any{creatingRow(210123, extra),
						creatingRow(210124, map[string]any{"status": "running", "maxmem": 512 * headroomMiB})}, nil)
				node, _, err := rig.place(t, createVMCloudProps{})
				if err != nil || node == "" {
					t.Fatalf("placement = %q, %v; want the VM placed", node, err)
				}
				for _, e := range rig.logs.All() {
					if e.Level >= log.LevelWarn && strings.Contains(e.Message, "headroom") {
						t.Errorf("warning %q %v; want none for a guest still being created", e.Message, e.Attrs)
					}
				}
				if !hasDebug(rig.logs, "headroom estimate leaves out a guest that PVE is still creating") {
					t.Errorf("no Debug line for the guest; logs: %v", rig.logs.All())
				}
			})
		}
	}

	t.Run("settled guest with no memory", func(t *testing.T) {
		t.Parallel()
		rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomWarn,
			[]map[string]any{creatingRow(210123, map[string]any{"status": "stopped"})}, nil)
		if node, _, err := rig.place(t, createVMCloudProps{}); err != nil || node == "" {
			t.Fatalf("placement = %q, %v; want the VM placed in warn mode", node, err)
		}
		if _, ok := hasWarn(rig.logs, "guest 210123 on pve02 has no configured memory"); !ok {
			t.Errorf("no unavailable warning for the settled guest; logs: %v", rig.logs.All())
		}
	})
}

// TestHeadroom_AnUnknownStatusCountsAsCreatingOnlyOnANodeThatReportsItsGuests
// pins that the unknown status marks a guest as still being created only when
// another guest on its node has a known status, because a node whose status
// daemon has stalled lists every guest as unknown. The create lock does not
// depend on the node.
func TestHeadroom_AnUnknownStatusCountsAsCreatingOnlyOnANodeThatReportsItsGuests(t *testing.T) {
	t.Parallel()
	running := creatingRow(210124, map[string]any{"status": "running", "maxmem": 512 * headroomMiB})

	t.Run("unknown beside a running guest is left out", func(t *testing.T) {
		t.Parallel()
		for _, mode := range []string{config.HAHeadroomWarn, config.HAHeadroomEnforce} {
			rig := newHeadroomRig(t, config.HAModeAuto, mode, []map[string]any{
				creatingRow(210123, map[string]any{"status": "unknown"}), running}, nil)
			if node, _, err := rig.place(t, createVMCloudProps{}); err != nil || node == "" {
				t.Fatalf("%s: placement = %q, %v; want the VM placed", mode, node, err)
			}
			if !hasDebug(rig.logs, "headroom estimate leaves out a guest that PVE is still creating") {
				t.Errorf("%s: no Debug line for the guest; logs: %v", mode, rig.logs.All())
			}
		}
	})

	t.Run("every guest unknown on one node keeps the estimate unavailable", func(t *testing.T) {
		t.Parallel()
		unknown := func() []map[string]any {
			return []map[string]any{
				creatingRow(210123, map[string]any{"status": "unknown"}),
				creatingRow(210125, map[string]any{"status": "unknown"}),
			}
		}
		warnRig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomWarn, unknown(), nil)
		if node, _, err := warnRig.place(t, createVMCloudProps{}); err != nil || node == "" {
			t.Fatalf("placement = %q, %v; want the VM placed in warn mode", node, err)
		}
		if _, ok := hasWarn(warnRig.logs, "guest 210123 on pve02 has no configured memory"); !ok {
			t.Errorf("no unavailable warning; logs: %v", warnRig.logs.All())
		}
		enforceRig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomEnforce, unknown(), nil)
		if _, _, err := enforceRig.place(t, createVMCloudProps{}); err == nil {
			t.Errorf("placement succeeded; want a refusal in enforce mode")
		}
	})

	t.Run("create lock is left out whatever the node reports", func(t *testing.T) {
		t.Parallel()
		rig := newHeadroomRig(t, config.HAModeAuto, config.HAHeadroomEnforce, []map[string]any{
			creatingRow(210123, map[string]any{"lock": "create"}),
			creatingRow(210125, map[string]any{"status": "unknown"}),
		}, nil)
		// The unknown sibling has no memory and is settled, so only the locked
		// guest is judged here: give the sibling memory to isolate the lock.
		rig.cluster.guests[1]["maxmem"] = 512 * headroomMiB
		if node, _, err := rig.place(t, createVMCloudProps{}); err != nil || node == "" {
			t.Fatalf("placement = %q, %v; want the VM placed", node, err)
		}
		if !hasDebug(rig.logs, "headroom estimate leaves out a guest that PVE is still creating") {
			t.Errorf("no Debug line for the locked guest; logs: %v", rig.logs.All())
		}
	})
}
