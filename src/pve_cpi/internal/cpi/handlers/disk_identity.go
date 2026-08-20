// The disk-identity seam (D13): every disk handler decodes the Director's
// CID, then resolves it HERE before any storage-level call. PVE's move_disk
// reassignment renames a volume to match its new owner, so the envelope
// volid is only a birth record for stable-ID disks; the volid the cluster
// currently knows the volume by comes out of the identity resolution, and
// everything downstream — backend node lookups, config scans, storage
// deletes — operates on that.
package handlers

import (
	"context"
	"fmt"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// resolvedDisk is one disk's decoded and resolved identity, carried through a
// handler in place of the raw (bareDiskCID, meta) pair.
type resolvedDisk struct {
	// diskCID is the Director's verbatim encoded CID.
	diskCID string
	// birth is the envelope volid — the volume's name at create_disk time.
	birth string
	// volid is the volume's current name: what the identity scan found on a
	// drive entry, the provenance-recorded name for a mid-transfer disk, or
	// the birth name when nothing in the cluster references the disk. For
	// legacy disks this is always the birth name.
	volid string
	// meta is the decoded CID metadata (nil for meta-less envelopes).
	meta *pve.DiskCIDMeta
	// stableID is meta.ID; empty for legacy disks.
	stableID string
	// holder is the VM the identity scan found referencing the volume.
	// Nil for legacy disks (their handlers keep their own scan patterns) and
	// for stable-ID disks nothing references.
	holder *pve.DiskHolder
	// intent is non-nil when the disk is mid detach-transfer: only a parker's
	// provenance intent record names it. Mutating handlers resume the
	// transfer before acting; read handlers treat the disk as existing.
	intent *pve.DiskTransferIntent
}

// sentinelKey is the key this disk's records live under in the description
// sentinels (bosh_attached_disks, bosh_disk_metadata): the stable ID when the
// disk has one, the (birth) volid otherwise — exactly the dual keying the
// sentinel readers tolerate.
func (rd resolvedDisk) sentinelKey() string {
	if rd.stableID != "" {
		return rd.stableID
	}
	return rd.birth
}

// resolveDiskForOp resolves a decoded disk CID to its current volid and
// holder. Legacy CIDs (no stable ID) return immediately with the birth volid
// and no API calls — their handlers behave byte-identically to before stable
// IDs existed. Stable-ID CIDs pay one cluster scan (plus a parker provenance
// sweep only when nothing references the volume).
func resolveDiskForOp(ctx context.Context, deps Deps, op, diskCID, bareDiskCID string, meta *pve.DiskCIDMeta) (resolvedDisk, error) {
	rd := resolvedDisk{diskCID: diskCID, birth: bareDiskCID, volid: bareDiskCID, meta: meta}
	if meta != nil {
		rd.stableID = meta.ID
	}
	if rd.stableID == "" || deps.Config == nil {
		return rd, nil
	}
	ident, err := pve.ResolveDiskIdentity(ctx, deps.PVE, deps.Log(ctx), bareDiskCID, rd.stableID, parkerReadConfigFor(deps))
	if err != nil {
		return resolvedDisk{}, wrapHolderScanError(err, fmt.Sprintf("%s: resolve disk identity for %s", op, diskCID))
	}
	rd.volid = ident.Volid
	if ident.Holder.Found {
		h := ident.Holder
		rd.holder = &h
	}
	rd.intent = ident.Intent
	return rd, nil
}

// resumeTransferIfNeeded converges a mid-transfer disk to its parked state
// before a mutating handler acts on it, then re-resolves so the caller works
// from the converged state. A no-op for disks not mid-transfer.
func resumeTransferIfNeeded(ctx context.Context, deps Deps, op string, rd resolvedDisk) (resolvedDisk, error) {
	if rd.intent == nil {
		return rd, nil
	}
	deps.Log(ctx).Warn(op+": disk has an interrupted transfer to its parker; resuming it first",
		log.String("disk_cid", rd.diskCID),
		log.Int("parker_vmid", rd.intent.ParkerVMID),
	)
	pctx := pve.ParkContext{DiskCID: rd.diskCID, SourceVMCID: rd.intent.SourceVMCID, StableID: rd.stableID, Opts: rd.intent.Opts}
	if _, err := pve.ResumeDiskTransferToParker(ctx, deps.PVE, deps.Log(ctx), *rd.intent, rd.stableID, parkerWriteConfigFor(deps), pctx); err != nil {
		return resolvedDisk{}, retriableUnlessPermanent(err,
			fmt.Sprintf("%s: resume interrupted transfer for disk %s", op, rd.diskCID))
	}
	return resolveDiskForOp(ctx, deps, op, rd.diskCID, rd.birth, rd.meta)
}

// parkerWriteConfigFor is parkerReadConfigFor plus the park-only fields a
// path that ATTACHES disks to parkers (park, transfer, resume) must set —
// see parkerReadConfigFor's doc comment for why the read helper leaves
// DiskStorage empty on purpose.
func parkerWriteConfigFor(deps Deps) pve.ParkerConfig {
	cfg := parkerReadConfigFor(deps)
	cfg.DiskStorage = deps.Config.DiskStorage
	return cfg
}
