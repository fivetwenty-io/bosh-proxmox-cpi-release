// Parker pool placement. A parker VM joins its resource pool after it exists,
// from a sweep the handler runs once a park has succeeded, rather than from
// inside the park itself. parker.go creates parkers from four places and only
// one of them is EnsureParker, so a placement inside the park path would reach
// the first parker a node ever gets and nothing after it.
package pve

import (
	"context"
	"errors"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// parkerPoolSweepTimeout bounds the whole placement sweep, covering the pool
// ensure, every membership read, and every assignment together.
//
// EnsurePoolExists rides the full storage-lock attempt budget on purpose,
// because every pool mutation cluster-wide serializes on the single pmxcfs
// user_cfg lock and a burst deploy contends there by design. That budget is
// right for create_vm, where the pool is part of the outcome. It is wrong
// here, where the pool is cosmetic and a detach is waiting, so the sweep
// spends this instead. The name and the shape match the poolsPreflightTimeout
// convention in cmd/cpi, so the number stays visible rather than buried.
const parkerPoolSweepTimeout = 10 * time.Second

// PlaceParkersInPool puts cfg.Pool's missing parkers on node into it, creating
// that pool if it does not exist yet. Its candidates come from
// ListParkersForNode, so they are the parkers in cfg's band that this
// director may adopt, and a parker another director's tag attributes
// elsewhere is not one of them. Handlers call it once a park or a transfer has
// returned success, and they discard its error after logging; it returns one
// only so tests can read the failures.
//
// The sweep is best-effort, and that is a decision rather than a shortcut. A
// parker outside its pool is a fully valid, adoptable parker, so a failure here
// costs an operator a grouping in the PVE UI. A failed park, by contrast, is a
// failed detach that the Director retries forever. Promoting any of this to
// fatal trades a real outcome for a cosmetic one, so every failure below logs
// and the sweep carries on to the next parker.
//
// Membership comes from PoolHasVM, which reads the pmxcfs-backed pool object,
// and never from /cluster/resources or FindVMPoolViaCluster. That index lags by
// minutes, so the parker this park just created is the one most likely to be
// missing from it, and it is also the single most important parker in the
// sweep.
//
// A parker that turns out to belong to some other pool is left where it is.
// MoveVMToPool would reconcile it, because it passes allow-move=1, and we
// deliberately do not call it. Moving a parker out of a pool that an operator
// or another cpi-config entry put it in is not a cosmetic sweep's decision to
// make, so renaming pve.parker_pool strands the existing parkers in the old
// pool until somebody moves them by hand.
//
// On the managed create_disk path the sweep runs after the allocation journal
// window has closed, because no call for our pool may run inside a managed
// allocation guard. A parker left unpooled there leaves no journal record, so
// reconciliation will never notice one. That follows from the guard rule rather
// than from a defect, and it is acceptable exactly because pool membership is
// cosmetic.
//
// The listing here repeats the one parkDiskOnNode made seconds ago. It is a
// single node-local call covered by the bound above, and passing the park's
// parker list through would cost this function its self-contained signature for
// very little in return.
func PlaceParkersInPool(ctx context.Context, c Client, logger *log.Logger, node string, cfg ParkerConfig) error {
	if cfg.Pool == "" {
		// The documented pve.parker_pool: "" opt-out. Nothing reaches PVE.
		return nil
	}
	if ctx == nil {
		return cpierrors.Cloud("PlaceParkersInPool: ctx must not be nil")
	}
	if c == nil {
		return cpierrors.Cloud("PlaceParkersInPool: PVE client must not be nil")
	}
	if c.Pools() == nil {
		// AssignVMToPool dereferences c.Pools() unguarded, and a client
		// without a pool service is a CPI that cannot place anything anyway.
		if logger != nil {
			logger.Debug("parker pool placement skipped because this client has no pool service",
				log.String("pool", cfg.Pool),
				log.String("node", node),
			)
		}
		return nil
	}

	sweepCtx, cancel := context.WithTimeout(ctx, parkerPoolSweepTimeout)
	defer cancel()

	var failures []error
	ensureFailed := false
	if ensureErr := EnsurePoolExists(sweepCtx, c, cfg.Pool, PoolProvenance(cfg.DirectorID), logger); ensureErr != nil {
		// A pool we could not create may already exist, so the assignments
		// below are still worth attempting.
		failures = append(failures, ensureErr)
		ensureFailed = true
		if logger != nil {
			logger.Warn("could not ensure the parker pool exists; the parkers on this node stay unpooled unless the pool is already there",
				log.String("pool", cfg.Pool),
				log.String("node", node),
				log.Err(ensureErr),
			)
		}
	}

	parkers, listErr := ListParkersForNode(sweepCtx, c, node, cfg)
	if listErr != nil {
		if logger != nil {
			logger.Warn("could not list this node's parkers, so none of them were placed in the parker pool",
				log.String("pool", cfg.Pool),
				log.String("node", node),
				log.Err(listErr),
			)
		}
		return errors.Join(append(failures, listErr)...)
	}

	for _, vmid := range parkers {
		member, probeErr := c.Pools().PoolHasVM(sweepCtx, cfg.Pool, int64(vmid))
		if probeErr != nil {
			failures = append(failures, probeErr)
			if logger != nil {
				logger.Warn("could not read whether this parker already belongs to the parker pool, so it was left alone",
					log.String("pool", cfg.Pool),
					log.String("node", node),
					log.Int("parker_vmid", vmid),
					log.Err(probeErr),
				)
			}
			continue
		}
		if member {
			continue
		}
		assignErr := AssignVMToPool(sweepCtx, c, cfg.Pool, int64(vmid), logger)
		if assignErr == nil {
			continue
		}
		if errors.Is(assignErr, ErrVMInAnotherPool) {
			if logger != nil {
				logger.Debug("this parker belongs to another pool and stays there, because moving it is not this sweep's decision to make",
					log.String("pool", cfg.Pool),
					log.String("node", node),
					log.Int("parker_vmid", vmid),
					log.Err(assignErr),
				)
			}
			continue
		}
		failures = append(failures, assignErr)
		if logger != nil {
			fields := []log.Field{
				log.String("pool", cfg.Pool),
				log.String("node", node),
				log.Int("parker_vmid", vmid),
				log.Err(assignErr),
			}
			if ensureFailed {
				// The pool ensure above already warned once for this sweep,
				// and a missing Pool.Allocate grant fails every assignment
				// after it for that same reason, so these lines stay at debug
				// rather than repeating one warning per parker.
				logger.Debug("could not place this parker in the parker pool, for the reason the pool ensure above already reported", fields...)
			} else {
				logger.Warn("could not place this parker in the parker pool; it stays where it is and the park itself is unaffected", fields...)
			}
		}
	}
	return errors.Join(failures...)
}
