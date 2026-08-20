// Stable disk identity (D13): a drive serial= token is the disk's identity,
// the envelope volid is a birth record. PVE's move_disk reassignment renames
// a volume to match its new owner, so any consumer that resolves a disk by
// its birth volid alone stops finding it after the first ownership transfer.
// The token rides the drive entry as serial=<token>, written only at attach
// boundaries (a mid-life serial edit on a running VM diverges silently from
// the live device until the next full restart — live-spike result).
package pve

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"

	cpierrors "github.com/fivetwenty-io/bosh-pve-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
)

// DiskStableIDPrefix marks a drive serial as a CPI stable disk identity. No
// other producer writes bpd- serials, so the prefix is authoritative: a drive
// entry carrying one is a CPI persistent disk regardless of what its volume
// name says.
const DiskStableIDPrefix = "bpd-"

// DiskStableIDLen is the exact stable-ID length: the prefix plus 16 lowercase
// hex characters — 20 bytes, which is PVE's drive-serial cap. Enforced at
// generation and validated on CID decode.
const DiskStableIDLen = 20

// GenerateDiskStableID returns a fresh stable disk identity token:
// "bpd-" + 16 lowercase hex characters from 8 crypto/rand bytes.
func GenerateDiskStableID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", cpierrors.Wrap(err, "GenerateDiskStableID: read random bytes")
	}
	return DiskStableIDPrefix + hex.EncodeToString(b[:]), nil
}

// StableIDFromDriveOptStr extracts the stable-ID serial from a PVE drive
// option string ("<volid>,serial=bpd-...,size=..."). Returns ("", false) when
// the entry carries no serial option or a serial without the bpd- prefix
// (an operator- or guest-tooling-assigned serial is not a CPI identity).
func StableIDFromDriveOptStr(optStr string) (string, bool) {
	rest := optStr
	for {
		comma := strings.IndexByte(rest, ',')
		if comma < 0 {
			return "", false
		}
		rest = rest[comma+1:]
		if v, ok := strings.CutPrefix(rest, "serial="); ok {
			if end := strings.IndexByte(v, ','); end >= 0 {
				v = v[:end]
			}
			if strings.HasPrefix(v, DiskStableIDPrefix) {
				return v, true
			}
			return "", false
		}
	}
}

// DiskTransferIntent is a parker's recorded intent to receive a disk whose
// detach-side transfer has not finished. It is written into the parker's
// provenance sentinel BEFORE the source VM's slot is deleted, so a crash
// anywhere in the transfer window always leaves at least one carrier of the
// disk's identity (the write ordering D13 specifies).
type DiskTransferIntent struct {
	// ParkerVMID and ParkerNode identify the parker carrying the record.
	ParkerVMID int
	ParkerNode string
	// Slot is the parker bus slot the transfer targets.
	Slot string
	// Volid is the volid recorded when the record was written: the pre-move
	// volid while the transfer is in flight, rewritten to the landed volid
	// when it completes. Stale exactly in the window after the move task and
	// before the finalize write — recovery re-derives it from the slot.
	Volid string
	// SourceVMCID is the VM the disk was being detached from, when recorded.
	SourceVMCID string
}

// DiskIdentity is the result of resolving a disk CID to the volid the
// cluster currently knows the volume by.
type DiskIdentity struct {
	// Volid is the disk's current volid: the one found on a drive entry by
	// the identity scan, the provenance-recorded one for a mid-transfer disk,
	// or the birth volid when nothing in the cluster references the disk.
	Volid string
	// Holder is the VM whose config references the volume; zero (Found=false)
	// when nothing does.
	Holder DiskHolder
	// Intent is non-nil when the disk was located only through a parker's
	// provenance record: a detach-side transfer crashed mid-flight. Mutating
	// handlers resume the transfer before proceeding; read paths treat the
	// recorded volid as the best-known name.
	Intent *DiskTransferIntent
}

// ResolveDiskIdentity resolves a disk's current volid and holder, in the
// D13 fallback order: stable-ID scan (which also matches the birth volid in
// the same pass) → parker provenance sentinel → birth volid.
//
// stableID == "" is the legacy case and returns the birth volid immediately,
// with no API calls: legacy CIDs are volid-resolved forever, and their
// callers keep the exact call pattern they had before stable IDs existed.
func ResolveDiskIdentity(
	ctx context.Context, c Client, logger *log.Logger, birthVolid, stableID string, cfg ParkerConfig,
) (DiskIdentity, error) {
	if birthVolid == "" {
		return DiskIdentity{}, cpierrors.Cloud("ResolveDiskIdentity: birthVolid must not be empty")
	}
	if stableID == "" {
		return DiskIdentity{Volid: birthVolid}, nil
	}
	if c == nil {
		return DiskIdentity{}, cpierrors.Cloud("ResolveDiskIdentity: client must not be nil")
	}

	hit, err := findVMByDiskIdentityScan(ctx, c, cfg.FallbackNode, birthVolid, stableID)
	if err == nil {
		return DiskIdentity{Volid: hit.Volid, Holder: holderFromScanHit(logger, hit, birthVolid, cfg)}, nil
	}
	if !errors.Is(err, ErrDiskNotAttachedToAnyVM) {
		return DiskIdentity{}, cpierrors.Wrap(err, "ResolveDiskIdentity: identity scan")
	}

	// No active bus slot anywhere carries the disk. A detach-side transfer
	// that crashed between the intent record and the serial re-apply leaves
	// the volume findable only through the receiving parker's provenance.
	intent, found, provErr := findParkedDiskIntentByStableID(ctx, c, stableID, cfg)
	if provErr != nil {
		return DiskIdentity{}, cpierrors.Wrap(provErr, "ResolveDiskIdentity: parker provenance scan")
	}
	if found {
		volid := intent.Volid
		if volid == "" {
			volid = birthVolid
		}
		i := intent
		return DiskIdentity{Volid: volid, Intent: &i}, nil
	}

	// Never transferred (or free-floating): the volume keeps its birth name.
	return DiskIdentity{Volid: birthVolid}, nil
}

// holderFromScanHit classifies an identity-scan hit into the DiskHolder shape
// resolveDiskHolder produces, without a second config read: the scan already
// carried the tags and slot out of the config it matched.
func holderFromScanHit(logger *log.Logger, hit DiskScanHit, birthVolid string, cfg ParkerConfig) DiskHolder {
	holder := DiskHolder{Found: true, VMID: hit.VMID, Node: hit.Node, Tags: hit.Tags}
	inBand := hit.VMID >= cfg.VMIDRangeStart && hit.VMID <= cfg.VMIDRangeEnd
	if !inBand {
		return holder
	}
	if !tagContainsParker(hit.Tags) {
		if logger != nil {
			// Same anomaly, same level policy as resolveDiskHolder: surprising
			// under "parked", routine under "free" or a stood-down default.
			logUntagged := logger.Debug
			if cfg.ParkedEnabled {
				logUntagged = logger.Warn
			}
			logUntagged("disk holder is in the parker range but carries no bosh-parker tag — treating it as a real VM",
				log.Int("vmid", hit.VMID),
				log.String("node", hit.Node),
				log.String("volid", birthVolid),
				log.String("tags", hit.Tags),
			)
		}
		return holder
	}
	holder.IsParker = true
	holder.Slot = hit.Slot
	return holder
}

// findParkedDiskIntentByStableID scans every parker in the configured band,
// cluster-wide, for a bosh_parked_disks entry keyed by stableID. Returns the
// recorded intent and whether one was found.
//
// A parker whose config vanished between the listing and the read is skipped:
// its provenance vanished with it, and the strict-anchor refusal (which owns
// the "parker deleted out-of-band" condition) is a holder-scan concern, not a
// resolution one. Any other config-read failure propagates — concluding "no
// record" from a read that never arrived is how a mid-transfer disk gets
// treated as free-floating.
func findParkedDiskIntentByStableID(
	ctx context.Context, c Client, stableID string, cfg ParkerConfig,
) (DiskTransferIntent, bool, error) {
	if cfg.VMIDRangeStart <= 0 || cfg.VMIDRangeEnd <= cfg.VMIDRangeStart {
		// No usable band, no parkers to scan. Not an error: resolution simply
		// has no provenance fallback to consult.
		return DiskTransferIntent{}, false, nil
	}
	parkers, err := listParkersCluster(ctx, c, cfg)
	if err != nil {
		return DiskTransferIntent{}, false, err
	}
	for _, p := range parkers {
		vmCfg, cfgErr := c.QEMU().Config(ctx, p.node, p.vmid)
		if cfgErr != nil {
			if parkerConfigGone(cfgErr) {
				continue
			}
			return DiskTransferIntent{}, false, cpierrors.Wrap(WrapConfigReadError(cfgErr),
				fmt.Sprintf("provenance scan: config fetch for parker vmid %d on node %s", p.vmid, p.node))
		}
		if tags, _ := vmCfg["tags"].(string); !tagContainsParker(tags) {
			continue
		}
		_, disks, _ := parseParkerSentinel(DescriptionFromConfig(vmCfg))
		entry, ok := disks[stableID]
		if !ok {
			continue
		}
		return DiskTransferIntent{
			ParkerVMID:  p.vmid,
			ParkerNode:  p.node,
			Slot:        entry.Slot,
			Volid:       entry.Volid,
			SourceVMCID: entry.SourceVMCID,
		}, true, nil
	}
	return DiskTransferIntent{}, false, nil
}

// parkerCandidate is one in-band cluster row a provenance scan reads.
type parkerCandidate struct {
	vmid int
	node string
}

// listParkersCluster lists every QEMU guest in the parker band across all
// nodes, from one /cluster/resources call. Rows that elide "node" fall back
// to cfg.FallbackNode and are dropped when there is none — the same policy
// every other scan in this package applies to node-less rows.
func listParkersCluster(ctx context.Context, c Client, cfg ParkerConfig) ([]parkerCandidate, error) {
	typeStr := "vm"
	var resp *sdkcluster.ListResourcesResponse
	listErr := RetryOnTransient(ctx, nil, "provenance_scan_list", 0, func() error {
		var inner error
		resp, inner = c.Cluster().ListResources(ctx, &sdkcluster.ListResourcesParams{Type: &typeStr})
		return inner
	})
	if listErr != nil {
		return nil, cpierrors.Wrap(WrapError(listErr), "provenance scan: list cluster resources")
	}
	if resp == nil {
		return nil, cpierrors.Retriable("provenance scan: nil response from cluster resources")
	}
	var out []parkerCandidate
	for _, raw := range *resp {
		var entry struct {
			VMID int64  `json:"vmid"`
			Node string `json:"node"`
			Type string `json:"type"`
		}
		if jsonErr := json.Unmarshal(raw, &entry); jsonErr != nil || entry.VMID <= 0 {
			continue
		}
		// LXC containers cannot be parkers and their configs are unreadable
		// through the QEMU endpoint; skip them like every other scan here.
		if entry.Type != "" && entry.Type != clusterResourceTypeQemu {
			continue
		}
		if entry.VMID < int64(cfg.VMIDRangeStart) || entry.VMID > int64(cfg.VMIDRangeEnd) {
			continue
		}
		node := entry.Node
		if node == "" {
			node = cfg.FallbackNode
		}
		if node == "" {
			continue
		}
		out = append(out, parkerCandidate{vmid: int(entry.VMID), node: node})
	}
	return out, nil
}
