package pve

import (
	"context"
	"fmt"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
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
		name, ok := m["name"].(string)
		if !ok || name == "" || name == "current" {
			continue
		}
		names = append(names, name)
	}
	return names, nil
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
// Options reuse AwaitTask's defaults (2 s interval, 5 min max wait) and may be
// overridden with WithPollInterval / WithMaxWait.
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
		names, err := HasSnapshots(ctx, client, node, vmid)
		if err != nil {
			return cpierrors.Wrap(err,
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
			return cpierrors.Cloud(
				"WaitForSnapshotAbsent: snapshot %q on vm %d still present after %ds",
				snapName, vmid, ao.maxWaitSeconds)
		}
		select {
		case <-ctx.Done():
			return cpierrors.Wrap(ctx.Err(),
				fmt.Sprintf("WaitForSnapshotAbsent: snapshot %q on vm %d", snapName, vmid))
		case <-time.After(interval):
		}
	}
}
