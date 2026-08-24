package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

// tagDeletingVM is the PVE tag stamped on a VM before an async fast-path destroy.
// It is a diagnostic marker: any operator or script can list VMs tagged
// bosh-deleting to find those whose fast-path destroy was issued but has not
// yet completed. The sweepFastDeleteStragglers function consumes this tag on
// subsequent fast-path deletes to re-issue destroy for any straggler VM, making
// the tag part of a self-draining work queue.
const tagDeletingVM = "bosh-deleting"

// stampDeletingTag reads the VM's current tags, merges in tagDeletingVM, and
// writes back via UpdateQemuConfig. Failures are logged but never propagated:
// the caller treats tagging as best-effort. Existing operator tags are
// preserved via mergeTagList.
//
// The tag read-modify-write runs under a per-VMID cluster lock so a concurrent
// set_vm_metadata or set_disk_metadata call cannot interleave and overwrite the
// bosh-deleting marker. Lock acquisition failure is best-effort: logged, then
// the RMW proceeds unlocked so the marker is never silently dropped.
func stampDeletingTag(ctx context.Context, deps Deps, node, vmCID string, vmid int, logger *log.Logger) {
	// stampRMW is the actual read-modify-write body, called under the lock when
	// available and directly (unlocked) when the pool service is absent.
	stampRMW := func() {
		// Best-effort read of existing tags. On failure existing is nil and
		// mergeTagList still adds tagDeletingVM as the sole entry.
		var existing []string
		if cfg, cfgErr := deps.PVE.QEMU().Config(ctx, node, vmid); cfgErr == nil {
			if v, ok := cfg[jsonKeyTags]; ok {
				if s, ok := v.(string); ok {
					existing = parseTagsField(s)
				}
			}
		}
		tags := mergeTagList(existing, []string{tagDeletingVM}, maxTagLength)
		if err := deps.PVE.Nodes().UpdateQemuConfig(ctx, node, vmCID,
			&sdknodes.UpdateQemuConfigParams{Tags: &tags}); err != nil {
			logger.Warn("delete_vm: fast-path tag write failed (non-fatal; destroy will proceed)",
				log.String("vmid", vmCID), log.Err(err))
		}
	}

	lockOwner := fmt.Sprintf("stampDeletingTag/%d", vmid)
	lockErr := withVMIDLock(ctx, deps.PVE.Pools(), vmid, lockOwner, logger, func() error {
		stampRMW()
		return nil
	})
	if lockErr != nil {
		// Pool service unavailable or cluster fault: proceed unlocked so the
		// diagnostic tag is never silently skipped.
		logger.Warn("delete_vm: stampDeletingTag: could not acquire VMID lock; tagging without lock (best-effort)",
			log.String("vmid", vmCID), log.Err(lockErr))
		stampRMW()
	}
}

// sweepStragglersMaxPerSweep caps how many straggler VMs are reaped in a single
// sweepFastDeleteStragglers call. Stragglers beyond the cap are left for the
// next sweep (which runs on every fast-path delete_vm), so they converge
// without blocking the current delete for more than this many sequential
// round-trips.
const sweepStragglersMaxPerSweep = 5

// sweepFastDeleteStragglers scans the cluster for VMs carrying tagDeletingVM
// (bosh-deleting) and re-issues a skiplock+purge destroy for each one. This
// makes fast-path deletes self-healing: a VM whose async destroy stalled is
// reaped by the next fast-path delete call. The sweep is:
//   - Best-effort: all errors are logged, never propagated.
//   - Bounded: processes at most sweepStragglersMaxPerSweep per call; any
//     remainder is logged and left for the next sweep invocation.
//   - Idempotent: a VM already gone (IsNotFound) is silently skipped.
//
// Call from the fast path before issuing the current delete so a straggler
// accumulation does not grow unbounded across deployments.
func sweepFastDeleteStragglers(ctx context.Context, deps Deps, logger *log.Logger) {
	if deps.PVE == nil || deps.PVE.Cluster() == nil {
		return
	}
	resp, err := deps.PVE.Cluster().ListResources(ctx, &sdkcluster.ListResourcesParams{})
	if err != nil {
		logger.Warn("delete_vm: straggler sweep: ListResources failed (non-fatal)", log.Err(err))
		return
	}
	if resp == nil {
		return
	}

	type clusterItem struct {
		Type string `json:"type"`
		VMID int64  `json:"vmid"`
		Node string `json:"node"`
		Tags string `json:"tags"`
	}

	purge := true
	skiplock := true
	processed := 0
	skipped := 0
	for _, raw := range *resp {
		var item clusterItem
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		if item.Type != resourceTypeQemu || item.VMID == 0 || item.Node == "" {
			continue
		}
		if !tagsContain(item.Tags, tagDeletingVM) {
			continue
		}
		if processed >= sweepStragglersMaxPerSweep {
			skipped++
			continue
		}
		processed++
		vmIDStr := strconv.FormatInt(item.VMID, 10)
		sweepLogger := logger.With(
			log.String("node", item.Node),
			log.String("vmid", vmIDStr),
		)
		// Authoritative config read comes FIRST — before any mutation this
		// straggler triggers. The index row that selected this VMID can be
		// minutes stale: the tagged VM may already be destroyed and the VMID
		// reused by a live, untagged VM, in which case the skiplock+purge
		// below would destroy a healthy guest and every disk it references.
		// The config read is node-local truth. A "VM already gone" shape
		// (404, or pmxcfs's config-missing 500) means there is nothing left
		// to reap; any other read failure defers the straggler (fail-closed).
		strCfg, strCfgErr := deps.PVE.QEMU().Config(ctx, item.Node, int(item.VMID))
		if strCfgErr != nil {
			if pve.IsNotFound(strCfgErr) || pve.IsPmxcfsConfigMissing(strCfgErr) {
				sweepLogger.Debug("delete_vm: straggler sweep: VM already gone")
				continue
			}
			sweepLogger.Warn("delete_vm: straggler sweep: config read failed; deferring straggler to next sweep (non-fatal)",
				log.Err(strCfgErr))
			continue
		}
		// Re-test the deleting tag from the authoritative config, not the
		// index row. Tag absent means the row is stale — this VMID's current
		// occupant was never marked for deletion, so it must not be destroyed.
		authTags, _ := strCfg[jsonKeyTags].(string)
		if !tagsContain(authTags, tagDeletingVM) {
			sweepLogger.Warn("delete_vm: straggler sweep: index row is stale — the VM's authoritative config does not carry the deleting tag; skipping (VMID likely reused by a live VM)")
			continue
		}
		// Base value is pve.destroy_unreferenced_disks (default false; see the
		// config field doc for the cross-cluster shared-storage data-loss
		// hazard enabling it introduces).
		//
		// A straggler may carry tagRetainEphemeral: its original fast-path delete
		// stamped bosh-deleting before the retain detach ran, so the straggler can
		// hold an ephemeral disk in any state — still attached, or already
		// unlinked+swept (unreferenced with a matching VMID, exactly what
		// DestroyUnreferencedDisks=true frees). Re-run the detach to finish any
		// pending unlink, and force the destroy flag false for retain-tagged
		// stragglers regardless of the config knob. On detach failure, skip this
		// straggler (left for the next sweep) rather than destroy with the
		// volume in an unknown state. The tag is read from the authoritative
		// config for the same staleness reason as the deleting tag above.
		destroyDisks := deps.Config.DestroyUnreferencedDisks
		if tagsContain(authTags, tagRetainEphemeral) {
			retained, retainErr := detachRetainedEphemeralDisk(ctx, deps, item.Node, vmIDStr, int(item.VMID), sweepLogger)
			if retainErr != nil {
				sweepLogger.Warn("delete_vm: straggler sweep: retain-ephemeral detach failed; deferring straggler to next sweep (non-fatal)",
					log.Err(retainErr))
				continue
			}
			destroyDisks = destroyDisks && !retained
		}
		// A straggler can still hold a foreign persistent disk on an active
		// slot: fastPathDeleteVM stamps bosh-deleting BEFORE its
		// detachForeignActiveDisks call, so a failed detach leaves a tagged
		// VM whose purge here would destroy the volume. Defer such a
		// straggler instead of destroying it -- the Director's delete_vm
		// retry re-runs the real detach (including parker transfers), and
		// once the disks are off, a later sweep or the retry itself reaps
		// the VM.
		if foreign := pve.FindForeignActiveDisks(strCfg, int(item.VMID)); len(foreign) > 0 {
			sweepLogger.Warn("delete_vm: straggler sweep: foreign persistent disks still attached; deferring to the delete_vm retry to detach them",
				log.Any("slots", foreign))
			continue
		}
		// Same unusedN protection the sync and fast paths run: a persistent
		// volume demoted to unusedN (snapshot-pinned, sweep-resistant) would
		// be destroyed with the VM.
		if guardErr := guardUnusedVolumes(ctx, deps, item.Node, vmIDStr, int(item.VMID), deps.Config.DiskStorage); guardErr != nil {
			sweepLogger.Warn("delete_vm: straggler sweep: unused-volume guard refused destroy; deferring straggler to next sweep (non-fatal)",
				log.Err(guardErr))
			continue
		}
		// Fire-and-forget: discard the UPID, no await. Purge's pool-membership
		// interplay: see the synchronous-path DeleteQemu comment in
		// HandleDeleteVM. Deliberately single-shot, unlike the other cleanup
		// mutations: the sweep is a self-draining queue (a straggler whose
		// destroy loses a lock race stays tagged bosh-deleting and the next
		// fast-path delete re-issues it), so an in-place retry would only
		// serialize the request path behind contention the queue absorbs for
		// free.
		_, delErr := deps.PVE.Nodes().DeleteQemu(ctx, item.Node, vmIDStr, &sdknodes.DeleteQemuParams{
			Purge:                    &purge,
			DestroyUnreferencedDisks: &destroyDisks,
			Skiplock:                 &skiplock,
		})
		if delErr != nil {
			if pve.IsNotFound(delErr) {
				sweepLogger.Debug("delete_vm: straggler sweep: VM already gone")
			} else {
				sweepLogger.Warn("delete_vm: straggler sweep: destroy failed (non-fatal)", log.Err(delErr))
			}
			continue
		}
		sweepLogger.Info("delete_vm: straggler sweep: re-issued destroy for bosh-deleting VM")
	}
	if skipped > 0 {
		logger.Info("delete_vm: straggler sweep: cap reached; remaining stragglers deferred to next sweep",
			log.Int("processed", processed),
			log.Int("deferred", skipped),
		)
	}
}

// fastPathDeleteVM executes the fast-path delete for a single VM. It:
//  1. Runs sweepFastDeleteStragglers to reap any prior stalled fast-path destroys.
//  2. Stamps bosh-deleting on the current VM (best-effort, fail-open).
//  3. Issues Stop fire-and-forget (UPID discarded, no await).
//  4. Issues DeleteQemu with skiplock=true (handles running/locked VMs) without
//     awaiting the returned task UPID.
//  5. Returns immediately — no unbounded get_task poll on any task.
//
// The fast path intentionally does NOT run the pve.pool_reap_empty reaper: it
// is the bounded-time, "known-simple" path and the reaper needs a pre-destroy
// pool lookup plus a post-destroy task await, neither of which fit its
// fire-and-forget contract. An empty pool left behind by a fast-path delete
// is still reaped the next time a sync-path delete_vm empties it, or by an
// operator running `pvesh delete /pools/<id>` directly.
//
// skiplock=true is restricted to the root@pam superuser authenticated via
// password; PVE rejects it for any other identity regardless of granted
// privileges — including an API token owned by root@pam (see
// pve.IsRootPamIdentity's doc comment for why) — so the CPI must authenticate
// as root@pam with a password to clear locked VMs here.
// Eventual consistency: has_vm may briefly still see this VM after return.
func fastPathDeleteVM(ctx context.Context, deps Deps, node, vmCID string, vmid int, logger *log.Logger) error {
	// Reap any straggler fast-path VMs cluster-wide before issuing our own
	// destroy. Best-effort; never blocks the current delete.
	sweepFastDeleteStragglers(ctx, deps, logger)

	// Stamp diagnostic tag — fail-open.
	stampDeletingTag(ctx, deps, node, vmCID, vmid, logger)

	// Deregister HA state BEFORE the stop. While a VM is HA-managed, a
	// status/stop call is redirected to a CRM request whose task completes on
	// acceptance, not when the VM halts — and the destroy below cannot remove
	// a running VM (skiplock only skips config locks). Deregistering first
	// restores plain qmstop semantics. Both calls are synchronous and bounded
	// (no task await) — compatible with the fast path's bounded-time contract.
	// Idempotent, not-found-tolerant, best-effort.
	if deps.Config.AntiAffinityUseHaRulesEnabled() || deps.Config.DLBConfigured() {
		if aaErr := removeAntiAffinityMembership(ctx, deps, vmid, logger); aaErr != nil {
			logger.Warn("delete_vm: fast-path HA anti-affinity/DLB cleanup incomplete (non-fatal)", log.Err(aaErr))
		}
	}
	// Remove the node-affinity HA pin as well. The fast path returns before
	// the sync path's pin cleanup, so without this a pinned VM (AZ pin or PCI
	// strict pin) deleted via fast_path_delete would leave an orphan
	// bosh-na-<vmid> rule forever.
	if pinErr := removeNodeAffinityPin(ctx, deps, vmid, logger); pinErr != nil {
		logger.Warn("delete_vm: fast-path HA node-affinity pin cleanup incomplete (non-fatal)", log.Err(pinErr))
	}

	// Fire-and-forget stop. The UPID is discarded; no await. PVE may not finish
	// the stop before destroy arrives; the destroy task can still fail on a
	// running VM, in which case the bosh-deleting stamp above queues this VM for
	// sweepFastDeleteStragglers to reap on a later fast-path delete.
	logger.Debug("delete_vm: fast-path: issuing stop fire-and-forget")
	_, stopErr := deps.PVE.QEMU().Stop(ctx, node, vmid)
	if stopErr != nil && !pve.IsNotFound(stopErr) {
		// Log but do not abort: the skiplock destroy handles a still-running VM.
		logger.Warn("delete_vm: fast-path: stop issued but returned error (non-fatal; destroy proceeds with skiplock)",
			log.Err(stopErr))
	}
	if pve.IsNotFound(stopErr) {
		// VM already gone — clean up agent state and return success.
		logger.Info("delete_vm: fast-path: VM not found during stop — already deleted")
		if agentErr := deps.Agent.Remove(ctx, node, vmid); agentErr != nil {
			logger.Warn("delete_vm: agent.Remove failed after fast-path not-found stop", log.Err(agentErr))
		}
		return nil
	}

	// Protect attached persistent disks before the skiplock destroy. Synchronous
	// config PUTs only — does not introduce an unbounded await. When this detach
	// fails, the VM stays tagged bosh-deleting with the disk still attached;
	// sweepFastDeleteStragglers defers exactly that shape (it re-checks for
	// foreign active disks before destroying), so the disk survives until a
	// delete_vm retry detaches it.
	if protErr := detachForeignActiveDisks(ctx, deps, node, vmCID, vmid, logger); protErr != nil {
		return protErr
	}
	// Preserve the VM's own ephemeral disk when retain_ephemeral_on_delete is set.
	// Returns retained=true whenever the retain tag is present (even if the disk
	// was already unlinked on a prior attempt). The retained flag gates
	// DestroyUnreferencedDisks below.
	retained, retainErr := detachRetainedEphemeralDisk(ctx, deps, node, vmCID, vmid, logger)
	if retainErr != nil {
		return retainErr
	}
	// Same unusedN guard the sync path runs. Without it the fast-path purge would
	// still destroy a persistent volume left in an unusedN slot — e.g. a foreign
	// disk whose detach above demoted it to unusedN but could not sweep it
	// because a snapshot references the volume. guardUnusedVolumes existence-
	// probes the configured pve.disk_storage and fails closed on any volume it
	// cannot confirm deleted.
	if guardErr := guardUnusedVolumes(ctx, deps, node, vmCID, vmid, deps.Config.DiskStorage); guardErr != nil {
		return guardErr
	}

	// Issue destroy with skiplock=true. Discard the UPID; no await.
	// HA pin and anti-affinity/DLB deregistration already ran before the stop
	// above (see the CRM-interference comment there).
	// Purge's pool-membership interplay: see the synchronous-path DeleteQemu
	// comment in HandleDeleteVM.
	//
	// DestroyUnreferencedDisks is pve.destroy_unreferenced_disks (default
	// false; see the config field doc for the cross-cluster shared-storage
	// data-loss hazard it introduces when enabled) AND-ed with !retained:
	// retain semantics always win regardless of the knob -- after the
	// unlink+sweep sequence the ephemeral volume is unreferenced AND has a
	// matching VMID, exactly what DestroyUnreferencedDisks=true would free.
	logger.Debug("delete_vm: fast-path: issuing skiplock destroy without await")
	purge := true
	destroyDisks := deps.Config.DestroyUnreferencedDisks && !retained
	skiplock := true
	_, delErr := deps.PVE.Nodes().DeleteQemu(ctx, node, vmCID, &sdknodes.DeleteQemuParams{
		Purge:                    &purge,
		DestroyUnreferencedDisks: &destroyDisks,
		Skiplock:                 &skiplock,
	})
	if delErr != nil {
		if pve.IsNotFound(delErr) {
			logger.Info("delete_vm: fast-path: VM not found during destroy — already deleted")
			if agentErr := deps.Agent.Remove(ctx, node, vmid); agentErr != nil {
				logger.Warn("delete_vm: agent.Remove failed after fast-path idempotent destroy", log.Err(agentErr))
			}
			return nil
		}
		return cpierrors.Wrap(pve.WrapError(delErr), fmt.Sprintf("delete_vm: fast-path destroy VM %s", vmCID))
	}

	// Agent cleanup is best-effort; VM may still be tearing down asynchronously.
	cleanupAgentForVM(ctx, deps, node, vmid, logger)
	logger.Info("delete_vm: fast-path: destroy issued, returning without task await (eventual consistency)")
	return nil
}

// HandleDeleteVM returns a handler for the delete_vm CPI method.
//
// Arguments:
//   - args[0]: vm_cid (string) — integer VMID as a string.
//
// Logic:
//  1. Parse vm_cid -> vmid int.
//  2. Locate VM via cluster scan (FindVMNodeViaCluster) to get authoritative node.
//     Not-found -> nil (idempotent; VM is already absent).
//     Transport error -> propagate.
//  3. Stop VM via qemu.Stop + pve.AwaitTask (idempotent on 404 -> return nil).
//  4. Delete VM via nodes.DeleteQemu (purge=true, destroy-unreferenced-disks=true).
//  5. Call deps.Agent.Remove to clean up registry/cloud-init state.
//
// Returns nil result on success (including when the VM was already absent).
func HandleDeleteVM(deps Deps) cpi.Handler {
	return cpi.HandlerFunc(func(ctx context.Context, args []json.RawMessage, reqCtx jsonrpc.Context) (any, error) {
		deps, err := deps.WithRequestOverrides(ctx, reqCtx)
		if err != nil {
			return nil, err
		}
		vmCID, vmid, err := parseDeleteVMArgs(args)
		if err != nil {
			return nil, err
		}

		logger := deps.Log(ctx).With(log.String("vm_cid", vmCID), log.Int("vmid", vmid))

		// --- locate VM authoritatively ---
		// The cluster scan's hit is authoritative (correct even after an HA
		// failover), but its miss is not: /cluster/resources lags node-local
		// state by minutes on loaded clusters, and treating a stale miss as
		// "already deleted" makes the Director drop its record while the live
		// VM keeps running with its IP. FindVMAuthoritative therefore proves
		// absence on a miss with per-node config probes before this handler
		// may take the idempotent-success branch; any probe failure surfaces
		// retriable instead. Transport error -> propagate.
		logger.Debug("delete_vm: locating VM (cluster scan, per-node probes on miss)")
		loc, lookupErr := pve.FindVMAuthoritative(ctx, deps.PVE, vmid)
		if lookupErr != nil {
			return nil, cpierrors.Wrap(lookupErr, fmt.Sprintf("delete_vm: locate VM %s", vmCID))
		}
		node, vmTags := loc.Node, loc.Tags
		if !loc.Found || node == "" {
			logger.Info("delete_vm: VM absent from cluster index and every node's config probe — already deleted, returning success")
			// Best-effort agent cleanup: registry/cloud-init state may still exist.
			if agentErr := deps.Agent.Remove(ctx, deps.Config.Node, vmid); agentErr != nil {
				logger.Warn("delete_vm: agent.Remove failed after cluster-not-found", log.Err(agentErr))
			}
			return nil, nil
		}
		logger.Debug("delete_vm: VM located", log.String("node", node))

		// --- parker VM refusal (belt-and-braces PVE-level backstop) ---
		if alreadyGone, parkerErr := refuseIfParkerVM(ctx, deps, node, vmid, vmTags); parkerErr != nil {
			return nil, parkerErr
		} else if alreadyGone {
			return nil, nil
		}

		// --- per-node in-flight gate (opt-in; limit=0 → unlimited, no gating) ---
		if deps.Config != nil {
			inflightRelease, inflightErr := deps.Inflight.acquire(ctx, node, deps.Config.MaxInflightPerNodeLimit())
			if inflightErr != nil {
				return nil, cpierrors.Retriable("delete_vm: in-flight limit exceeded or context cancelled on node %s: %s", node, inflightErr.Error())
			}
			defer inflightRelease()
		}

		// --- fast-path branch: tag-and-return without terminal-state poll ---
		// When fast_path_delete is enabled the handler must return in bounded time
		// regardless of PVE task behaviour. This requires bypassing ALL unbounded
		// AwaitTaskWithLogger calls — including the stop-task await that
		// stopVMBeforeDelete performs. The fast path therefore takes its own stop
		// approach: issue Stop fire-and-forget (UPID discarded, no await), then
		// issue DeleteQemu with skiplock=true so PVE destroys the VM even if it is
		// still running or holds a config lock. The destroy call itself is NOT
		// awaited — the UPID is discarded and the handler returns immediately.
		//
		// skiplock=true is restricted to the root@pam superuser authenticated via
		// password — PVE rejects it for any other identity regardless of role or
		// privilege, including an API token owned by root@pam, so no ACL grant on
		// a least-privilege token (or any token at all) can enable it; run the
		// CPI as root@pam with a password to use it.
		//
		// Eventual consistency: a subsequent has_vm call may briefly still see the
		// VM while PVE's async destroy runs. The bosh-deleting tag marks VMs whose
		// fast-path destroy was issued but may not have completed; sweepFastDeleteStragglers
		// reaps them on the next fast-path delete (a self-draining work queue), so a
		// stalled async destroy is retried automatically rather than left for manual
		// `qm destroy <vmid>` cleanup.
		//
		// The fast path bypasses the §7.15 operation-timeout envelope naturally:
		// no poll loop runs; the handler returns as soon as the destroy API call
		// returns (which itself is bounded by the HTTP transport timeout).
		if deps.Config.FastPathDeleteEnabled() {
			if fpErr := fastPathDeleteVM(ctx, deps, node, vmCID, vmid, logger); fpErr != nil {
				return nil, fpErr
			}
			cleanupAdvertisedRoutes(ctx, deps, vmid, vmTags, logger)
			return nil, nil
		}

		// --- HA anti-affinity / DLB / node-affinity pin cleanup (best-effort) ---
		cleanupHAMembership(ctx, deps, vmid, logger)

		// --- stop VM (synchronous path) ---
		if stopDone, stopErr := stopVMBeforeDelete(ctx, deps, node, vmid, vmCID, logger); stopErr != nil {
			return nil, stopErr
		} else if stopDone {
			cleanupAdvertisedRoutes(ctx, deps, vmid, vmTags, logger)
			return nil, nil
		}

		// --- protect attached persistent disks: detach foreign-VMID volumes so
		//     the destroy below cannot take them; refuse if a detach is not
		//     guaranteed (fail-closed, retriable) ---
		if protErr := detachForeignActiveDisks(ctx, deps, node, vmCID, vmid, logger); protErr != nil {
			return nil, protErr
		}

		// --- preserve the VM's own ephemeral disk when retain_ephemeral_on_delete is set ---
		// retained=true whenever the retain tag is present (even if a prior attempt
		// already unlinked the disk); DestroyUnreferencedDisks must be false on that
		// path. See function doc.
		retained, retainErr := detachRetainedEphemeralDisk(ctx, deps, node, vmCID, vmid, logger)
		if retainErr != nil {
			return nil, retainErr
		}

		// --- guard: refuse to destroy if a persistent volume is still attached ---
		if guardErr := guardUnusedVolumes(ctx, deps, node, vmCID, vmid, deps.Config.DiskStorage); guardErr != nil {
			return nil, guardErr
		}

		// --- opt-in empty-pool reaper: capture membership BEFORE destroy ---
		// The /cluster/resources row for this vmid (and therefore its pool
		// membership) disappears once the VM is destroyed, and delete_vm has
		// no env.bosh to re-derive a pool name from -- so the lookup must run
		// here, before the destroy call below.
		reapPool := capturePoolForReap(ctx, deps, vmid, logger)

		// --- delete VM (synchronous path; see destroyVMWithRecovery for the
		//     purge/DestroyUnreferencedDisks semantics and recovery ladder) ---
		deleteResp, alreadyGone, deleteErr := destroyVMWithRecovery(ctx, deps, node, vmCID, vmid, retained, logger)
		if deleteErr != nil {
			return nil, deleteErr
		}
		if alreadyGone {
			cleanupAdvertisedRoutes(ctx, deps, vmid, vmTags, logger)
			return nil, nil
		}

		// Await the destroy task so the VM is fully purged from PVE before we
		// return. DeleteQemu returns a UPID as a json.RawMessage; an empty or
		// null response means PVE completed synchronously and no await is needed.
		if awaitErr := awaitDeleteTask(ctx, deps, node, vmCID, deleteResp, logger); awaitErr != nil {
			return nil, awaitErr
		}

		// --- opt-in empty-pool reaper: run AFTER the destroy has completed so
		//     PVE has already dropped this VM's pool membership. Never fails
		//     delete_vm -- every branch is logged and swallowed.
		reapEmptyPoolIfManaged(ctx, deps, reapPool, logger)

		// --- agent cleanup ---
		cleanupAgentForVM(ctx, deps, node, vmid, logger)

		// --- advertised-route SDN subnet cleanup (provenance-tagged, refcounted,
		//     entirely fail-open — see delete_vm_routes.go) ---
		cleanupAdvertisedRoutes(ctx, deps, vmid, vmTags, logger)

		logger.Info("delete_vm: VM deleted successfully", log.String("node", node))
		return nil, nil
	})
}

// parseDeleteVMArgs extracts and validates the vm_cid argument, returning the
// CID string and its integer VMID.
func parseDeleteVMArgs(args []json.RawMessage) (string, int, error) {
	if len(args) < 1 {
		return "", 0, cpierrors.Cloud("delete_vm: missing required argument vm_cid")
	}
	var vmCID string
	if err := json.Unmarshal(args[0], &vmCID); err != nil {
		return "", 0, cpierrors.Cloud("delete_vm: vm_cid must be a string: %s", err.Error())
	}
	if vmCID == "" {
		return "", 0, cpierrors.Cloud("delete_vm: vm_cid must not be empty")
	}
	vmid, err := strconv.Atoi(vmCID)
	if err != nil {
		return "", 0, cpierrors.Cloud("delete_vm: vm_cid %q is not a valid integer VMID: %s", vmCID, err.Error())
	}
	if vmid <= 0 {
		return "", 0, cpierrors.Cloud("delete_vm: vm_cid %q must be a positive integer", vmCID)
	}
	return vmCID, vmid, nil
}

// refuseIfParkerVM is the belt-and-braces PVE-level backstop against deleting
// a parker VM. The Director should never hand a parker CID to delete_vm
// (parkers are internal CPI state), but `bosh delete-vm` and cloud-check's
// "delete VM reference" both reach this handler with an operator-supplied CID.
//
// Classification is by tag, not by band membership. The band is a
// configuration value and the tag is a fact about the VM: an operator who
// opts out of parking without carrying the band forward would otherwise
// disarm this guard over exactly the parkers their change stranded. The fast
// path issues skiplock=true + purge=true, which bypasses protection=1 and
// would destroy every disk that parker holds -- up to 31 of them, from
// deployments this call has nothing to do with.
//
// vmTags comes from the /cluster/resources?type=vm row the handler has already
// read to locate the VM, so the check normally costs nothing. PVE populates
// tags on that row (cleanupAdvertisedRoutes relies on the same field), and a
// non-empty value is authoritative. An empty one is not: it cannot distinguish
// a genuinely untagged VM from a PVE that omits the field, so it falls back to
// reading the config. Every parker the CPI creates is tagged, so on a
// CPI-managed cluster that fallback never fires.
//
// Returns alreadyGone=true when the VM vanished during the fallback config
// read (the caller should treat delete_vm as already complete).
func refuseIfParkerVM(ctx context.Context, deps Deps, node string, vmid int, vmTags string) (alreadyGone bool, err error) {
	if deps.Config == nil {
		return false, nil
	}
	if pve.TagsMarkParker(vmTags) {
		return false, cpierrors.Cloud("refusing to delete parker VM %d", vmid)
	}
	if strings.TrimSpace(vmTags) != "" {
		// The row carried tags and they do not mark a parker: authoritative.
		return false, nil
	}
	// No tags on the row. That is either a VM with no tags -- which no parker
	// ever is, since the CPI stamps bosh-cpi and bosh-parker at creation -- or a
	// PVE that does not populate the field, in which case the row proves
	// nothing and the config is the only place the answer lives. Read it. The
	// cost lands only on untagged VMs, which for a CPI-managed cluster is none,
	// and the alternative is a band-gated fallback that goes quiet for exactly
	// the operator who dropped the band while parkers were still standing.
	// Fail-closed: a transient config read error must NOT proceed to a purge.
	// Any doubt is retriable so the Director retries when PVE recovers.
	vmCfg, cfgErr := deps.PVE.QEMU().Config(ctx, node, vmid)
	if cfgErr != nil {
		if pve.IsNotFound(cfgErr) {
			// VM gone during the read -- treat as already deleted.
			return true, nil
		}
		return false, cpierrors.Retriable(
			"delete_vm: could not read config for VMID %d to verify parker status: %s (retry when PVE recovers)",
			vmid, cfgErr.Error())
	}
	tagsRaw, _ := vmCfg[jsonKeyTags].(string)
	if pve.TagsMarkParker(tagsRaw) {
		return false, cpierrors.Cloud("refusing to delete parker VM %d", vmid)
	}
	return false, nil
}

// cleanupHAMembership removes the VM's CPI-managed HA state before the stop:
// anti-affinity/DLB membership and the per-VM node-affinity pin. Order
// matters: while a VM is HA-managed, a status/stop call is redirected to a
// CRM request whose task completes when the request is accepted — not when
// the VM actually halts — so a stop-then-deregister sequence races the LRM
// and the subsequent destroy fails with "VM is running". Deregistering first
// restores plain qmstop semantics for the stop that follows.
//
// The anti-affinity/DLB removal is gated on its opt-ins; it also covers
// DLB-only VMs (removeAntiAffinityMembership purges the HA resource and
// prunes any associated rules; for a DLB-only VM with no affinity rule it
// simply deregisters the HA resource). The node-affinity pin removal is
// unconditional because two writers create that rule: the AZ pin (gated by
// placement.pin_az_via_ha_rules) and the PCI strict pin (applied whenever
// pci_passthroughs is set, regardless of that flag). delete_vm has no
// cloud_properties, so it cannot know which writer ran; removing
// unconditionally guarantees no orphan rule survives the VM. For a VM that
// never had a pin this is two cheap not-found no-ops.
//
// Everything here is best-effort: HA failures are logged and never block VM
// deletion.
func cleanupHAMembership(ctx context.Context, deps Deps, vmid int, logger *log.Logger) {
	if deps.Config.AntiAffinityUseHaRulesEnabled() || deps.Config.DLBConfigured() {
		if aaErr := removeAntiAffinityMembership(ctx, deps, vmid, logger); aaErr != nil {
			logger.Warn("delete_vm: HA anti-affinity/DLB cleanup incomplete (non-fatal)", log.Err(aaErr))
		}
	}
	if pinErr := removeNodeAffinityPin(ctx, deps, vmid, logger); pinErr != nil {
		logger.Warn("delete_vm: HA node-affinity pin cleanup incomplete (non-fatal)", log.Err(pinErr))
	}
}

// destroyVMWithRecovery issues the DeleteQemu destroy with the standard
// transient/lock retry, then works the two recoverable refusals PVE can
// answer with:
//
//   - "VM is running - destroy failed": the stop above completed its task,
//     but a stop accepted while the VM was still HA-managed (the CRM files
//     the request and the task ends before the LRM halts the guest) can
//     leave the VM briefly running. Wait until it actually reports stopped,
//     then destroy once more. Observed live on a DLB-registered compilation
//     VM.
//   - config lock (lock: clone|create|...): a killed worker or node reboot
//     mid-clone/mid-create leaves the guest config locked, which PVE rejects
//     the destroy against — without recovery the Director would retry
//     delete_vm forever against a VM that can never come unstuck on its own.
//     Retry once with skiplock=true when the CPI is authenticated as
//     root@pam (the only identity PVE honors skiplock for); otherwise
//     retryDestroyWithSkiplock returns the error unretried and the
//     still-locked branch below surfaces the actionable `qm unlock` error.
//
// Purge removes the VMID from backup/HA/replication configs (per PVE's own
// API description). Resource-pool membership (pve.vm_pool,
// stemcell_template_pool) is not explicitly documented as part of what purge
// cleans up — the delete endpoint's own description separately says it
// "Removes any VM specific permissions [ACLs] and firewall rules", which is a
// related but distinct PVE construct from pool membership. A stale
// pool-membership entry for a deleted VMID (if PVE leaves one) carries no
// capability risk — there is no VM behind that VMID to act on — but this is
// not verified against a live cluster; do not depend on the pool member list
// shrinking immediately after delete_vm without checking your PVE version.
//
// DestroyUnreferencedDisks is pve.destroy_unreferenced_disks (default false)
// AND-ed with !retained: it is always false on the retain path (the
// ephemeral disk is now unreferenced + own-VMID — true would destroy it)
// regardless of the config knob, and otherwise reflects the operator's
// opt-in. See detachRetainedEphemeralDisk for the retain rationale and the
// DestroyUnreferencedDisks field doc on config.CPIConfig for the
// cross-cluster shared-storage data-loss hazard enabling the knob
// introduces. When true it triggers pvesm free under the per-storage
// lockfile for every attached volume, so on bursty deploys this can surface
// "can't lock file ... got timeout" — the retry wrapper absorbs that signal;
// everything else propagates immediately.
//
// Returns alreadyGone=true for the idempotent not-found path (agent state
// already cleaned; the caller finishes route cleanup and returns success).
func destroyVMWithRecovery(
	ctx context.Context, deps Deps, node, vmCID string, vmid int, retained bool, logger *log.Logger,
) (resp *sdknodes.DeleteQemuResponse, alreadyGone bool, err error) {
	purge := true
	destroyDisks := deps.Config.DestroyUnreferencedDisks && !retained
	logger.Debug("delete_vm: deleting VM")
	var deleteResp *sdknodes.DeleteQemuResponse
	attemptDestroy := func() error {
		return pve.RetryOnTransientOrLock(ctx, logger, "delete_vm", 0, func() error {
			var innerErr error
			deleteResp, innerErr = deps.PVE.Nodes().DeleteQemu(ctx, node, vmCID, &sdknodes.DeleteQemuParams{
				Purge:                    &purge,
				DestroyUnreferencedDisks: &destroyDisks,
			})
			return innerErr
		})
	}
	deleteErr := attemptDestroy()
	if deleteErr != nil && pve.IsVMRunningDestroyFailure(deleteErr) {
		logger.Info("delete_vm: VM still running after stop task — waiting for it to halt before destroy retry")
		if waitErr := waitForVMStopped(ctx, deps, node, vmid, vmCID, logger); waitErr != nil {
			return nil, false, waitErr
		}
		deleteErr = attemptDestroy()
	}
	if deleteErr != nil && pve.IsVMConfigLocked(deleteErr) {
		deleteResp, deleteErr = retryDestroyWithSkiplock(ctx, deps, node, vmCID, vmid, purge, destroyDisks, deleteErr, logger)
	}
	if deleteErr != nil {
		if pve.IsNotFound(deleteErr) {
			logger.Info("delete_vm: VM not found during delete -- already deleted, returning success")
			// Still clean up registry/cloud-init state if it exists; errors
			// are logged but not fatal.
			cleanupAgentForVM(ctx, deps, node, vmid, logger)
			return nil, true, nil
		}
		if pve.IsVMConfigLocked(deleteErr) {
			// Still locked after any skiplock attempt: surface an
			// actionable, retriable error naming the lock type and the
			// `qm unlock <vmid>` recovery command, instead of a generic
			// 5xx the Director would retry forever with no diagnostic value.
			logUnresolvedVMLock(logger, "delete_vm: delete failed", vmid, node, deleteErr)
			return nil, false, pve.WrapVMConfigLocked(deleteErr, vmid, node)
		}
		return nil, false, cpierrors.Wrap(pve.WrapError(deleteErr), fmt.Sprintf("delete_vm: delete VM %s", vmCID))
	}
	return deleteResp, false, nil
}

// stopVMBeforeDelete stops the VM and awaits the stop task.
// Returns (true, nil) when the caller should treat the operation as already complete
// (idempotent not-found paths). Returns (false, nil) on clean stop. Returns (false, err) on failure.
func stopVMBeforeDelete(ctx context.Context, deps Deps, node string, vmid int, vmCID string, logger *log.Logger) (done bool, err error) {
	// Stop returns a UPID; if VM is not found (already deleted), treat as success.
	// Wrap in RetryOnTransient so a pvedaemon worker-recycle (HTTP 5xx /
	// "got no worker upid - start worker failed") under burst load is
	// absorbed in-process rather than surfacing as RetriableCloudError to
	// the director.
	logger.Debug("delete_vm: stopping VM")
	var stopUPID string
	stopErr := pve.RetryOnTransient(ctx, logger, "delete_vm.stop", 0, func() error {
		var innerErr error
		stopUPID, innerErr = deps.PVE.QEMU().Stop(ctx, node, vmid)
		return innerErr
	})
	if stopErr != nil {
		if pve.IsNotFound(stopErr) {
			logger.Info("delete_vm: VM not found during stop — already deleted, returning success")
			return true, nil
		}
		return false, cpierrors.Wrap(pve.WrapError(stopErr), fmt.Sprintf("delete_vm: stop VM %s", vmCID))
	}
	if stopUPID != "" {
		if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, stopUPID, logger); awaitErr != nil {
			// A qmstop task can be accepted (Stop returns a UPID) yet fail
			// when the VM's config has already been removed — surfaced as
			// "unable to find configuration file for VM ...". For delete_vm
			// that means the target is already gone, so treat it as success.
			if pve.IsNotFound(awaitErr) || pve.IsPmxcfsConfigMissing(awaitErr) {
				logger.Info("delete_vm: VM config missing during stop await — already deleted, returning success")
				if agentErr := deps.Agent.Remove(ctx, node, vmid); agentErr != nil {
					logger.Warn("delete_vm: agent.Remove failed after idempotent stop-await", log.Err(agentErr))
				}
				return true, nil
			}
			return false, cpierrors.Wrap(pve.WrapError(awaitErr),
				fmt.Sprintf("delete_vm: await stop task for VM %s", vmCID))
		}
	}

	return false, nil
}

// waitForVMStopped polls the VM's current status until it reports "stopped"
// (or the VM disappears, which the destroy retry handles idempotently). Only
// called on the "VM is running - destroy failed" path — the common case never
// issues a status read. Bounded by deleteStopWaitBudget; polls every
// deleteStopPollInterval. On budget exhaustion returns a retriable error —
// delete_vm is idempotent, so the Director can safely retry once the VM
// settles.
func waitForVMStopped(ctx context.Context, deps Deps, node string, vmid int, vmCID string, logger *log.Logger) error {
	deadline := time.Now().Add(deleteStopWaitBudget())
	for {
		st, statusErr := deps.PVE.QEMU().Status(ctx, node, vmid)
		if statusErr != nil {
			if pve.IsNotFound(statusErr) {
				// VM gone — nothing left to wait for.
				return nil
			}
			return cpierrors.Wrap(pve.WrapError(statusErr),
				fmt.Sprintf("delete_vm: status VM %s while waiting for stop", vmCID))
		}
		state, _ := st["status"].(string)
		if state == vmPowerStateStopped {
			return nil
		}
		if time.Now().After(deadline) {
			return cpierrors.Retriable(
				"delete_vm: VM %s still %q after stop task completed (waited %s); retry when it settles",
				vmCID, state, deleteStopWaitBudget())
		}
		logger.Debug("delete_vm: waiting for VM to stop", log.String("state", state))
		select {
		case <-ctx.Done():
			return cpierrors.Retriable("delete_vm: context cancelled while waiting for VM %s to stop", vmCID)
		case <-time.After(deleteStopPollInterval()):
		}
	}
}

// guardUnusedVolumes reads the VM config and refuses to destroy if any unusedN
// slot still references a live persistent volume.
//
// PVE's PUT config delete:scsiN demotes a disk to unusedN rather than fully
// clearing the config entry. A DELETE /qemu/{vmid} then destroys every disk
// still referenced -- unusedN included -- silently nuking the volume. The
// SDK's DetachDisk sweeps unusedN automatically, but this guard catches future
// SDK regressions or any other code path that reaches delete_vm with a dangling
// volume reference.
//
// Belt-and-suspenders: we ALWAYS scan unusedN entries regardless of whether
// diskStorage is configured. When diskStorage is known we can run an existence
// probe and safely skip stale dangling references. When diskStorage is empty
// (operator did not configure pve.disk_storage) we cannot probe existence --
// we fail CLOSED to avoid silently nuking a live volume. A storage mismatch is
// treated identically: the slot may belong to an unrecognised storage pool we
// cannot probe, so it also fails closed.
//
// Returns nil on success (no protected volumes found or VM config is 404).
// Returns cpierrors.Cloud when protected volumes are present.
// Returns a wrapped error when the config read fails for reasons other than 404.
func guardUnusedVolumes(ctx context.Context, deps Deps, node, vmCID string, vmid int, diskStorage string) error {
	vmCfg, cfgErr := deps.PVE.QEMU().Config(ctx, node, vmid)
	if cfgErr != nil {
		if !pve.IsNotFound(cfgErr) && !pve.IsPmxcfsConfigMissing(cfgErr) {
			return cpierrors.Wrap(pve.WrapError(cfgErr),
				fmt.Sprintf("delete_vm: read config for VM %s before destroy", vmCID))
		}
		// A 404 -- or pmxcfs's "Configuration file ... does not exist" 500,
		// which is the same condition wearing a different status code --
		// means the VM is gone; fall through to the destroy call below,
		// which handles the NotFound case idempotently.
		return nil
	}

	// Only an unusedN slot whose volume STILL EXISTS represents a real
	// persistent disk that the DELETE below would silently destroy. PVE
	// demotes a disk to unusedN on detach; when a snapshot still
	// references the volume the SDK's sweep cannot remove that slot, so
	// it can linger. Once the volume itself is deleted (e.g. delete_disk
	// runs before delete_vm) the slot is a dangling reference pointing at
	// nothing -- destroying the VM cannot lose data, so it must not block
	// delete_vm. Existence-probe failures fail closed (treated as
	// present) so a transient error never green-lights destroying a live
	// volume. Probe on the VM's node: any volume still referenced by this
	// VM's config is reachable from there.
	var protected []string
	for slot, volid := range pve.FindUnusedDiskEntries(vmCfg) {
		storage, _, parseErr := pve.ParseDiskCID(volid)
		if parseErr != nil {
			// Unparseable volid -- skip; can't determine storage.
			deps.Log(ctx).Warn("delete_vm: unused-slot has unparseable volid -- skipping",
				log.String("slot", slot), log.String("volid", volid))
			continue
		}
		if diskStorage == "" {
			// No configured disk storage: cannot probe existence.
			// Fail closed -- block destroy to avoid data loss.
			deps.Log(ctx).Warn("delete_vm: unused-slot present but pve.disk_storage not configured -- failing closed",
				log.String("slot", slot), log.String("volid", volid))
			protected = append(protected, fmt.Sprintf("%s=%s", slot, volid))
			continue
		}
		if storage != diskStorage {
			// Storage doesn't match configured disk storage: we cannot
			// probe existence on an unknown pool. Fail closed.
			deps.Log(ctx).Warn("delete_vm: unused-slot storage does not match pve.disk_storage -- failing closed",
				log.String("slot", slot), log.String("volid", volid),
				log.String("slot_storage", storage), log.String("disk_storage", diskStorage))
			protected = append(protected, fmt.Sprintf("%s=%s", slot, volid))
			continue
		}
		// ExistsTolerant: block-backed storages return 500 with
		// "Failed to find logical volume" for missing LVs rather
		// than 404. Treat those as "volume gone" so a stale unused
		// slot pointing at a deleted volume does not wedge delete_vm.
		exists, existErr := pve.ExistsTolerant(ctx, deps.PVE, node, diskStorage, volid)
		if existErr != nil {
			deps.Log(ctx).Warn("delete_vm: unused-slot volume existence probe failed -- treating slot as present (fail-closed)",
				log.String("slot", slot), log.String("volid", volid), log.Err(existErr))
		} else if !exists {
			deps.Log(ctx).Info("delete_vm: ignoring stale unused slot -- volume already deleted",
				log.String("slot", slot), log.String("volid", volid))
			continue
		}
		protected = append(protected, fmt.Sprintf("%s=%s", slot, volid))
	}
	if len(protected) > 0 {
		return cpierrors.Cloud(
			"delete_vm: refusing to destroy VM %s -- persistent volumes still attached as unused slots: %v (call detach_disk first or verify pve.disk_storage configuration)",
			vmCID, protected,
		)
	}
	return nil
}

// detachForeignActiveDisks protects persistent disks the BOSH Director attached
// to this VM but has not yet detached (e.g. an interrupted recreate). PVE's
// DELETE /qemu/{vmid} with purge=true destroys EVERY disk referenced by the VM
// config, including persistent volumes on active bus slots (scsi1, ...). Such a
// volume is recognised two ways: by its embedded VMID label differing from the
// VM's own VMID (create_disk allocates persistent volumes under a synthetic
// free VMID, so a disk "zfs-1:vm-15689-disk-0" attached to VM 6031 is
// foreign), or by a bpd- stable-ID serial on the drive entry — a move_disk
// reassignment renames a persistent volume for its new owner, so a
// transferred disk fails the VMID heuristic and the serial is what still
// marks it persistent.
//
// A legacy foreign disk is detached: DetachDisk fully unreferences the volume
// (the SDK demotes the slot to unusedN and sweeps it), leaving the volume
// intact on storage. A stable-ID disk is instead transferred to a parker —
// its volume is owner-named, and the detach's unusedN sweep would let PVE
// deallocate it. Only after every foreign disk is off the VM does delete_vm
// proceed to destroy it.
//
// Fail-closed: if a foreign disk cannot be detached, or any foreign disk still
// remains on an active slot after the attempt, a RETRIABLE error is returned
// and the VM is NOT destroyed — a transient PVE error never escalates to silent
// data loss. The Director retries delete_vm; the next attempt re-detaches and
// proceeds.
func detachForeignActiveDisks(ctx context.Context, deps Deps, node, vmCID string, vmid int, logger *log.Logger) error {
	cfg, cfgErr := deps.PVE.QEMU().Config(ctx, node, vmid)
	if cfgErr != nil {
		if pve.IsNotFound(cfgErr) || pve.IsPmxcfsConfigMissing(cfgErr) {
			// VM gone (404, or pmxcfs's config-missing 500) — the destroy
			// path handles the absence idempotently.
			return nil
		}
		return cpierrors.Wrap(pve.WrapError(cfgErr),
			fmt.Sprintf("delete_vm: read config for VM %s before foreign-disk detach", vmCID))
	}
	foreign := pve.FindForeignActiveDiskDetails(cfg, vmid)
	if len(foreign) == 0 {
		return nil
	}
	// The doomed VM's description is the disks' override-record carrier; it
	// dies with the VM, so any recorded overrides must move now or be lost.
	desc := pve.DescriptionFromConfig(cfg)
	slots := make([]string, 0, len(foreign))
	for slot := range foreign {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	for _, slot := range slots {
		entry := foreign[slot]
		// A stable-ID disk moves to a parker by reassignment instead of a
		// plain detach. Two reasons: after a reassignment the volume is named
		// for THIS VM, and the SDK detach's unusedN sweep would let PVE
		// deallocate a volume its holder owns — the exact data loss this
		// guard exists to prevent; and the disk's CID promises a parker
		// anchor, which a plain detach would leave unhonored.
		if entry.StableID != "" {
			logger.Warn("delete_vm: stable-ID persistent disk still attached on active slot -- transferring to a parker to preserve it before destroy",
				log.String("slot", slot), log.String("volid", entry.Volid), log.String("stable_id", entry.StableID))
			pctx := pve.ParkContext{
				SourceVMCID: vmCID,
				StableID:    entry.StableID,
				Opts:        pve.DiskOptOverlayFromDesc(desc, entry.StableID, entry.Volid),
			}
			if _, transferErr := pve.TransferDiskToParker(ctx, deps.PVE, logger, node, vmid, entry.Volid, parkerWriteConfigFor(deps), pctx); transferErr != nil {
				return retriableUnlessPermanent(transferErr, fmt.Sprintf(
					"delete_vm: refusing to destroy VM %s -- could not transfer persistent disk %s=%s to a parker to preserve it (the volume would otherwise be destroyed; retry resumes the transfer)",
					vmCID, slot, entry.Volid))
			}
			pve.RemoveAttachedDiskCID(ctx, deps.PVE, logger, node, vmid, entry.StableID, entry.Volid)
			continue
		}
		// A legacy foreign disk is plain-detached and left free-floating, and
		// its override record dies with this VM's description — there is no
		// carrier to move it to on this path.
		if lost := pve.DiskOptOverlayFromDesc(desc, entry.Volid); len(lost) > 0 {
			logger.Warn("delete_vm: recorded option overrides for this disk are lost with the VM -- re-apply them with update_disk after the next attach",
				log.String("slot", slot), log.String("volid", entry.Volid))
		}
		logger.Warn("delete_vm: persistent disk still attached on active slot -- detaching to preserve volume before destroy",
			log.String("slot", slot), log.String("volid", entry.Volid))
		detachErr := pve.RetryOnTransientOrLock(ctx, logger, "delete_vm.foreign_detach", 0, func() error {
			return deps.PVE.QEMU().DetachDisk(ctx, node, vmid, slot)
		})
		if detachErr != nil {
			return cpierrors.Retriable(
				"delete_vm: refusing to destroy VM %s -- could not detach persistent disk %s=%s to preserve it: %s (the volume would otherwise be destroyed; retry re-attempts detach)",
				vmCID, slot, entry.Volid, detachErr.Error())
		}
	}
	// Re-read config: a detach that silently no-ops (SDK regression / race) must
	// not let the destroy take the volume while it is still on an active slot. A
	// detach that demoted the disk to unusedN but could not sweep it (a snapshot
	// reference blocks the sweep) is caught by the guardUnusedVolumes pass that
	// follows this call on BOTH the sync and fast paths.
	confirmCfg, confErr := deps.PVE.QEMU().Config(ctx, node, vmid)
	if confErr != nil {
		if pve.IsNotFound(confErr) {
			return nil
		}
		return cpierrors.Wrap(pve.WrapError(confErr),
			fmt.Sprintf("delete_vm: re-read config for VM %s after foreign-disk detach", vmCID))
	}
	if remaining := pve.FindForeignActiveDisks(confirmCfg, vmid); len(remaining) > 0 {
		return cpierrors.Retriable(
			"delete_vm: refusing to destroy VM %s -- persistent disks still attached after detach attempt: %v (retry)",
			vmCID, remaining)
	}
	return nil
}

// ephemeralVolidPattern matches the volume name suffix created by attachEphemeralDisk:
// "vm-<vmid>-ephemeral-<n>". This is distinct from "vm-<n>-disk-<n>" (persistent disks)
// so EmbeddedDiskVMID does not match it; a dedicated check is required.
// The pattern anchors on "vm-" + digits + "-ephemeral-" to avoid false-positives
// on unrelated volume names.
const ephemeralVolidInfix = "-ephemeral-"

// detachRetainedEphemeralDisk checks whether the VM carries the tagRetainEphemeral
// tag and, when it does, finds every ephemeral disk slot (volid containing
// "vm-<vmid>-ephemeral-"), unlinks each with force=false (which demotes the disk to
// unusedN), then sweeps each unusedN config entry to remove the config reference
// without freeing storage.
//
// Returns (true, nil) whenever the tag is present — including when no active
// ephemeral slot remains (a prior attempt may already have unlinked+swept it, leaving
// the volume unreferenced with a matching VMID). Tag presence, not unlink success,
// gates the destroy flag so retried deletes and the straggler sweep stay safe. The
// caller MUST pass DestroyUnreferencedDisks=false to the subsequent DeleteQemu when
// retained==true:
//
//	DestroyUnreferencedDisks=true instructs PVE to free every volume that (a) is not
//	referenced in the config AND (b) has a VMID matching the VM being destroyed. After
//	the unlink+sweep sequence the ephemeral volume is unreferenced (config entry gone)
//	and has a matching VMID — so DestroyUnreferencedDisks=true would destroy it. Setting
//	the flag to false on the retain path is the only mechanism that keeps the volume.
//
//	Residual: on a retain-flagged VM, ANY other unreferenced own-VMID volumes (e.g. an
//	old orphan from a prior failed create) also survive. This is conservative by design;
//	scripts/disk-audit can inventory them.
//
// Returns (false, nil) when the tag is absent (byte-identical path: no extra config read,
// no API calls, DestroyUnreferencedDisks unchanged).
//
// Unlink mechanics:
//  1. UpdateQemuUnlink(force=false) → PVE moves the slot to unusedN; storage untouched.
//  2. Re-read VM config to find the resulting unusedN slot for this volid.
//  3. UpdateQemuConfig(Delete: "unusedN") → removes the config reference only; storage intact.
//  4. Caller sets DestroyUnreferencedDisks=false → DeleteQemu leaves the now-unreferenced
//     volume alone.
//
//nolint:gocognit // Multi-step unlink+sweep per ephemeral slot; each step individually simple.
func detachRetainedEphemeralDisk(
	ctx context.Context,
	deps Deps,
	node, vmCID string,
	vmid int,
	logger *log.Logger,
) (retained bool, err error) {
	// --- read VM config to check for tagRetainEphemeral ---
	vmCfg, cfgErr := deps.PVE.QEMU().Config(ctx, node, vmid)
	if cfgErr != nil {
		if pve.IsNotFound(cfgErr) {
			return false, nil
		}
		return false, cpierrors.Wrap(pve.WrapError(cfgErr),
			fmt.Sprintf("delete_vm: read config for VM %s to check retain-ephemeral tag", vmCID))
	}

	tagsRaw, _ := vmCfg[jsonKeyTags].(string)
	if !tagsContain(tagsRaw, tagRetainEphemeral) {
		return false, nil // flag not set — byte-identical path; no API calls
	}

	// Find ephemeral disk slots: active bus slots whose bare volid contains
	// "-ephemeral-" and whose embedded VMID matches the VM's own VMID.
	// EmbeddedDiskVMID matches "vm-<n>-disk-<n>" only; ephemeral uses a different
	// naming convention so a string-infix check is required.
	ephemeralSlots := findEphemeralActiveDisks(vmCfg, vmid)
	if len(ephemeralSlots) == 0 {
		// Tag present but no active ephemeral slot. Either a prior delete attempt
		// already unlinked+swept the disk (the volume now sits unreferenced with a
		// matching VMID — exactly what DestroyUnreferencedDisks=true would free),
		// or the VM never had an ephemeral disk. Return retained=true in both
		// cases: tag presence, not unlink success, must gate the destroy flag, or
		// a retried delete would destroy the volume the first attempt preserved.
		logger.Info("delete_vm: retain_ephemeral_on_delete set, no active ephemeral slot (already detached or none); forcing DestroyUnreferencedDisks=false",
			log.String("vmid", vmCID))
		return true, nil
	}

	slots := make([]string, 0, len(ephemeralSlots))
	for slot := range ephemeralSlots {
		slots = append(slots, slot)
	}
	sort.Strings(slots)

	for _, slot := range slots {
		volid := ephemeralSlots[slot]

		// Step 1: unlink with force=false → PVE demotes slot to unusedN, storage intact.
		unlinkErr := pve.RetryOnTransientOrLock(ctx, logger, "delete_vm.retain_ephemeral_unlink", 0, func() error {
			return deps.PVE.Nodes().UpdateQemuUnlink(ctx, node, vmCID, &sdknodes.UpdateQemuUnlinkParams{
				Idlist: slot,
			})
		})
		if unlinkErr != nil {
			return false, cpierrors.Retriable(
				"delete_vm: retain_ephemeral_on_delete: could not unlink ephemeral slot %s (volid=%s) on VM %s: %s (retry re-attempts)",
				slot, volid, vmCID, unlinkErr.Error())
		}

		// Step 2: re-read config to find the unusedN slot the unlink created.
		postUnlinkCfg, postErr := deps.PVE.QEMU().Config(ctx, node, vmid)
		if postErr != nil {
			if pve.IsNotFound(postErr) {
				return false, nil
			}
			return false, cpierrors.Wrap(pve.WrapError(postErr),
				fmt.Sprintf("delete_vm: retain_ephemeral_on_delete: re-read config after unlink for VM %s slot %s", vmCID, slot))
		}

		// Step 3: find the unusedN slot that now holds our volid and delete that
		// config entry (UpdateQemuConfig Delete removes only the config reference;
		// storage is left intact). After this sweep the volume is unreferenced.
		// The caller MUST then pass DestroyUnreferencedDisks=false — see function doc.
		unusedSlot := ""
		for unusedKey, unusedVolid := range pve.FindUnusedDiskEntries(postUnlinkCfg) {
			if unusedVolid == volid || strings.HasPrefix(unusedVolid, volid+",") {
				unusedSlot = unusedKey
				break
			}
		}

		if unusedSlot != "" {
			deleteKey := unusedSlot
			sweepErr := pve.RetryOnTransientOrLock(ctx, logger, "delete_vm.retain_ephemeral_sweep", 0, func() error {
				return deps.PVE.Nodes().UpdateQemuConfig(ctx, node, vmCID,
					&sdknodes.UpdateQemuConfigParams{Delete: &deleteKey})
			})
			if sweepErr != nil {
				return false, cpierrors.Retriable(
					"delete_vm: retain_ephemeral_on_delete: could not sweep unusedN entry %s (volid=%s) on VM %s: %s (retry re-attempts; volume is safe)",
					unusedSlot, volid, vmCID, sweepErr.Error())
			}
			logger.Warn("delete_vm: retain_ephemeral_on_delete: ephemeral disk unlinked and config ref swept; caller will set DestroyUnreferencedDisks=false to preserve volume",
				log.String("vmid", vmCID),
				log.String("slot", slot),
				log.String("volid", volid),
			)
			retained = true
		} else {
			// The unusedN entry may have already been swept (SDK auto-sweep).
			// Treat as retained: we cannot confirm the config ref is gone, so
			// use DestroyUnreferencedDisks=false as the conservative choice.
			logger.Warn("delete_vm: retain_ephemeral_on_delete: ephemeral disk unlinked; no unusedN entry found after unlink (may have been auto-swept); setting retained=true conservatively",
				log.String("vmid", vmCID),
				log.String("slot", slot),
				log.String("volid", volid),
			)
			retained = true
		}
	}
	return retained, nil
}

// findEphemeralActiveDisks returns every (slot -> bare volid) on an active bus slot
// of cfg whose bare volid contains ephemeralVolidInfix AND whose embedded storage
// VMID matches ownerVMID. Only own-VMID ephemeral volumes are returned; foreign-VMID
// volumes are left for detachForeignActiveDisks.
//
// Detection uses a string-infix check on the bare volid ("vm-<vmid>-ephemeral-<n>")
// because EmbeddedDiskVMID matches "vm-<n>-disk-<n>" only and would not match the
// ephemeral naming convention.
func findEphemeralActiveDisks(cfg map[string]any, ownerVMID int) map[string]string {
	out := make(map[string]string)
	prefix := fmt.Sprintf("vm-%d%s", ownerVMID, ephemeralVolidInfix)
	for slot, optstr := range sdkqemu.ParseDisks(cfg) {
		bare := optstr
		if comma := strings.Index(optstr, ","); comma >= 0 {
			bare = optstr[:comma]
		}
		// bare volid is "storage:vm-<vmid>-ephemeral-<n>"; strip storage prefix.
		volPart := bare
		if colon := strings.Index(bare, ":"); colon >= 0 {
			volPart = bare[colon+1:]
		}
		if strings.HasPrefix(volPart, prefix) {
			out[slot] = bare
		}
	}
	return out
}

// awaitDeleteTask extracts the UPID from the DeleteQemu response and awaits the
// destroy task. An empty or null response means PVE completed synchronously;
// NotFound or PmxcfsConfigMissing during the poll is treated as idempotent success.
// Returns nil on success or idempotent not-found. Returns a wrapped error otherwise.
func awaitDeleteTask(ctx context.Context, deps Deps, node, vmCID string, deleteResp *sdknodes.DeleteQemuResponse, logger *log.Logger) error {
	if deleteResp == nil {
		return nil
	}
	deleteUPID, upidErr := pve.UPIDFromRaw(*deleteResp)
	if upidErr != nil {
		// Malformed UPID is unexpected but non-fatal: the delete call
		// already succeeded; log and continue rather than fail the operation.
		logger.Warn("delete_vm: cannot parse UPID from delete response -- skipping await",
			log.Err(upidErr))
		return nil
	}
	if deleteUPID == "" {
		return nil
	}
	if awaitErr := pve.AwaitTaskWithLogger(ctx, deps.PVE, node, deleteUPID, logger); awaitErr != nil {
		// NotFound or PmxcfsConfigMissing during destroy-await means
		// the VM was already gone by the time we polled -- idempotent.
		if pve.IsNotFound(awaitErr) || pve.IsPmxcfsConfigMissing(awaitErr) {
			logger.Info("delete_vm: VM config missing during destroy await -- treating as already deleted")
			return nil
		}
		return cpierrors.Wrap(pve.WrapError(awaitErr),
			fmt.Sprintf("delete_vm: await destroy task for VM %s", vmCID))
	}
	return nil
}

// cleanupAgentForVM calls agent.Remove to purge registry/cloud-init state for
// the destroyed VM. Errors are non-fatal: the VM is already destroyed in PVE.
// The BOSH Director does not retry delete_vm on agent errors.
func cleanupAgentForVM(ctx context.Context, deps Deps, node string, vmid int, logger *log.Logger) {
	logger.Debug("delete_vm: calling agent.Remove")
	if agentErr := deps.Agent.Remove(ctx, node, vmid); agentErr != nil {
		logger.Warn("delete_vm: agent.Remove returned error (VM already destroyed)", log.Err(agentErr))
	}
}

// capturePoolForReap returns the VM's current pool membership for the
// post-destroy reaper, or "" when the reaper is disabled or the lookup fails.
// Only runs when pve.pool_reap_empty is enabled (the release default);
// best-effort: a lookup error just means the reaper no-ops after destroy.
func capturePoolForReap(ctx context.Context, deps Deps, vmid int, logger *log.Logger) string {
	if deps.Config == nil || !deps.Config.PoolReapEmpty {
		return ""
	}
	reapPool, _, poolErr := pve.FindVMPoolViaCluster(ctx, deps.PVE, vmid)
	if poolErr != nil {
		logger.Debug("delete_vm: pre-destroy pool lookup failed (non-fatal; reaper will no-op)", log.Err(poolErr))
		return ""
	}
	return reapPool
}

// reapEmptyPoolIfManaged deletes poolID when it is empty AND was created by
// this CPI (provenance comment per pve.IsCPIManagedPoolComment, which accepts
// the current prefix and the legacy pre-rename one), tolerating
// the two live PVE races this can hit. It is the delete_vm reaper for
// pve.pool_reap_empty (release default true; per-deployment pools go away
// with their deployments) and is called ONLY from the synchronous (non-fast-path)
// delete after the destroy task has been awaited, so PVE has already dropped
// the destroyed VM's pool membership by the time GetPoolComment/DeletePool run.
//
// No-ops (zero PVE calls) when:
//   - the reaper is disabled (deps.Config == nil or !PoolReapEmpty), or
//   - poolID == "" (the VM was not in any pool, or the pre-destroy lookup
//     failed and the caller already reset reapPool to ""), or
//   - deps.PVE.Pools() is nil (test fixtures / wiring gaps that never
//     configured a pool service), or
//   - poolID names the static vm_pool or the stemcell_template_pool (shared
//     long-lived pools, never reaped regardless of emptiness or comment).
//
// Otherwise:
//  1. GetPoolComment(poolID): a lookup error or a not-found pool both return
//     immediately (logged at debug) -- nothing to reap.
//  2. Comment prefix check: a pool whose comment fails
//     pve.IsCPIManagedPoolComment (neither the current nor the legacy
//     provenance prefix) is an operator's own pool and is NEVER deleted by
//     the CPI, regardless of emptiness.
//  3. DeletePool(poolID):
//     - nil error: the pool was empty and CPI-managed -- reaped, logged Info.
//     - pve.IsPoolNotEmpty / pve.IsPoolNotFound: PVE returns HTTP 500 + text
//     for both (never 404 -- IsPoolNotFound is the substring classifier,
//     not the generic 404-based IsNotFound), covering the two expected
//     races: another VM joined the pool between destroy and this call, or
//     a concurrent delete_vm/operator action already removed the pool.
//     Both are tolerated at debug -- not a failure.
//     - any other error: logged at Warn. Every branch is non-fatal: the
//     reaper must never fail delete_vm, which has already destroyed the VM
//     by the time this runs.
func reapEmptyPoolIfManaged(ctx context.Context, deps Deps, poolID string, logger *log.Logger) {
	if deps.Config == nil || !deps.Config.PoolReapEmpty || poolID == "" {
		return
	}
	if deps.PVE == nil || deps.PVE.Pools() == nil {
		return
	}
	// The static vm_pool and the stemcell template pool are long-lived shared
	// pools (create-if-missing at boot/first use), not per-deployment ones:
	// reaping either would churn create/delete on every last-VM delete and,
	// for the stemcell pool, momentarily drop the ACL boundary templates live
	// behind. Refuse both by name before any API call.
	if poolID == deps.Config.VMPool || poolID == deps.Config.StemcellTemplatePool {
		logger.Debug("delete_vm: reaper: pool is the static vm_pool or stemcell_template_pool; never reaped",
			log.String("pool", poolID))
		return
	}

	comment, found, err := deps.PVE.Pools().GetPoolComment(ctx, poolID)
	if err != nil {
		logger.Debug("delete_vm: reaper: GetPoolComment failed (non-fatal; not reaping)",
			log.String("pool", poolID), log.Err(err))
		return
	}
	if !found {
		logger.Debug("delete_vm: reaper: pool already gone before reap attempt",
			log.String("pool", poolID))
		return
	}
	if !pve.IsCPIManagedPoolComment(comment) {
		logger.Debug("delete_vm: reaper: pool not CPI-managed, not reaping",
			log.String("pool", poolID), log.String("comment", comment))
		return
	}

	// Pool deletes serialize on PVE's cluster-wide user_cfg lock, so a lock
	// timeout under concurrent deploys is contention, not a verdict; ride
	// RetryOnTransientOrLock instead of leaking the empty pool on the first
	// timeout. The two resolved verdicts (not empty, already gone) are
	// short-circuited to nil inside the closure so the retry budget is never
	// spent on them; the switch below still sees them via deleteErr. The
	// budget is the small sweep budget, not the full lock curve: this runs on
	// the request path after the destroy already succeeded, every branch
	// below is swallowed, and a leaked empty pool is cosmetic, so minutes of
	// backoff here would only slow delete_vm down.
	var deleteErr error
	_ = pve.RetryOnTransientOrLock(ctx, logger, "delete_vm.pool_reap", cleanupSweepMaxAttempts, func() error {
		deleteErr = deps.PVE.Pools().DeletePool(ctx, poolID)
		if deleteErr != nil && (pve.IsPoolNotEmpty(deleteErr) || pve.IsPoolNotFound(deleteErr)) {
			return nil
		}
		return deleteErr
	})
	switch {
	case deleteErr == nil:
		logger.Info("delete_vm: reaper: reaped empty pool", log.String("pool", poolID))
	case pve.IsPoolNotEmpty(deleteErr) || pve.IsPoolNotFound(deleteErr):
		logger.Debug("delete_vm: reaper: pool not reaped -- still has members or already gone (race); tolerated",
			log.String("pool", poolID), log.Err(deleteErr))
	default:
		logger.Warn("delete_vm: reaper: empty-pool reap failed (non-fatal)",
			log.String("pool", poolID), log.Err(deleteErr))
	}
}
