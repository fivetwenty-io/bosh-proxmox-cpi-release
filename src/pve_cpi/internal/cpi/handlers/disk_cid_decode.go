package handlers

import (
	"context"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// decodeDiskCID decodes an encoded disk CID on behalf of a disk handler,
// logging the codec's precise rejection reason before mapping the failure to
// DiskNotFound. method is the CPI method name, used to prefix the log message.
//
// Every disk handler shares this rather than inlining its own
// pve.ParseEncodedDiskCID + DiskNotFound pair, so the diagnostic cannot drift
// between them or be forgotten by the next handler added.
//
// Why the error type stays DiskNotFound: a CID this CPI could not have emitted
// names a disk this CPI does not have, and DiskNotFound is the error the
// Director knows how to act on for orphan cleanup. Returning a CloudError
// instead would change Director behavior for a case that is genuinely
// "we don't have that disk".
//
// Why the reason is logged rather than folded into the error message: the
// Director surfaces the CPI error text into task output, and DiskNotFound's
// message is matched by tooling; the codec's reason (wrong prefix, bad
// base64url, bad gzip, oversized inflation, empty volid) belongs in the CPI's
// own log where an operator debugging a "disk not found" on a volume that is
// visibly present on PVE will look next. Without it that operator has no way
// to learn the CID never decoded — the exact confusion the V8 live run hit.
func decodeDiskCID(ctx context.Context, deps Deps, method, diskCID string) (bareCID string, meta *pve.DiskCIDMeta, err error) {
	bareCID, meta, decErr := pve.ParseEncodedDiskCID(diskCID)
	if decErr != nil {
		deps.Log(ctx).Warn(method+": disk CID did not decode as a CPI-issued envelope; reporting the disk as not found "+
			"(a CID this CPI never emitted names no disk it owns) — if the volume is visibly present on PVE, "+
			"the CID itself is the problem, not the volume",
			log.String("disk_cid", diskCID),
			log.Err(decErr),
		)
		return "", nil, cpierrors.DiskNotFound(diskCID)
	}
	return bareCID, meta, nil
}
