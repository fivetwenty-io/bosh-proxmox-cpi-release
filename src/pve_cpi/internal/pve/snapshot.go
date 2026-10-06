package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// HasSnapshots returns the names of real (non-synthetic) snapshots for the given VM.
//
// PVE always includes a synthetic entry named "current" in the snapshot list;
// this entry represents the live state of the VM, not an actual snapshot. This
// function filters it out along with any entries whose name is empty or whose
// "name" field is not a string. Only entries that survive the filter are returned.
//
// Return values:
//   - (nil, nil)     — no real snapshots exist; disk ops are safe to proceed.
//   - (names, nil)   — one or more real snapshots exist; names are in response
//     order. Callers in attach_disk, detach_disk, and resize_disk use this to
//     gate mutating PVE calls when snapshot integrity must be preserved.
//   - (nil, err)     — the ListSnapshots call failed; the error wraps the
//     original with context identifying the VM and node.
//
// Usage pattern in disk-op guards:
//
//	names, err := pve.HasSnapshots(ctx, deps.PVE, node, vmid)
//	if err != nil {
//	    // handle per RequireSnapshotCheckPass policy
//	}
//	if len(names) > 0 {
//	    // fail or warn per AllowDiskOpsWithSnapshots policy
//	}
func HasSnapshots(ctx context.Context, client Client, node string, vmid int) ([]string, error) {
	entries, err := client.QEMU().ListSnapshots(ctx, node, vmid)
	if err != nil {
		return nil, fmt.Errorf("HasSnapshots: list snapshots for vm %d on node %s: %w", vmid, node, err)
	}

	var names []string
	for _, m := range entries {
		name, ok := ConfigString(m, "name")
		if !ok || name == "" || name == "current" {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}

// SnapshotConfig reads the configuration PVE keeps for one snapshot of a VM.
// Its drive keys and its vmstate key name the volumes the snapshot holds,
// which the VM's current configuration does not list.
//
// A read that returns nothing, or anything other than a JSON object, is an
// error rather than an empty snapshot, because a caller uses the result to
// prove what the snapshot holds. A failed read wraps the SDK error with %w,
// so DescribeAuditError can still classify it.
func SnapshotConfig(ctx context.Context, client Client, node string, vmid int, name string) (map[string]any, error) {
	response, err := client.Nodes().ListQemuSnapshotConfig(ctx, node, strconv.Itoa(vmid), name)
	if err != nil {
		return nil, fmt.Errorf("SnapshotConfig: read snapshot %q of vm %d on node %s: %w", name, vmid, node, err)
	}
	if response == nil || len(*response) == 0 {
		return nil, fmt.Errorf("SnapshotConfig: snapshot %q of vm %d on node %s returned no configuration", name, vmid, node)
	}
	var cfg map[string]any
	if err := json.Unmarshal(*response, &cfg); err != nil || cfg == nil {
		return nil, fmt.Errorf("SnapshotConfig: snapshot %q of vm %d on node %s returned a malformed configuration", name, vmid, node)
	}
	return cfg, nil
}

// WaitForSnapshotAbsent polls until snapName no longer appears in the VM's
// snapshot list, or the configured timeout / ctx deadline elapses.
//
// PVE removes a snapshot via an asynchronous worker task, but the SDK's
// DeleteSnapshot issues the DELETE and discards the task UPID, so callers
// cannot AwaitTask the removal directly. Without waiting, delete_snapshot
// returns while PVE is still deleting the snapshot, and an immediately
// following operation whose PVE-side guard rejects live snapshots — notably
// detach_disk — fails spuriously. This poll bridges that gap.
//
// Options reuse AwaitTask's defaults (2 s interval, 5 min max wait); the max
// wait may be overridden per call with WithMaxWait.
//
// Returns nil once snapName is gone. Returns a *cpierrors.Error on timeout, on
// ctx cancellation, or when the snapshot list cannot be read.
func WaitForSnapshotAbsent(
	ctx context.Context, client Client, node string, vmid int, snapName string, opts ...AwaitOption,
) error {
	if ctx == nil {
		return cpierrors.Cloud("WaitForSnapshotAbsent: ctx must not be nil")
	}
	if client == nil {
		return cpierrors.Cloud("WaitForSnapshotAbsent: client must not be nil")
	}

	ao := &awaitOptions{
		pollIntervalMs: defaultPollIntervalMs,
		maxWaitSeconds: defaultMaxWaitSeconds,
	}
	for _, opt := range opts {
		opt(ao)
	}

	interval := time.Duration(ao.pollIntervalMs) * time.Millisecond
	deadline := time.Now().Add(time.Duration(ao.maxWaitSeconds) * time.Second)

	for {
		var names []string
		retryErr := RetryOnTransient(ctx, nil, "wait_snapshot_absent_poll", 0, func() error {
			var inner error
			names, inner = HasSnapshots(ctx, client, node, vmid)
			return inner
		})
		if retryErr != nil {
			// WrapConfigReadError: this is a config-scan read, and its
			// transient shapes (a cycling pveproxy, a bare 5xx) must stay
			// retriable through the terminal wrap.
			return cpierrors.Wrap(WrapConfigReadError(retryErr),
				fmt.Sprintf("WaitForSnapshotAbsent: vm %d on node %s", vmid, node))
		}
		present := false
		for _, n := range names {
			if n == snapName {
				present = true
				break
			}
		}
		if !present {
			return nil
		}
		if time.Now().After(deadline) {
			// Still present after the wait budget means the asynchronous
			// removal is still in progress server-side, not that it failed:
			// retriable, so the Director re-enters and finds it gone rather
			// than treating a slow storage backend as a permanent failure.
			msg := fmt.Sprintf(
				"WaitForSnapshotAbsent: snapshot %q on vm %d still present after %ds (removal still in progress)",
				snapName, vmid, ao.maxWaitSeconds)
			return cpierrors.WrapAs(errors.New(msg), cpierrors.TypeRetriableCloud, msg)
		}
		select {
		case <-ctx.Done():
			return cpierrors.Wrap(ctx.Err(),
				fmt.Sprintf("WaitForSnapshotAbsent: snapshot %q on vm %d", snapName, vmid))
		case <-time.After(interval):
		}
	}
}

// refuseSnapshotNamingVolume refuses a config-edit park of volid while a
// snapshot of the source VM still names it. PVE's move_disk refuses an owned
// volume in that case, because is_volume_in_use reads every snapshot section
// as well as the current config, but a config-edit attach has no such check.
// Without this one, the parker would take a volume that a rollback of the
// snapshot puts back on the source as a second reference.
//
// The refusal wraps ErrMoveDiskSnapshotRefused, so IsMoveDiskSnapshotRefusal
// reports it and every caller treats it as the deferred park it already knows
// for an owned volume. Snapshots that don't name the volume don't block the
// park. A source that's gone has no snapshots. Any other read error refuses
// too, as a plain retriable error rather than a snapshot refusal, whatever
// require_snapshot_check_pass says, because going ahead without an answer
// risks a second reference.
func refuseSnapshotNamingVolume(ctx context.Context, c Client, node string, vmid int, volid string) error {
	names, err := HasSnapshots(ctx, c, node, vmid)
	if err != nil {
		if parkerConfigGone(err) {
			return nil
		}
		return cpierrors.Retriable("transfer in: could not list the snapshots of source vm %d before parking %q, so nothing was attached: %s",
			vmid, volid, err.Error())
	}
	for _, name := range names {
		cfg, cfgErr := SnapshotConfig(ctx, c, node, vmid, name)
		if cfgErr != nil {
			return cpierrors.Retriable("transfer in: could not read snapshot %q of source vm %d before parking %q, so nothing was attached: %s",
				name, vmid, volid, cfgErr.Error())
		}
		for key, value := range cfg {
			if !isQemuDiskKey(key) && key != "vmstate" {
				continue
			}
			text, ok := ConfigStringValue(value)
			if ok && bareDriveVolid(text) == volid {
				return &snapshotNamesVolumeRefusal{
					snapshot: name,
					err: fmt.Errorf("transfer in: snapshot %q of source vm %d names %q on %s, so the park waits until that snapshot is deleted: %w",
						name, vmid, volid, key, ErrMoveDiskSnapshotRefused),
				}
			}
		}
	}
	return nil
}

// snapshotNamesVolumeRefusal is refuseSnapshotNamingVolume's refusal. Its text
// and its ErrMoveDiskSnapshotRefused cause are the refusal's own, so every
// caller that reads a snapshot refusal reads it unchanged, and it also says
// that the CPI declined the park before it sent anything to PVE.
type snapshotNamesVolumeRefusal struct {
	snapshot string
	err      error
}

func (e *snapshotNamesVolumeRefusal) Error() string { return e.err.Error() }

func (e *snapshotNamesVolumeRefusal) Unwrap() error { return e.err }

// SnapshotNamingVolumeRefusal reports whether err is the CPI's own refusal to
// park a volume that a snapshot of the source VM still names, and returns
// that snapshot's name. The CPI makes the check before it sends a move or a
// config edit, so the refusal is the CPI's and not PVE's. PVE's refusal of a
// move doesn't count.
func SnapshotNamingVolumeRefusal(err error) (string, bool) {
	var refusal *snapshotNamesVolumeRefusal
	if errors.As(err, &refusal) {
		return refusal.snapshot, true
	}
	return "", false
}
