package handlers

import (
	"context"
	"encoding/json"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
)

// storageUtilBytesPerGiB is the GiB→bytes conversion used by the
// storage-utilization gate call sites (create_disk, resize_disk).
const storageUtilBytesPerGiB = int64(1024 * 1024 * 1024)

// storageUtilizationStatus fetches available/total bytes for storageName on
// node via GET /nodes/<node>/storage, applying the same active +
// images-capable entry selection used by the placement headroom filter and
// calculate_vm_cloud_properties (see storageStatusEntry). Missing or
// unreachable facts are reported via ok=false and a Warn is logged naming the
// reason; callers of this function fail open (skip the gate) in that case,
// consistent with the existing absolute-bytes headroom filter's fail-open
// behavior on a ListStorage error.
func storageUtilizationStatus(ctx context.Context, deps Deps, node, storageName, opName string) (avail, total int64, ok bool) {
	if deps.PVE == nil || deps.PVE.Nodes() == nil || node == "" || storageName == "" {
		return 0, 0, false
	}
	resp, err := deps.PVE.Nodes().ListStorage(ctx, node, &nodes.ListStorageParams{Storage: &storageName})
	if err != nil {
		deps.Log(ctx).Warn(opName+": ListStorage failed; storage-utilization gate fails open",
			log.String("node", node), log.String("storage", storageName), log.Err(err))
		return 0, 0, false
	}
	if resp == nil {
		return 0, 0, false
	}
	for _, raw := range *resp {
		var entry storageStatusEntry
		if uerr := json.Unmarshal(raw, &entry); uerr != nil {
			continue
		}
		if entry.Storage != storageName {
			continue
		}
		if entry.Active != 1 {
			deps.Log(ctx).Warn(opName+": storage pool inactive; storage-utilization gate fails open",
				log.String("node", node), log.String("storage", storageName))
			return 0, 0, false
		}
		if entry.Total <= 0 {
			deps.Log(ctx).Warn(opName+": storage pool reports zero total capacity; storage-utilization gate fails open",
				log.String("node", node), log.String("storage", storageName))
			return 0, 0, false
		}
		return entry.Avail, entry.Total, true
	}
	deps.Log(ctx).Warn(opName+": storage pool not reported by node; storage-utilization gate fails open",
		log.String("node", node), log.String("storage", storageName))
	return 0, 0, false
}

// projectedUtilizationPct computes the utilization percentage of a pool after
// adding addBytes (pass 0 for a point-in-time check, as snapshot_disk does).
// Negative intermediate values (which should not occur with well-formed PVE
// facts, but are defended against for API skew) are clamped to 0 rather than
// producing a nonsensical negative or wrapped percentage.
func projectedUtilizationPct(avail, total, addBytes int64) float64 {
	used := total - avail
	if used < 0 {
		used = 0
	}
	projected := used + addBytes
	if projected < 0 {
		projected = 0
	}
	return float64(projected) / float64(total) * 100
}

// checkMaxUtilizationGate enforces pve.storage.max_utilization_pct for a
// single node/pool/opName evaluation point: create_disk (addBytes = the new
// disk's size) and resize_disk (addBytes = the resize delta). addBytes must
// already reflect only the bytes being ADDED to the pool by this operation,
// not the operation's total size.
//
// Disabled (MaxUtilizationPctValue() <= 0, the default) is a no-op returning
// nil — zero behavior change. When enabled and the projected utilization
// exceeds the ceiling:
//   - enforce mode (default) returns a RETRIABLE CloudError naming the pool,
//     node, current projected percentage, and the ceiling — capacity can be
//     freed, so the director should re-drive rather than treat this as a
//     permanent failure.
//   - warn mode logs the same facts at Warn and returns nil (operation
//     proceeds).
//
// Fails open (returns nil, no error, no Warn beyond what
// storageUtilizationStatus already logs) when storage facts cannot be
// determined.
func checkMaxUtilizationGate(ctx context.Context, deps Deps, node, storageName string, addBytes int64, opName string) error {
	if deps.Config == nil {
		return nil
	}
	ceiling := deps.Config.MaxUtilizationPctValue()
	if ceiling <= 0 {
		return nil
	}
	avail, total, ok := storageUtilizationStatus(ctx, deps, node, storageName, opName)
	if !ok {
		return nil
	}
	pct := projectedUtilizationPct(avail, total, addBytes)
	if pct <= float64(ceiling) {
		return nil
	}
	if deps.Config.MaxUtilizationEnforce() {
		return cpierrors.Retriable(
			"%s: storage pool %q on node %q projected utilization %.1f%% would exceed the configured "+
				"ceiling %d%% (capacity can be freed; re-drive after freeing space)",
			opName, storageName, node, pct, ceiling,
		)
	}
	deps.Log(ctx).Warn(opName+": storage pool projected utilization exceeds ceiling (warn mode; proceeding)",
		log.String("node", node),
		log.String("storage", storageName),
		log.Float64("projected_pct", pct),
		log.Int("ceiling_pct", ceiling),
	)
	return nil
}

// warnIfStorageAboveCeilingOp is the fixed opName warnIfStorageAboveCeiling
// uses for logging and the ListStorage fail-open Warn — the function has a
// single caller (snapshot_disk) by design (see its doc comment), so opName is
// not a parameter.
const warnIfStorageAboveCeilingOp = "snapshot_disk"

// warnIfStorageAboveCeiling implements the snapshot_disk evaluation point,
// which is Warn-only regardless of storage.max_utilization_mode: snapshot
// growth is unbounded and cannot be estimated ahead of time, so this checks
// only whether the pool is ALREADY above the ceiling at snapshot time
// (addBytes=0) and, if so, logs a Warn. It never returns an error, even when
// max_utilization_mode is "enforce". Disabled (ceiling <= 0) and
// missing-facts are both no-ops (fail open), matching checkMaxUtilizationGate.
func warnIfStorageAboveCeiling(ctx context.Context, deps Deps, node, storageName string) {
	if deps.Config == nil {
		return
	}
	ceiling := deps.Config.MaxUtilizationPctValue()
	if ceiling <= 0 {
		return
	}
	avail, total, ok := storageUtilizationStatus(ctx, deps, node, storageName, warnIfStorageAboveCeilingOp)
	if !ok {
		return
	}
	pct := projectedUtilizationPct(avail, total, 0)
	if pct <= float64(ceiling) {
		return
	}
	deps.Log(ctx).Warn(
		warnIfStorageAboveCeilingOp+": storage pool already above the utilization ceiling; snapshot growth is unbounded and "+
			"cannot be gated ahead of time (informational only, snapshot proceeds)",
		log.String("node", node),
		log.String("storage", storageName),
		log.Float64("current_pct", pct),
		log.Int("ceiling_pct", ceiling),
	)
}
