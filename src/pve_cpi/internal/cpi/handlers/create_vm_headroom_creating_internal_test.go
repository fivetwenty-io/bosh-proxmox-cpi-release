package handlers

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// creatingRow is a guest that PVE lists before it reports the guest's memory,
// as it does for a sibling create still in flight. extra adds the row's lock
// or status.
func creatingRow(vmid int, node string, extra map[string]any) map[string]any {
	row := map[string]any{"type": "qemu", "vmid": vmid, "node": node, "template": 0}
	for k, v := range extra {
		row[k] = v
	}
	return row
}

// TestHeadroom_AGuestStillBeingCreatedDoesNotBlockTheEstimate pins that a
// guest another call is still creating leaves the estimate running. Such a
// guest carries a create or clone lock, or PVE reports its status as unknown
// until its status daemon first reports it, and in both cases PVE lists no
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
					[]map[string]any{creatingRow(210123, "pve02", extra)}, nil)
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
			[]map[string]any{creatingRow(210123, "pve02", map[string]any{"status": "stopped"})}, nil)
		if node, _, err := rig.place(t, createVMCloudProps{}); err != nil || node == "" {
			t.Fatalf("placement = %q, %v; want the VM placed in warn mode", node, err)
		}
		if _, ok := hasWarn(rig.logs, "guest 210123 on pve02 has no configured memory"); !ok {
			t.Errorf("no unavailable warning for the settled guest; logs: %v", rig.logs.All())
		}
	})
}
