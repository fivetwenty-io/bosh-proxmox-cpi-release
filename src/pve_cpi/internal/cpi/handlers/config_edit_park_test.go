package handlers

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// requireConfigEditPark proves a stable-ID disk parked by config edit off VM
// 777, the source in every row that uses it. A volume that 777 doesn't own by
// name has no unused entry to move, so the transfer attaches it to a parker
// under the name it already had. The disk must sit on exactly one parker slot,
// carrying its serial and naming volume, and neither view of 777 may name it.
// The applied view is the config the endpoint returns, and the pending view is
// the delete the fake's pending model holds. A count of zero moves alone
// doesn't show this, because a transfer that never ran would count zero too.
func requireConfigEditPark(t *testing.T, client *lifecycleFlowPVE, serial, volume string) {
	t.Helper()
	const source = 777
	var holders []string
	for vmid, cfg := range client.state.configs {
		for key, value := range cfg {
			text, _ := value.(string)
			if !isDiskOptionKey(key) || !strings.Contains(text, "serial="+serial) {
				continue
			}
			holders = append(holders, fmt.Sprintf("%d.%s", vmid, key))
			if name := strings.Split(text, ",")[0]; name != volume {
				t.Fatalf("%d.%s names %s, want the volume's unchanged name %s", vmid, key, name, volume)
			}
			if tags, _ := cfg["tags"].(string); !strings.Contains(tags, pve.ParkerTag) {
				t.Fatalf("%d.%s carries the serial on a VM that isn't a parker (tags %q)", vmid, key, tags)
			}
		}
	}
	if len(holders) != 1 {
		t.Fatalf("slots carrying serial %s: %v, want exactly one parker slot", serial, holders)
	}
	if client.state.volumes[volume] == nil {
		t.Fatalf("the parked volume %s does not exist", volume)
	}
	for key, value := range client.state.configs[source] {
		if isDiskOptionKey(key) && strings.Split(fmt.Sprint(value), ",")[0] == volume {
			t.Fatalf("the applied view of VM %d still names %s on %s", source, volume, key)
		}
	}
	if client.pending != nil {
		client.pending.mu.Lock()
		defer client.pending.mu.Unlock()
		for _, held := range []map[string]any{client.pending.deletes[source], client.pending.replaced[source]} {
			for key, value := range held {
				if strings.Split(fmt.Sprint(value), ",")[0] == volume {
					t.Fatalf("the pending view of VM %d still names %s on %s", source, volume, key)
				}
			}
		}
	}
}

// parkRenameCycle gives a managed disk that sits on VM 777 a parker-owned
// name. A journal-managed disk is born under the disk band's VMID, which
// neither 777 nor a parker owns, so one detach parks it by config edit and
// leaves its name alone. Attaching it back to 777 moves it and renames it for
// 777, which owns that name, and the next detach keeps an unused entry, so the
// move runs and renames it for the parker. Each step is checked against the
// fake's move counter, so a cycle that skipped a rename fails here and not
// somewhere downstream.
func parkRenameCycle(t *testing.T, ctx context.Context, deps Deps, client *lifecycleFlowPVE, cid string) {
	t.Helper()
	deps.Config.DetachedDiskStrategy = "parked"
	start := client.moves
	steps := []struct {
		name       string
		run        func() error
		movesSoFar int
	}{
		{"detach by config edit", func() error { return detachDiskAt(t, ctx, deps, "777", cid) }, 0},
		{"attach back with a rename for 777", func() error { return attachDiskAt(t, ctx, deps, "777", cid) }, 1},
		{"detach with a rename for the parker", func() error { return detachDiskAt(t, ctx, deps, "777", cid) }, 2},
	}
	for _, step := range steps {
		if err := step.run(); err != nil {
			t.Fatalf("rename cycle, %s: %v", step.name, err)
		}
		if got := client.moves - start; got != step.movesSoFar {
			t.Fatalf("rename cycle, %s: %d moves so far, want %d", step.name, got, step.movesSoFar)
		}
	}
}

// parkerSlotsCarrying lists the disk slots of VM vmid that carry serial.
func parkerSlotsCarrying(client *lifecycleFlowPVE, vmid int, serial string) []string {
	var slots []string
	for key, value := range client.state.configs[vmid] {
		if text, _ := value.(string); isDiskOptionKey(key) && strings.Contains(text, "serial="+serial) {
			slots = append(slots, key)
		}
	}
	return slots
}
