package handlers

import (
	"context"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// poolAccessProbeTimeout bounds each pool-visibility probe issued by
// ensurePoolAccess so a hung or slow PVE API call cannot stall a request
// indefinitely. A probe that does not return in time is classified as
// transient (Warn-only, the request proceeds) rather than blocking forever.
const poolAccessProbeTimeout = 10 * time.Second

// ensurePoolAccess probes PVE resource-pool visibility for every statically
// named pool layer (pve.vm_pool, pve.stemcell_template_pool). create_vm and
// create_stemcell call it before they mutate anything, against deps.PVE and
// deps.Config, which WithRequestOverrides has already rebound to the cluster
// the request targets. The probe therefore never touches the job-level
// cluster for a request routed elsewhere, and a denied job credential cannot
// stop a request for another cluster.
//
// Both pools default on ("bosh" and "bosh-templates" per jobs/pve_cpi/spec),
// so this runs for a zero-config deployment unless the operator explicitly
// opts both out. The pve.vm_pool_template layer cannot be probed here, because
// its names are rendered per create_vm call and do not exist until first use.
// A missing /pool grant for those surfaces as a named, non-retriable create_vm
// error (see ensureResolvedPool) instead.
//
// When the "parked" detached-disk strategy is active
// (DetachedDiskParkedEnabled) and the parker pool resolves to a non-empty
// name, that pool is probed too, deduped against the other two. Unlike the
// two pools above, a denial on the parker pool only warns, because
// parker-pool assignment is a best-effort sweep and should not fail a
// create_vm that the grant gap does not affect.
//
// Design: the cheapest side-effect-free signal that proves the CPI's identity
// can read a pool path is GET /pools/{poolid} (pve.PoolService.GetPoolComment)
// issued against each configured pool name. It exercises exactly the
// Pool.Audit grant PVE checks per pool, without creating or mutating
// anything. There is no side-effect-free way to probe Pool.Allocate, so a
// clean probe proves read access only; the failure message still names both
// grants so an operator fixes both at once.
//
// A pool that does not exist yet is not a failure. GetPoolComment maps both
// PVE not-found shapes, a 404 and the 500 with "pool 'x' does not exist", to
// (found=false, err=nil), and the CPI creates the pool lazily on first use.
// Only a classified permission error (pve.IsPoolPermissionDenied, HTTP
// 401 or 403) fails the request, as a non-retriable CloudError. Every other
// error (network fault, PVE 5xx, context deadline) is logged at Warn and
// treated as transient, because the same fault resurfaces on the first real
// pool-touching call, where the normal per-request retry path takes over.
//
// Skipped entirely, with zero PVE calls, when pve.vm_pool,
// pve.stemcell_template_pool, and the parker pool all resolve to "". Also a
// no-op when the config, client, or client.Pools() is nil.
func ensurePoolAccess(ctx context.Context, deps Deps) error {
	cfg, client := deps.Config, deps.PVE
	if cfg == nil || client == nil || client.Pools() == nil {
		return nil
	}
	logger := deps.Log(ctx)

	parkerPool := ""
	if cfg.DetachedDiskParkedEnabled() {
		parkerPool = cfg.ParkerPoolValue()
	}
	if cfg.VMPool == "" && cfg.StemcellTemplatePool == "" && parkerPool == "" {
		return nil
	}

	// failFast marks pve.vm_pool and pve.stemcell_template_pool. Every
	// create_vm/create_stemcell needs them, so a permission denial on either
	// one fails the request. The parker pool is best-effort (see the doc comment
	// above), so it carries failFast=false and only ever warns.
	type poolProbe struct {
		poolID   string
		failFast bool
	}
	candidates := []poolProbe{
		{cfg.VMPool, true},
		{cfg.StemcellTemplatePool, true},
		{parkerPool, false},
	}

	pools := make([]poolProbe, 0, 3)
	seen := make(map[string]bool, 3)
	for _, c := range candidates {
		if c.poolID == "" || seen[c.poolID] {
			continue
		}
		seen[c.poolID] = true
		pools = append(pools, c)
	}

	for _, p := range pools {
		probeCtx, cancel := context.WithTimeout(ctx, poolAccessProbeTimeout)
		_, found, err := client.Pools().GetPoolComment(probeCtx, p.poolID)
		cancel()
		if err == nil {
			if !found {
				logger.Debug("pools preflight: pool does not exist yet; it will be created on first use",
					log.String("pool", p.poolID))
				continue
			}
			logger.Debug("pools preflight: pool visible", log.String("pool", p.poolID))
			continue
		}
		if pve.IsPoolPermissionDenied(err) {
			if !p.failFast {
				logger.Warn("pools preflight: PVE denied read access to the parker pool "+
					"(non-fatal; parker-pool assignment is best effort, request continuing)",
					log.String("pool", p.poolID), log.Err(err))
				continue
			}
			return cpierrors.Cloud(
				"pools preflight: PVE denied read access to pool %q: %s -- grant the CPI's "+
					"identity Pool.Audit AND Pool.Allocate on /pool/%s (Pool.Allocate cannot be "+
					"probed here without a mutating call, but is required by create_vm and "+
					"create_stemcell the first time either assigns a VM to this pool); "+
					"alternatively, set pve.vm_pool: \"\" and pve.stemcell_template_pool: \"\" "+
					"to disable pool assignment entirely",
				p.poolID, err.Error(), p.poolID)
		}
		logger.Warn("pools preflight: could not confirm pool access (non-fatal; treating as transient, request continuing)",
			log.String("pool", p.poolID), log.Err(err))
	}
	return nil
}
