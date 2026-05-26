package pve

import (
	"context"
	"fmt"
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
