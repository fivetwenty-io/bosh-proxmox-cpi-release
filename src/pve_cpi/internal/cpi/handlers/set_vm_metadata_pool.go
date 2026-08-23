// set_vm_metadata_pool.go — pool-membership reconciliation run at the tail of
// set_vm_metadata, under the same per-VMID cluster lock as the metadata write.
//
// The director-level pve.vm_pool_template renders a VM's pool from
// {director}/{deployment}/{instance_group} tokens. When the template (or a
// token's value) changes after a VM was created, the VM's actual pool drifts
// from what the template would render today. set_vm_metadata is the one CPI
// call the Director re-issues on every deploy for every VM, so it is the
// natural reconcile point: re-render from the PERSISTED create-time tokens
// (the bosh_pool sentinel — never from live metadata, so the render cannot
// oscillate between two token sources) and move the VM when the rendered
// name differs from its current pool.
//
// Scope rules, in order:
//
//   - cfg.VMPoolTemplate == "": the template layer is off; nothing is ever
//     reconciled or adopted.
//   - Sentinel present, layer != "template": the pool was a call-level,
//     vm_type, or static choice — an explicit decision the CPI never
//     overrides. Untouched.
//   - Sentinel present, layer == "template": re-render from the sentinel's
//     tokens; ensure + move when the render differs from the current pool.
//   - No sentinel (pre-provenance legacy VM): adopt ONLY when the VM's
//     current pool equals cfg.VMPool (the static default a pre-flip release
//     put it in); derive tokens from the incoming metadata map, move, and
//     write the sentinel so every later pass uses persisted inputs. A legacy
//     VM in any other pool is untouched.
//
// Every failure here is Warn-level and never fails set_vm_metadata — pool
// placement is advisory next to the metadata write the Director asked for.
package handlers

import (
	"context"
	"fmt"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// reconcileVMPoolMembership is the entry point described in the file header.
// Callers must hold the per-VMID cluster lock (HandleSetVMMetadata's
// withVMIDLock closure) so a concurrent set_vm_metadata cannot interleave
// its own read-render-move sequence with this one.
func reconcileVMPoolMembership(ctx context.Context, deps Deps, node string, vmid int, metadata map[string]any, logger *log.Logger) {
	cfg := deps.Config
	if cfg == nil || cfg.VMPoolTemplate == "" {
		return
	}
	if deps.PVE == nil || deps.PVE.Pools() == nil {
		return
	}

	vmCfg, cfgErr := deps.PVE.QEMU().Config(ctx, node, vmid)
	if cfgErr != nil {
		logger.Warn("set_vm_metadata: pool reconcile: could not read VM config; skipping", log.Err(cfgErr))
		return
	}
	pm, hasSentinel := pve.GetPoolMembership(pve.DescriptionFromConfig(vmCfg))

	currentPool, found, poolErr := pve.FindVMPoolViaCluster(ctx, deps.PVE, vmid)
	if poolErr != nil {
		logger.Warn("set_vm_metadata: pool reconcile: pool lookup failed; skipping", log.Err(poolErr))
		return
	}
	if !found {
		// The membership lookup reads the /cluster/resources index, which
		// lags node-local state by minutes, and set_vm_metadata runs seconds
		// after create_vm: a young VM is the common case here, not an
		// anomaly. The VM itself is proven present (its config was just read
		// above), so a silent return would leave exactly the newest VMs
		// unreconciled. Proceed with an unknown current pool for the
		// template-layer converge only: it moves to the desired pool
		// regardless (MoveVMToPool tolerates the VM already being there).
		// Legacy adoption gets the found flag and skips, because it requires
		// an actual reading proving the VM sits in the static pool; an
		// unknown pool must not satisfy that gate (with vm_pool unset the
		// gate would otherwise compare empty to empty and adopt blind).
		logger.Debug("set_vm_metadata: pool reconcile: VM not in cluster index yet; treating current pool as unknown")
		currentPool = ""
	}

	if hasSentinel {
		reconcileTemplateLayerVM(ctx, deps, node, vmid, pm, currentPool, logger)
		return
	}
	if !found {
		logger.Debug("set_vm_metadata: pool adopt: current pool unknown (index lag); adoption needs a proven static-pool reading, skipping")
		return
	}
	adoptLegacyVM(ctx, deps, node, vmid, metadata, currentPool, logger)
}

// reconcileTemplateLayerVM re-renders the template from pm's persisted tokens
// and converges the VM's pool to the result. VMs whose persisted layer is not
// "template" are never touched.
func reconcileTemplateLayerVM(ctx context.Context, deps Deps, node string, vmid int, pm *pve.PoolMembership, currentPool string, logger *log.Logger) {
	if pm.Layer != pve.PoolLayerTemplate {
		return
	}

	desired := renderPoolTemplateTokens(deps.Config, pm.Director, pm.Deployment, pm.InstanceGroup)
	if desired == "" {
		// Every persisted token substituted empty — resolution would fall to
		// the static layer, which is not this VM's persisted layer. Leave it.
		logger.Debug("set_vm_metadata: pool reconcile: template render collapsed to empty; skipping")
		return
	}
	if _, err := validateResolvedPoolName(deps.Config, desired); err != nil {
		logger.Warn("set_vm_metadata: pool reconcile: re-rendered pool name invalid; skipping", log.Err(err))
		return
	}

	if desired == currentPool {
		if pm.Name != desired {
			writePoolSentinel(ctx, deps, node, vmid, desired, pm.Director, pm.Deployment, pm.InstanceGroup, logger)
		}
		return
	}

	if !movePoolMember(ctx, deps, vmid, desired, currentPool, pm.Director, logger) {
		return
	}
	writePoolSentinel(ctx, deps, node, vmid, desired, pm.Director, pm.Deployment, pm.InstanceGroup, logger)
}

// adoptLegacyVM handles a VM with no bosh_pool sentinel — created before the
// CPI persisted pool provenance. Adoption is deliberately narrow: only a VM
// sitting in the static cfg.VMPool (where a pre-flip release put every VM)
// is treated as template-layer, with tokens taken from the incoming metadata
// map (the only source a legacy VM has). Anything in any other pool — an
// operator's own pool, a call-level pool the old release honored — stays
// untouched. The sentinel is written after the move so the derivation is
// persisted and every later pass runs on stored inputs.
func adoptLegacyVM(ctx context.Context, deps Deps, node string, vmid int, metadata map[string]any, currentPool string, logger *log.Logger) {
	if currentPool != deps.Config.VMPool {
		return
	}

	director := metadataString(metadata, "director")
	deployment := metadataString(metadata, "deployment")
	instanceGroup := metadataString(metadata, "instance_group")
	if instanceGroup == "" {
		instanceGroup = metadataString(metadata, "job")
	}

	desired := renderPoolTemplateTokens(deps.Config, director, deployment, instanceGroup)
	if desired == "" {
		return
	}
	if _, err := validateResolvedPoolName(deps.Config, desired); err != nil {
		logger.Warn("set_vm_metadata: pool adopt: rendered pool name invalid; skipping", log.Err(err))
		return
	}

	if desired != currentPool {
		if !movePoolMember(ctx, deps, vmid, desired, currentPool, director, logger) {
			return
		}
	}
	writePoolSentinel(ctx, deps, node, vmid, desired, director, deployment, instanceGroup, logger)
}

// movePoolMember ensures the target pool exists (with provenance comment)
// and moves the VM into it. Returns false when either step failed (already
// logged at Warn); the caller then skips the sentinel write so the recorded
// name never claims a membership the move did not achieve.
func movePoolMember(ctx context.Context, deps Deps, vmid int, desired, currentPool, director string, logger *log.Logger) bool {
	if err := pve.EnsurePoolExists(ctx, deps.PVE, desired, pve.PoolProvenance(director), logger); err != nil {
		logger.Warn("set_vm_metadata: pool reconcile: could not ensure target pool; skipping",
			log.String("pool", desired), log.Err(err))
		return false
	}
	// The move serializes on PVE's cluster-wide user_cfg lock; ride
	// RetryOnTransientOrLock so concurrent deploys contending on that lock do
	// not make this reconcile give up (and skip the sentinel write) on the
	// first timeout. A success on any attempt returns nil here, so the caller
	// still writes the sentinel after a success-after-retry. The budget is
	// the small sweep budget: this reconcile is best-effort on the request
	// path, and its failure only skips the sentinel write, so the full lock
	// curve would trade minutes of set_vm_metadata latency for a retry the
	// next reconcile gets anyway.
	if err := pve.RetryOnTransientOrLock(ctx, logger, "set_vm_metadata.pool_move", cleanupSweepMaxAttempts, func() error {
		return deps.PVE.Pools().MoveVMToPool(ctx, desired, int64(vmid))
	}); err != nil {
		logger.Warn("set_vm_metadata: pool reconcile: move failed; skipping",
			log.String("pool", desired), log.Err(err))
		return false
	}
	logger.Info("set_vm_metadata: pool reconcile: VM moved to template-rendered pool",
		log.String("from", currentPool), log.String("pool", desired))
	return true
}

// writePoolSentinel persists the (re)derived pool record. Best-effort — the
// underlying write logs its own Warn on failure.
func writePoolSentinel(ctx context.Context, deps Deps, node string, vmid int, name, director, deployment, instanceGroup string, logger *log.Logger) {
	pve.UpdatePoolMembership(ctx, deps.PVE, logger, node, vmid, &pve.PoolMembership{
		Name:          name,
		Layer:         pve.PoolLayerTemplate,
		Director:      director,
		Deployment:    deployment,
		InstanceGroup: instanceGroup,
	})
}

// metadataString extracts metadata[key] as a string, rendering non-string
// scalars via fmt (matching buildBoshManagedTags' tolerance). Missing or nil
// values yield "".
func metadataString(metadata map[string]any, key string) string {
	v, ok := metadata[key]
	if !ok || v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}
