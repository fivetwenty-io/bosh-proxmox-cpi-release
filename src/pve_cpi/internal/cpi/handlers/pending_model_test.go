package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// fakePendingModel is the pending section of PVE's VM configs, for the fakes
// whose rows need a slot delete that stays pending. A fake that holds one
// keeps its config map as the applied view, the one the config endpoint
// returns, and the model keeps each held key's current value beside it.
//
// It follows qemu-server for a drive delete on a running VM. When the VM's
// hotplug setting lacks disk, the PUT succeeds and the delete stays pending
// (vmconfig_hotplug_pending dies with "skip" and the error is dropped). When
// hotplug includes disk but the guest holds the device, the pending delete is
// written and the PUT fails busy. Otherwise the delete applies at once, and so
// does every delete on a stopped VM and every unused-entry delete. A revert
// drops the pending delete, which puts the key back in the applied view.
type fakePendingModel struct {
	mu sync.Mutex
	// running marks the VMs that are running.
	running map[int]bool
	// busy marks the VMs whose guest still holds every disk device.
	busy map[int]bool
	// deletes holds, per VM, each key whose delete is pending and its
	// current value.
	deletes map[int]map[string]any
	// replaced holds, per VM, each key whose config value is a pending value
	// that replaced a different current drive, with that current value.
	replaced map[int]map[string]any
	// revertErr, when set, fails every revert.
	revertErr error
	// revertKeepsPending makes every revert succeed without dropping the
	// pending delete.
	revertKeepsPending bool
	// refuseDelete, when set, fails every bus-slot delete on a running VM
	// before PVE writes anything, so no pending delete is left behind.
	refuseDelete error
	// afterHold, when set, runs on the VM's config each time the model holds
	// a delete, so a row can make the readback show another change.
	afterHold func(cfg map[string]any)
	// stopRequests records the VMs a stop was issued for while they ran, and
	// restartAfterStop marks the VMs that something such as HA starts again
	// right after their stop completes.
	stopRequests     map[int]bool
	restartAfterStop map[int]bool
	// deleteCalls and reverts record each bus-slot delete and each revert, as
	// "<vmid>:<key>".
	deleteCalls []string
	reverts     []string
}

func newFakePendingModel() *fakePendingModel {
	return &fakePendingModel{running: map[int]bool{}, busy: map[int]bool{}, deletes: map[int]map[string]any{}}
}

// run marks a VM running, and stop marks it stopped.
func (m *fakePendingModel) run(vmid int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.running[vmid] = true
}

// issueStop records a stop issued through the API. A stop of a VM that isn't
// running does nothing, the way vm_stop returns early without a pid, so its
// pending changes stay.
func (m *fakePendingModel) issueStop(vmid int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.running[vmid] {
		return
	}
	if m.stopRequests == nil {
		m.stopRequests = map[int]bool{}
	}
	m.stopRequests[vmid] = true
}

// completeStops completes every issued stop the way vm_stop_cleanup does, which
// applies the VM's pending changes (QemuServer.pm:6250 and :6281). configs is
// the fake's applied view per VM.
func (m *fakePendingModel) completeStops(configs map[int]map[string]any) {
	m.mu.Lock()
	vmids := make([]int, 0, len(m.stopRequests))
	for vmid := range m.stopRequests {
		vmids = append(vmids, vmid)
	}
	m.stopRequests = nil
	m.mu.Unlock()
	for _, vmid := range vmids {
		m.completeStop(vmid, configs[vmid])
	}
}

// completeStop applies a VM's held deletes and replacements to cfg, its applied
// view, the way vmconfig_apply_pending does when a stop completes, and marks
// the VM stopped. A drive the change drops stays on an unusedN entry when the
// VM owns its volume, and otherwise it's dropped. A VM marked in
// restartAfterStop runs again at once.
func (m *fakePendingModel) completeStop(vmid int, cfg map[string]any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, held := range []map[string]any{m.deletes[vmid], m.replaced[vmid]} {
		for _, value := range held {
			text, _ := value.(string)
			volume := strings.Split(text, ",")[0]
			if owner, ok := pve.EmbeddedDiskVMID(volume); ok && owner == vmid && cfg != nil {
				for i := 0; ; i++ {
					key := fmt.Sprintf("unused%d", i)
					if _, taken := cfg[key]; !taken {
						cfg[key] = volume
						break
					}
				}
			}
		}
	}
	delete(m.deletes, vmid)
	delete(m.replaced, vmid)
	delete(m.running, vmid)
	delete(m.busy, vmid)
	if m.restartAfterStop[vmid] {
		m.running[vmid] = true
	}
}

// stop marks a VM stopped without applying its pending changes, the way a
// crash, a kill, or a node failure leaves it.
func (m *fakePendingModel) stop(vmid int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.running, vmid)
	delete(m.busy, vmid)
}

// holdDelete leaves a pending delete of key on a VM, the way a crash, an
// earlier release, or an operator's delete on a running VM leaves one.
func (m *fakePendingModel) holdDelete(vmid int, cfg map[string]any, key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.holdLocked(vmid, cfg, key)
}

func (m *fakePendingModel) holdLocked(vmid int, cfg map[string]any, key string) {
	value, present := cfg[key]
	if !present {
		return
	}
	if m.deletes[vmid] == nil {
		m.deletes[vmid] = map[string]any{}
	}
	m.deletes[vmid][key] = value
	delete(cfg, key)
}

// dropHeld forgets every pending delete of a VM, the way its destroy does, and
// returns the current values the deletes held.
func (m *fakePendingModel) dropHeld(vmid int) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var values []string
	for _, held := range []map[string]any{m.deletes[vmid], m.replaced[vmid]} {
		for _, value := range held {
			text, _ := value.(string)
			values = append(values, text)
		}
	}
	delete(m.deletes, vmid)
	delete(m.replaced, vmid)
	return values
}

// holdReplacement records a pending value for key that replaces its current
// drive, the way PVE does when a volume is written onto a slot whose old drive
// a running guest still holds. The fake's config, the applied view, takes the
// pending value, and the model keeps the current one.
func (m *fakePendingModel) holdReplacement(vmid int, cfg map[string]any, key string, pending any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.replaced == nil {
		m.replaced = map[int]map[string]any{}
	}
	if m.replaced[vmid] == nil {
		m.replaced[vmid] = map[string]any{}
	}
	m.replaced[vmid][key] = cfg[key]
	cfg[key] = pending
}

// pendingDelete reports whether a delete of key is pending on a VM.
func (m *fakePendingModel) pendingDelete(vmid int, key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, held := m.deletes[vmid][key]
	return held
}

// update applies the parts of a config PUT that the pending section decides,
// to cfg, the applied view of VM vmid. It reports handled when the PUT is a
// revert or a delete the model held pending, and the fake then returns err
// without applying anything else. Otherwise the fake applies the PUT as it
// always has.
func (m *fakePendingModel) update(vmid int, cfg map[string]any, params *sdknodes.UpdateQemuConfigParams) (handled bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if params.Revert != nil {
		for _, key := range strings.Split(*params.Revert, ",") {
			m.reverts = append(m.reverts, fmt.Sprintf("%d:%s", vmid, key))
		}
		if m.revertErr != nil {
			return true, m.revertErr
		}
		if m.revertKeepsPending {
			return true, nil
		}
		for _, key := range strings.Split(*params.Revert, ",") {
			if value, held := m.deletes[vmid][key]; held {
				cfg[key] = value
				delete(m.deletes[vmid], key)
			}
		}
		return true, nil
	}
	if params.Delete == nil {
		return false, nil
	}
	slot := *params.Delete
	if strings.HasPrefix(slot, "unused") {
		return false, nil
	}
	m.deleteCalls = append(m.deleteCalls, fmt.Sprintf("%d:%s", vmid, slot))
	if value, held := m.deletes[vmid][slot]; held && !m.running[vmid] {
		// A stopped VM applies its pending delete with the next write, so the
		// fake puts the key back and applies the delete the ordinary way.
		cfg[slot] = value
		delete(m.deletes[vmid], slot)
		return false, nil
	}
	if !m.running[vmid] {
		return false, nil
	}
	if m.refuseDelete != nil {
		return true, m.refuseDelete
	}
	lacksDisk := fakeHotplugLacksDisk(cfg)
	if !lacksDisk && !m.busy[vmid] {
		return false, nil
	}
	m.holdLocked(vmid, cfg, slot)
	if m.afterHold != nil {
		m.afterHold(cfg)
	}
	if lacksDisk {
		return true, nil
	}
	return true, fakeUnplugBusyError(slot)
}

// response is the pending endpoint's answer for a VM whose applied view is
// cfg. Every applied key comes with its current value, a replaced key comes
// with its current value and its pending one, and each held delete comes with
// its current value and a delete flag.
func (m *fakePendingModel) response(vmid int, cfg map[string]any) *sdknodes.ListQemuPendingResponse {
	m.mu.Lock()
	held := make(map[string]any, len(m.deletes[vmid]))
	for key, value := range m.deletes[vmid] {
		held[key] = value
	}
	replaced := make(map[string]any, len(m.replaced[vmid]))
	for key, value := range m.replaced[vmid] {
		replaced[key] = value
	}
	m.mu.Unlock()
	keys := make([]string, 0, len(cfg))
	for key := range cfg {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make(sdknodes.ListQemuPendingResponse, 0, len(keys)+len(held))
	add := func(item map[string]any) {
		raw, err := json.Marshal(item)
		if err != nil {
			panic(fmt.Sprintf("fakePendingModel: item %v: %v", item, err))
		}
		out = append(out, raw)
	}
	for _, key := range keys {
		if current, ok := replaced[key]; ok {
			add(map[string]any{"key": key, "value": current, "pending": cfg[key]})
			continue
		}
		add(map[string]any{"key": key, "value": cfg[key]})
	}
	heldKeys := make([]string, 0, len(held))
	for key := range held {
		heldKeys = append(heldKeys, key)
	}
	sort.Strings(heldKeys)
	for _, key := range heldKeys {
		add(map[string]any{"key": key, "value": held[key], "delete": 1})
	}
	return &out
}

// pendingRead serves the pending endpoint from a fake's own config read, so an
// injected config failure fails the pending read the same way, and adds the
// model's held keys.
func (m *fakePendingModel) pendingRead(
	ctx context.Context,
	read func(ctx context.Context, node string, vmid int) (map[string]any, error),
	node, vmid string,
) (*sdknodes.ListQemuPendingResponse, error) {
	id, err := strconv.Atoi(vmid)
	if err != nil {
		return nil, fmt.Errorf("fake pending endpoint: vmid %q: %w", vmid, err)
	}
	cfg, err := read(ctx, node, id)
	if err != nil {
		return nil, err
	}
	return m.response(id, cfg), nil
}

// fakeHotplugLacksDisk reads a hotplug setting the way parse_hotplug_features
// does. An absent key or "1" means the default, which includes disk.
func fakeHotplugLacksDisk(cfg map[string]any) bool {
	value, present := cfg["hotplug"]
	if !present {
		return false
	}
	text := strings.TrimSpace(fmt.Sprint(value))
	switch text {
	case "1":
		return false
	case "0", "":
		return true
	}
	for _, feature := range strings.Split(text, ",") {
		if strings.TrimSpace(feature) == "disk" {
			return false
		}
	}
	return true
}

// fakeUnplugBusyError is the error PVE returns when the guest still holds the
// device it was asked to unplug.
func fakeUnplugBusyError(slot string) error {
	return errors.New("API request failed: parameter error: Parameter verification failed. (code: 0, errors: " + slot +
		": hotplug problem - error on hot-unplugging device 'virtio" + slot + "' - still busy in guest?)")
}

// configPendingNodes serves a nodes fake's pending endpoint from its client's
// own config read, so the pending read fails where the config read fails, and
// hands every other call to the fake it wraps. The rollback and delete_vm
// fakes use it, because delete_vm's destroy decisions read both views.
type configPendingNodes struct {
	sdknodes.Service
	config func(ctx context.Context, node string, vmid int) (map[string]any, error)
}

func (n *configPendingNodes) ListQemuPending(ctx context.Context, node, vmid string) (*sdknodes.ListQemuPendingResponse, error) {
	return PendingFromConfigRead(ctx, n.config, node, vmid)
}
