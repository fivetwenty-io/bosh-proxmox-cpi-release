// ZFS thick-provisioning visibility: a diagnostic-only, never-mutating check
// that surfaces when a zfspool storage pool is provisioning every zvol at its
// full requested size (PVE's default) rather than sparsely, so an operator
// relying on storage.max_utilization_pct headroom math is not surprised by a
// pool that looks thin but is already fully committed.
package handlers

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
)

// zfsThickProvisioningWarnedPools deduplicates the once-per-pool-per-process
// Info log emitted by warnIfZFSThickProvisioned: naming a zfspool storage's
// thick-provisioning posture is useful once per process lifetime — repeating
// it on every create_disk call against the same pool would flood the log
// without adding information. Keyed by storage ID.
//
// No existing keyed once-per-key ledger exists elsewhere in this package to
// reuse: the closest precedents (vniZoneListWarnOnce in internal/pve/vni.go,
// localISOStorageWarnOnce in internal/cpi/handlers/create_vm.go) are both
// single-shot sync.Once guards for one fixed condition, not per-key. This is
// a new, minimal instance of the same "log it once" idea, generalized to a
// key via sync.Map — safe for concurrent create_disk calls without a
// separate mutex.
var zfsThickProvisioningWarnedPools sync.Map

// warnIfZFSThickProvisioned is a diagnostic-only check: when storageName
// resolves to a zfspool storage whose "sparse" flag is unset or 0 (PVE
// returns integer booleans, not JSON booleans — see pve.IsTrimCapable's
// storage-type classification for the same convention elsewhere in this
// codebase), every zvol the CPI creates there reserves its full requested
// size up front (thick provisioning) rather than allocating on demand.
// Operators sizing storage.max_utilization_pct headroom off a pool's
// nominal capacity should account for that: a thin-looking pool with
// apparent headroom can already be fully committed by reservation alone.
//
// knownStorageType MUST already be resolved by the caller (e.g. create_disk's
// discard/ssd auto-resolution lookup) and is a strict gate, not merely an
// optimization: this function only ever calls ListStorage when
// knownStorageType == "zfspool" exactly. Any other value — including ""
// (unresolved, e.g. because the caller's own discard/ssd values were both
// explicit and so never triggered a live type lookup — see
// needsDiskPerfStorageTypeLookup) — is a silent no-op with zero API calls.
// This diagnostic never introduces a live lookup create_disk would not have
// made anyway; it only ever piggybacks on a type already known to be
// zfspool, at the cost of not firing for operators who avoid the type
// lookup entirely via explicit discard/ssd.
//
// Never mutates storage configuration and never fails or delays the calling
// disk operation:
//   - deps.PVE/ClusterStorage unavailable, storageName empty, or
//     knownStorageType != "zfspool" → silent no-op, no API call.
//   - a ListStorage error or malformed response → logged at Debug only, never
//     propagated — this is a diagnostic, not a gate.
//   - storageName not present in the storage list → silent.
//   - sparse=1 → silent (this is the well-provisioned case).
//   - fires the Info log at most once per cluster+storage ID for the lifetime of the
//     process (zfsThickProvisioningWarnedPools); a second, third, etc. call
//     for the same pool is a fast no-op (checked before any API call).
func warnIfZFSThickProvisioned(ctx context.Context, deps Deps, storageName, knownStorageType string) {
	if storageName == "" || knownStorageType != pve.StorageTypeZFSPool || deps.PVE == nil || deps.PVE.ClusterStorage() == nil {
		return
	}
	// Keyed by cluster identity + pool name, not pool name alone: with
	// per-request context overrides one process can serve several clusters,
	// and "local-zfs" on cluster A must not silence the warning for a
	// distinct "local-zfs" on cluster B.
	warnKey := clusterIdentity(deps.Config) + "\x00" + storageName
	if _, alreadyWarned := zfsThickProvisioningWarnedPools.Load(warnKey); alreadyWarned {
		return
	}

	resp, err := deps.PVE.ClusterStorage().ListStorage(ctx, &sdkclusterstorage.ListStorageParams{})
	if err != nil || resp == nil {
		deps.Log(ctx).Debug("create_disk: ListStorage failed; skipping ZFS thick-provisioning check (diagnostic only, disk operation unaffected)",
			log.String("storage", storageName),
			log.Err(err),
		)
		return
	}

	for _, raw := range *resp {
		var entry struct {
			Storage string `json:"storage"`
			Type    string `json:"type"`
			// Sparse is PVE's integer-boolean convention (1/0), not a JSON
			// bool — absent or 0 means thick provisioning, the zfspool
			// plugin's own default when the config carries no "sparse" line.
			Sparse *int `json:"sparse,omitempty"`
		}
		if jerr := json.Unmarshal(raw, &entry); jerr != nil {
			continue
		}
		if entry.Storage != storageName {
			continue
		}
		if entry.Type != pve.StorageTypeZFSPool {
			return
		}
		if entry.Sparse != nil && *entry.Sparse != 0 {
			return // sparse=1: silent, this is the well-provisioned case.
		}
		if _, loaded := zfsThickProvisioningWarnedPools.LoadOrStore(warnKey, struct{}{}); loaded {
			// Lost a race against a concurrent caller that warned first.
			return
		}
		deps.Log(ctx).Info("create_disk: zfspool storage provisions thick (sparse is unset or 0); "+
			"every disk reserves its full size up front, and storage.max_utilization_pct capacity math "+
			"assumes full reservation — a pool that looks thin by nominal capacity can already be fully "+
			"committed by reservation alone",
			log.String("storage", storageName),
		)
		return
	}
	// storageName absent from the index: nothing to classify. Silent —
	// the disk operation's own storage-resolution error handling (elsewhere)
	// is the correct place to surface an unknown-storage problem, not this
	// diagnostic.
}
