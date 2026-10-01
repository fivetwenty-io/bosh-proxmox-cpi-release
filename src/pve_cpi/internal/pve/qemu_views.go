package pve

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// QemuViews is one VM's configuration in both of PVE's views, read in one call
// from GET /nodes/{node}/qemu/{vmid}/pending.
//
// The config endpoint, which the SDK's Config reads with no current flag,
// returns the configuration with pending changes applied. A drive delete that
// PVE could only record as pending is therefore invisible there, even though
// the running guest still has the disk plugged in. That happens when the VM's
// hotplug setting lacks disk, and when the guest still holds the device and
// the unplug fails after PVE has written the pending delete. Every decision
// about whether a VM still holds a volume reads both views through this type,
// so a pending delete never counts as a release.
//
// The pending endpoint (vm_pending in qemu-server, which returns
// config_with_pending_array) lists every key of the loaded config, digest
// included, with its current value, any pending value, and a delete flag of 1
// or 2. Keys that exist only as a pending value or only as a pending delete come
// as items of their own. Both views come from that one load_config, so they
// always agree with each other.
type QemuViews struct {
	entries map[string]qemuPendingEntry
}

// qemuPendingEntry is one key of the pending endpoint: its current value, its
// pending value, and whether a delete of the key is pending. A pending change
// is only ever what an item says explicitly, never a key the response leaves
// out.
type qemuPendingEntry struct {
	value      string
	hasValue   bool
	pending    string
	hasPending bool
	delete     bool
}

// ReadQemuViews reads a VM's configuration in both views. A 404 comes back
// unchanged, so callers keep their existing not-found handling.
func ReadQemuViews(ctx context.Context, c Client, node string, vmid int) (QemuViews, error) {
	if c == nil || c.Nodes() == nil {
		return QemuViews{}, cpierrors.Cloud("ReadQemuViews: nodes service not available")
	}
	resp, err := c.Nodes().ListQemuPending(ctx, node, strconv.Itoa(vmid))
	if err != nil {
		return QemuViews{}, err
	}
	views := QemuViews{entries: map[string]qemuPendingEntry{}}
	if resp == nil {
		return views, nil
	}
	for _, raw := range *resp {
		var row struct {
			Key     string          `json:"key"`
			Value   json.RawMessage `json:"value"`
			Pending json.RawMessage `json:"pending"`
			Delete  json.RawMessage `json:"delete"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return QemuViews{}, cpierrors.Cloud("ReadQemuViews: VM %d on node %s: malformed pending entry: %s", vmid, node, err.Error())
		}
		if row.Key == "" {
			continue
		}
		entry := qemuPendingEntry{}
		entry.value, entry.hasValue = pendingScalar(row.Value)
		entry.pending, entry.hasPending = pendingScalar(row.Pending)
		if flag, ok := pendingScalar(row.Delete); ok && flag != "0" && flag != "" {
			entry.delete = true
		}
		views.entries[row.Key] = entry
	}
	return views, nil
}

// pendingScalar renders one JSON scalar the way the config map's values read,
// and reports false for an absent or null field.
func pendingScalar(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	text, ok := ConfigStringValue(value)
	if !ok {
		return fmt.Sprint(value), true
	}
	return text, true
}

// Applied returns the configuration with pending changes applied, which is the
// view the config endpoint returns. Each key takes its current value, a pending
// value replaces it, and a key with a pending delete is left out.
func (v QemuViews) Applied() map[string]any {
	out := make(map[string]any, len(v.entries))
	for key, entry := range v.entries {
		switch {
		case entry.delete:
		case entry.hasPending:
			out[key] = entry.pending
		case entry.hasValue:
			out[key] = entry.value
		}
	}
	return out
}

// Current returns the configuration the running VM has now, before any pending
// change is applied.
func (v QemuViews) Current() map[string]any {
	out := make(map[string]any, len(v.entries))
	for key, entry := range v.entries {
		if entry.hasValue {
			out[key] = entry.value
		}
	}
	return out
}

// Holding returns every key either view has, which is what a decision about
// what a VM's destroy takes has to read. PVE's destroy_vm frees every owned
// drive the current config names and then every owned drive the pending
// section names, with the same callback (qemu-server QemuServer.pm:1895 and
// :1904 at a7b4240b). So Holding starts from the current view, where a slot
// whose delete is pending is still there and a key with a pending value keeps
// its current value, and then adds each key that exists only as a pending
// value.
func (v QemuViews) Holding() map[string]any {
	out := v.Current()
	for key, entry := range v.entries {
		if _, present := out[key]; !present && entry.hasPending && !entry.delete {
			out[key] = entry.pending
		}
	}
	return out
}

// PendingChanges returns, sorted, the keys that carry a pending value and the
// keys whose delete is pending.
func (v QemuViews) PendingChanges() (values, deletes []string) {
	for key, entry := range v.entries {
		if entry.hasPending {
			values = append(values, key)
		}
		if entry.delete {
			deletes = append(deletes, key)
		}
	}
	sort.Strings(values)
	sort.Strings(deletes)
	return values, deletes
}

// QemuHolding is what a VM's destroy can take, read from both views.
type QemuHolding struct {
	// Config is every key either view has, as Holding returns it.
	Config map[string]any
	// Replaced holds each disk key whose pending value names a different
	// volume than its current value, with that pending value. Config keeps
	// the current value for such a key, and destroy_vm frees an owned volume
	// that either value names, so a decision about what the destroy takes
	// checks both.
	Replaced map[string]any
}

// Volumes returns, sorted, every volume key names in either view.
func (h QemuHolding) Volumes(key string) []string {
	var volumes []string
	for _, cfg := range []map[string]any{h.Config, h.Replaced} {
		value, ok := ConfigString(cfg, key)
		if !ok {
			continue
		}
		if bare := bareDriveVolid(value); bare != "" && !slices.Contains(volumes, bare) {
			volumes = append(volumes, bare)
		}
	}
	sort.Strings(volumes)
	return volumes
}

// WithReplacements returns Config with each replaced key's pending value in
// place of its current one, which is the config the pending values would make.
// A check that judges what the destroy takes runs over both Config and this.
func (h QemuHolding) WithReplacements() map[string]any {
	out := make(map[string]any, len(h.Config))
	for key, value := range h.Config {
		out[key] = value
	}
	for key, value := range h.Replaced {
		out[key] = value
	}
	return out
}

// PendingReplacements returns each disk key whose pending value names a
// different volume than its current value, with that pending value. A CPI
// attach can create the shape when it writes a volume onto a slot whose delete
// is pending on a running VM, because PVE then keeps the old drive current and
// records the new one as pending.
func (v QemuViews) PendingReplacements() map[string]any {
	out := map[string]any{}
	for key, entry := range v.entries {
		if !isQemuDiskKey(key) || !entry.hasValue || !entry.hasPending {
			continue
		}
		if bareDriveVolid(entry.pending) != bareDriveVolid(entry.value) {
			out[key] = entry.pending
		}
	}
	return out
}

// ReadQemuHolding reads what a VM's destroy can take from both views, as
// QemuHolding describes. Its errors are ReadQemuViews' errors, so a 404 or
// pmxcfs's config-missing 500 reads the way a config read's does.
func ReadQemuHolding(ctx context.Context, c Client, node string, vmid int) (QemuHolding, error) {
	views, err := ReadQemuViews(ctx, c, node, vmid)
	if err != nil {
		return QemuHolding{}, err
	}
	return QemuHolding{Config: views.Holding(), Replaced: views.PendingReplacements()}, nil
}

// PendingDelete reports whether a delete of key is pending.
func (v QemuViews) PendingDelete(key string) bool {
	return v.entries[key].delete
}

// SlotsNaming returns, sorted, every disk key (a bus slot or an unused entry)
// whose current or pending value names bareVolid. A key with a pending delete
// still names the volume, because the running guest still has it.
func (v QemuViews) SlotsNaming(bareVolid string) []string {
	var keys []string
	for key, entry := range v.entries {
		if !isQemuDiskKey(key) {
			continue
		}
		if entry.hasValue && bareDriveVolid(entry.value) == bareVolid ||
			entry.hasPending && bareDriveVolid(entry.pending) == bareVolid {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// NamesVolume reports whether either view names bareVolid on any disk key.
func (v QemuViews) NamesVolume(bareVolid string) bool {
	return len(v.SlotsNaming(bareVolid)) > 0
}

// BusSlotNaming returns a bus slot that names bareVolid in either view,
// counting a slot whose delete is pending as attached.
func (v QemuViews) BusSlotNaming(bareVolid string) (string, bool) {
	for _, key := range v.SlotsNaming(bareVolid) {
		if !strings.HasPrefix(key, "unused") {
			return key, true
		}
	}
	return "", false
}

// ResolveAttachedSlot is ResolveDiskID read from both views. It returns the bus
// slot that names volid on the VM, counting a slot whose delete is pending as
// attached, and wraps ErrDiskNotAttached when no bus slot names it in either
// view.
func ResolveAttachedSlot(ctx context.Context, c Client, node string, vmid int, volid string) (string, error) {
	if node == "" || vmid <= 0 || volid == "" {
		return "", cpierrors.Cloud("ResolveAttachedSlot: node, a positive vmid, and volid are all required")
	}
	views, err := ReadQemuViews(ctx, c, node, vmid)
	if err != nil {
		return "", fmt.Errorf("ResolveAttachedSlot: config fetch failed for VM %d on node %q: %w", vmid, node, err)
	}
	if slot, ok := views.BusSlotNaming(volid); ok {
		return slot, nil
	}
	return "", fmt.Errorf("resolve disk %q on VM %d (node %q): %w", volid, vmid, node, ErrDiskNotAttached)
}

// isQemuDiskKey reports whether key is a bus slot or an unused entry, which are
// the keys that can name a volume.
func isQemuDiskKey(key string) bool {
	for _, prefix := range []string{"scsi", "virtio", "sata", "ide", "efidisk", "tpmstate", "unused"} {
		if rest, ok := strings.CutPrefix(key, prefix); ok && rest != "" {
			if _, err := strconv.Atoi(rest); err == nil {
				return true
			}
		}
	}
	return false
}
