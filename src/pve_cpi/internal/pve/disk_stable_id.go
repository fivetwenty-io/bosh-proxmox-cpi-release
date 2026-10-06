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
	"sort"
	"strings"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclient "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/client"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
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
	AllocationID, AllocationNamespace, AllocationBacking string
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
	// Opts is the disk's recorded drive-option overrides, carried in the
	// record so a resumed transfer re-persists them instead of finalizing an
	// entry that silently drops them.
	Opts map[string]string
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
	// Unused is non-empty when no active slot carries the disk and no
	// parker records its transfer, but one or more unusedN entries name its
	// birth volid. That is a disk stranded by a transfer that deleted the
	// guest's slot and never moved the volume, after its transfer record was
	// lost. Holder stays empty, because no slot holds the disk, and the
	// handlers that can act on an unused entry read this list instead.
	Unused []VolumeReference
}

// DiskBirthNameRunbook points the refusal of a disk whose birth volume another
// entry names at the way out. The docs do not ship in the release, so it names
// the repository as well as the file, and a test pins the heading it quotes.
const DiskBirthNameRunbook = `see "Another entry names a disk's birth volume" in docs/troubleshooting.md of bosh-proxmox-cpi-release`

// DiskBirthNameHeldError is the refusal ResolveDiskIdentity returns when no
// slot carries a disk's serial, no parker records its transfer, and some entry
// names its birth volume without proving it is the disk. The entry can be a
// slot under another serial or none, or an unusedN entry on a guest that holds
// no note for the disk. Another disk can take a birth name once a move renames
// the disk off it, so acting on that volume could write to another disk or
// hand it to this disk's consumer. The state stays until an operator acts, so
// the refusal is permanent.
type DiskBirthNameHeldError struct {
	StableID   string
	BirthVolid string
	Holders    []BirthNameHolder
}

func (e *DiskBirthNameHeldError) Error() string {
	names := make([]string, 0, len(e.Holders))
	for _, h := range e.Holders {
		names = append(names, describeBirthNameHolder(h))
	}
	verb := "names"
	if len(names) > 1 {
		verb = "name"
	}
	return fmt.Sprintf(
		"no slot carries the disk's serial %s, but %s %s its birth volume %s, so we can't tell whether that volume is still the disk. "+
			"Nothing acts on the volume until an operator confirms whose it is; %s",
		e.StableID, strings.Join(names, " and "), verb, e.BirthVolid, DiskBirthNameRunbook)
}

// describeBirthNameHolder names one entry the way the refusal quotes it.
func describeBirthNameHolder(h BirthNameHolder) string {
	switch {
	case h.Unused:
		return fmt.Sprintf("unused entry %s of VM %d on node %s, whose description holds no note for the disk", h.Slot, h.VMID, h.Node)
	case h.Serial != "":
		return fmt.Sprintf("slot %s of VM %d on node %s with serial %s", h.Slot, h.VMID, h.Node, h.Serial)
	default:
		return fmt.Sprintf("slot %s of VM %d on node %s with no serial", h.Slot, h.VMID, h.Node)
	}
}

// IsDiskBirthNameHeld reports whether err carries a DiskBirthNameHeldError
// and returns it.
func IsDiskBirthNameHeld(err error) (*DiskBirthNameHeldError, bool) {
	var held *DiskBirthNameHeldError
	if errors.As(err, &held) {
		return held, true
	}
	return nil, false
}

// ResolveDiskIdentity resolves a disk's current volid and holder. It looks
// for the disk in this order: the slot carrying the disk's serial, then a
// parker's transfer record, then the unusedN entries that name the birth
// volid on a guest holding the disk's note, and then the birth volid itself.
// The birth volid is only the volume's name at create_disk, and another
// volume can hold it after a move renamed the disk, so a slot that names it
// under another serial or none never resolves as the disk. When such a slot,
// or an unusedN entry on a guest holding no note for the disk, names the
// birth volid and neither the serial nor a record locates the disk, it
// returns a permanent DiskBirthNameHeldError in place of the birth volid,
// even when a noted unusedN entry names it too. A scan that couldn't read
// every node still fails retriably first. A disk nothing names resolves to
// its birth volid.
//
// stableID == "" is the legacy case and returns the birth volid immediately,
// with no API calls: legacy CIDs are volid-resolved forever, and their
// callers keep the exact call pattern they had before stable IDs existed.
func ResolveDiskIdentity(
	ctx context.Context, c Client, logger *log.Logger, birthVolid, stableID string, cfg ParkerConfig,
) (DiskIdentity, error) {
	return resolveDiskIdentity(ctx, c, logger, birthVolid, stableID, cfg, matchSerial)
}

// ResolveDiskIdentityMatchingName is ResolveDiskIdentity with a slot that
// names the birth volid matched as the disk, under any serial or none, and
// every unusedN entry that names it reported in Unused. It never returns a
// DiskBirthNameHeldError. It is for a caller that wants to know whether
// anything names the volume or carries the token at all, which is what the
// allocation collision check asks before it proves the volume absent.
func ResolveDiskIdentityMatchingName(
	ctx context.Context, c Client, logger *log.Logger, birthVolid, stableID string, cfg ParkerConfig,
) (DiskIdentity, error) {
	return resolveDiskIdentity(ctx, c, logger, birthVolid, stableID, cfg, matchNameOrSerial)
}

func resolveDiskIdentity(
	ctx context.Context, c Client, logger *log.Logger, birthVolid, stableID string, cfg ParkerConfig, match diskMatch,
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

	hit, err := findVMByDiskIdentityScan(ctx, c, birthVolid, stableID, match)
	if err == nil {
		return DiskIdentity{Volid: hit.Volid, Holder: holderFromScanHit(logger, hit, birthVolid, cfg)}, nil
	}
	if !errors.Is(err, ErrDiskNotAttachedToAnyVM) {
		return DiskIdentity{}, cpierrors.Wrap(err, "ResolveDiskIdentity: identity scan")
	}

	// No active bus slot anywhere carries the disk. A detach-side transfer
	// that crashed between the intent record and the serial re-apply leaves
	// the volume findable only through the receiving parker's provenance.
	// The record comes before the birth-name refusal below on purpose. A move
	// that landed before its serial write can take the disk's birth name
	// back, and the resume that settles it starts only from the record we
	// return here, so refusing would leave that crash with no way out.
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
		// The counts the scan gathered ride out here too. A mid-transfer disk
		// is one an interrupted detach left on a parker, and the handlers that
		// resume it reach the same absence proof the other two branches do, so
		// dropping the counts here would send that proof to the journal and
		// the storage status for evidence the caller was already holding.
		return DiskIdentity{
			Volid:  volid,
			Intent: &i,
			Holder: DiskHolder{StorageReferences: hit.StorageReferences},
		}, nil
	}

	// No slot carries the serial and no parker records a transfer, so the
	// entries that name the birth volume are all that's left, and none of them
	// proves it is the disk. A slot there holds another serial or none, and an
	// unused entry sits on a guest that holds no note for the disk. This check
	// comes before the default below that reports the noted unused entries, so
	// when any of these entries exists, the resolution refuses even if a noted
	// unused entry names the volume too.
	if len(hit.NameHolders) > 0 {
		return DiskIdentity{}, cpierrors.Wrap(
			&DiskBirthNameHeldError{StableID: stableID, BirthVolid: birthVolid, Holders: hit.NameHolders},
			"ResolveDiskIdentity: refusing to resolve the disk by its birth volume")
	}

	// Never transferred (or free-floating): the volume keeps its birth name.
	// The holder is empty, but the reference counts the scan gathered on its
	// way to that answer ride out on it: a free-floating disk is exactly what
	// delete_disk is about to prove absent, and the counts are the cheapest
	// second opinion it has. The unused entries that name the birth volid on
	// a guest holding the disk's note ride out too. They come after the intent
	// on purpose, because a deferred park leaves the volume on an unused entry
	// with its intent in place, and that disk has to keep resuming.
	return DiskIdentity{Volid: birthVolid, Holder: DiskHolder{StorageReferences: hit.StorageReferences}, Unused: hit.Unused}, nil
}

// holderFromScanHit classifies an identity-scan hit into the DiskHolder shape
// resolveDiskHolder produces, without a second config read: the scan already
// carried the tags and slot out of the config it matched.
func holderFromScanHit(logger *log.Logger, hit DiskScanHit, birthVolid string, cfg ParkerConfig) DiskHolder {
	holder := DiskHolder{Found: true, VMID: hit.VMID, Node: hit.Node, Tags: hit.Tags, StorageReferences: hit.StorageReferences}
	holder.PendingSlot, holder.PendingChange = pendingSlotOf(hit)
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
	var (
		intent DiskTransferIntent
		found  bool
	)
	err := walkParkerRecords(ctx, c, cfg, func(p parkerCandidate, disks map[string]parkerProvEntry) bool {
		entry, ok := disks[stableID]
		if !ok {
			return false
		}
		intent, found = intentFromParkerEntry(p, entry), true
		return true
	})
	if err != nil {
		return DiskTransferIntent{}, false, err
	}
	return intent, found, nil
}

// SourceTransferRecord is one stable-ID record on a parker that names a given
// source VM. DiskCID is the CID the record was written with, which is the
// Director's CID when the transfer came from a disk call and the volume's own
// name when delete_vm started it.
type SourceTransferRecord struct {
	StableID string
	DiskCID  string
	Intent   DiskTransferIntent
}

// FindSourceTransferRecords returns every stable-ID record, on every parker in
// the band, whose source VM is sourceVMCID. It reads the parkers the way
// findParkedDiskIntentByStableID does, so a parker that is gone is skipped with
// its records, any other read failure fails the scan, and a band that isn't
// configured finds nothing. The records come back sorted by parker and then by
// stable ID.
//
// A record whose transfer is still in flight names the volume by the name it
// has on the source VM. A finished record names the volume the parker holds,
// so a caller that matches records against the source VM's own entries only
// ever matches a transfer that hasn't landed.
func FindSourceTransferRecords(ctx context.Context, c Client, sourceVMCID string, cfg ParkerConfig) ([]SourceTransferRecord, error) {
	if c == nil {
		return nil, cpierrors.Cloud("FindSourceTransferRecords: client must not be nil")
	}
	if sourceVMCID == "" {
		return nil, cpierrors.Cloud("FindSourceTransferRecords: source VM CID must not be empty")
	}
	var out []SourceTransferRecord
	err := walkParkerRecords(ctx, c, cfg, func(p parkerCandidate, disks map[string]parkerProvEntry) bool {
		for key := range disks {
			entry := disks[key]
			if !strings.HasPrefix(key, DiskStableIDPrefix) || entry.SourceVMCID != sourceVMCID || entry.Volid == "" {
				continue
			}
			out = append(out, SourceTransferRecord{StableID: key, DiskCID: entry.DiskCID, Intent: intentFromParkerEntry(p, entry)})
		}
		return false
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Intent.ParkerVMID != out[j].Intent.ParkerVMID {
			return out[i].Intent.ParkerVMID < out[j].Intent.ParkerVMID
		}
		return out[i].StableID < out[j].StableID
	})
	return out, nil
}

// walkParkerRecords reads every tagged parker in the band and passes each
// one's bosh_parked_disks map to visit, stopping early when visit returns
// true. A band that isn't usable has no parkers to read, which isn't an error.
// A parker whose config vanished between the listing and the read is skipped,
// and any other read failure stops the walk and comes back, because a record
// we never read must not count as a record that isn't there.
func walkParkerRecords(
	ctx context.Context, c Client, cfg ParkerConfig, visit func(parkerCandidate, map[string]parkerProvEntry) bool,
) error {
	if cfg.VMIDRangeStart <= 0 || cfg.VMIDRangeEnd <= cfg.VMIDRangeStart {
		return nil
	}
	parkers, err := listParkersCluster(ctx, c, cfg)
	if err != nil {
		return err
	}
	for _, p := range parkers {
		vmCfg, cfgErr := c.QEMU().Config(ctx, p.node, p.vmid)
		if cfgErr != nil {
			if parkerConfigGone(cfgErr) {
				continue
			}
			return cpierrors.Wrap(WrapConfigReadError(cfgErr),
				fmt.Sprintf("provenance scan: config fetch for parker vmid %d on node %s", p.vmid, p.node))
		}
		if tags, _ := ConfigString(vmCfg, "tags"); !tagContainsParker(tags) {
			continue
		}
		_, disks, _ := parseParkerSentinel(DescriptionFromConfig(vmCfg))
		if visit(p, disks) {
			return nil
		}
	}
	return nil
}

// intentFromParkerEntry is the transfer intent one parker record describes.
func intentFromParkerEntry(p parkerCandidate, entry parkerProvEntry) DiskTransferIntent {
	return DiskTransferIntent{
		AllocationID: entry.AllocationID, AllocationNamespace: entry.AllocationNamespace, AllocationBacking: entry.AllocationBacking,
		ParkerVMID:  p.vmid,
		ParkerNode:  p.node,
		Slot:        entry.Slot,
		Volid:       entry.Volid,
		SourceVMCID: entry.SourceVMCID,
		Opts:        sanitizeDiskOptOverlay(entry.Opts),
	}
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
			VMID sdkclient.PVEInt `json:"vmid"`
			Node string           `json:"node"`
			Type string           `json:"type"`
		}
		if jsonErr := json.Unmarshal(raw, &entry); jsonErr != nil || entry.VMID.Int() <= 0 {
			continue
		}
		// LXC containers cannot be parkers and their configs are unreadable
		// through the QEMU endpoint; skip them like every other scan here.
		if entry.Type != "" && entry.Type != clusterResourceTypeQemu {
			continue
		}
		if entry.VMID.Int() < int64(cfg.VMIDRangeStart) || entry.VMID.Int() > int64(cfg.VMIDRangeEnd) {
			continue
		}
		node := entry.Node
		if node == "" {
			node = cfg.FallbackNode
		}
		if node == "" {
			continue
		}
		out = append(out, parkerCandidate{vmid: int(entry.VMID.Int()), node: node})
	}
	return out, nil
}
