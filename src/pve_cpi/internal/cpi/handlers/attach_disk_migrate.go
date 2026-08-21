// Cross-node persistent-disk migration glue (A2): the handler side of the
// mover flow pve.MigrateDiskViaMover implements. guardAndUnparkBeforeAttach
// routes here when pve.disk_migration resolves to "on_attach" and the disk's
// holder sits on a different node than the target VM with no safe config-edit
// path (a parker-named volume on any backend, any volume on a node-local
// backend, or a mover left by an interrupted migration).
package handlers

import (
	"context"
	"fmt"
	"time"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// diskBackendIsLocal reports whether the storage pool named in volid resolves
// to a node-local backend. The answer decides whether the migration must copy
// volume data (WithLocalDisks) or is a pure metadata move, and whether a
// birth-named volume needs the mover flow at all.
func diskBackendIsLocal(ctx context.Context, deps Deps, op, volid string) (bool, error) {
	storage, _, parseErr := pve.ParseDiskCID(volid)
	if parseErr != nil {
		return false, cpierrors.DiskNotFound(volid)
	}
	backend, err := backendResolverOrDefault(deps).Resolve(ctx, storage)
	if err != nil {
		return false, cpierrors.Wrap(err, fmt.Sprintf("%s: backend resolution failed for storage %q", op, storage))
	}
	return backend.Kind() == pve.BackendLocal, nil
}

// attachViaMigration moves a cross-node parked disk to the target VM's node
// through the mover flow, then hands back the ordinary same-node reassignment
// plan: the disk ends the call parked on a single-purpose mover ON the target
// node, attachDiskViaTransfer moves it onto the VM, and the now-empty mover
// is destroyed. rd is updated in place with the renamed volid and the mover
// as holder, so everything downstream operates on the disk's current name.
//
// The migrate-task await runs under the retry.disk_migrate budget; when the
// budget runs out while PVE is still copying, the error is retriable and the
// Director's retried attach re-enters the flow (the mover is adopted wherever
// the crash or timeout left it).
func attachViaMigration(
	ctx context.Context,
	deps Deps,
	op string,
	rd *resolvedDisk,
	holder pve.DiskHolder,
	node string,
	overlay map[string]string,
	localBacked bool,
) (attachPlan, error) {
	deps.Log(ctx).Info(op+": disk and VM are on different nodes; migrating the disk via a single-purpose mover",
		log.String("disk_cid", rd.diskCID),
		log.String("volid", rd.volid),
		log.Int("holder_vmid", holder.VMID),
		log.String("disk_node", holder.Node),
		log.String("vm_node", node),
		log.Bool("local_storage", localBacked),
	)

	pol := deps.Config.RetryDiskMigrate()
	spec := pve.DiskMigrationSpec{
		Holder:      holder,
		TargetNode:  node,
		Volid:       rd.volid,
		StableID:    rd.stableID,
		DiskCID:     rd.diskCID,
		SourceLocal: localBacked,
		Opts:        overlay,
		MaxAttempts: pol.MaxAttempts,
		AwaitBudget: time.Duration(pol.CapMs) * time.Millisecond,
	}
	// The write config: mover allocation scans disk_storage volume content
	// like every other parker-band allocation (see parkerReadConfigFor's doc
	// comment for why the read config must not be used here).
	mover, landed, err := pve.MigrateDiskViaMover(ctx, deps.PVE, deps.Log(ctx), spec, parkerWriteConfigFor(deps))
	if err != nil {
		return attachPlan{}, retriableUnlessPermanent(err,
			fmt.Sprintf("%s: migrate disk %s from node %s to node %s", op, rd.diskCID, holder.Node, node))
	}

	rd.volid = landed
	rd.holder = &mover
	return attachPlan{viaTransfer: true, parker: mover, overlay: overlay, destroyMover: true}, nil
}
