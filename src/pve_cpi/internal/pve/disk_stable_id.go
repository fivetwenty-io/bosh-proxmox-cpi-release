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
	"slices"
	"sort"
	"strconv"
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

// DiskIdentityCopiedRunbook points the refusal of a disk whose identity more
// than one guest carries at the way out. A test pins the heading it quotes.
const DiskIdentityCopiedRunbook = `see "A disk's serial or transfer record is on more than one guest" in docs/troubleshooting.md of bosh-proxmox-cpi-release`

// diskIdentityCopiedTellApart is how every form of the copy refusal tells the
// operator to find the copy. PVE files a restore under the guest it created
// and a clone under the guest it copied, so the VMID a task is filed under
// can name either one, and the Director and the CPI's own log settle it.
const diskIdentityCopiedTellApart = "PVE files a qmrestore task under the copy it created, but it files a qmclone task under " +
	"the VM it copied, so the VMID a task is filed under doesn't say which guest is the copy. The original is the guest " +
	"that runs the VM CID `bosh instances --details` lists with the disk, or the guest that holds the volume the CPI's " +
	"latest log line for the disk gives as volid_after."

// DiskIdentityCopiedError is the refusal ResolveDiskIdentity returns when a
// second read of the cluster still finds a disk's identity in more than one
// place. Holders lists the guests whose slots carry the disk's serial. When no
// slot carries it, Records lists the parkers that each keep a record of the
// disk's transfer. When one slot carries it, Records lists the parkers whose
// record names another volume that is still there, and Evidence says, for
// each of those records in turn, where that volume still is. A clone or a
// backup restore copies a guest's drive lines with their serials and its
// description with its records, so one of the volumes is a copy, and acting
// on the first one the scan reads could mount, grow, or delete the copy in
// place of the disk. The state stays until an operator removes the copy, so
// the refusal is permanent.
type DiskIdentityCopiedError struct {
	StableID string
	Holders  []DiskSerialHolder
	Records  []DiskTransferIntent
	Evidence []string
}

func (e *DiskIdentityCopiedError) Error() string {
	switch {
	case len(e.Holders) == 0:
		return e.recordsError()
	case len(e.Holders) == 1 && len(e.Records) > 0:
		return e.slotAndRecordError()
	}
	guests := make([]string, 0, len(e.Holders))
	for _, h := range e.Holders {
		guests = append(guests, describeSerialHolder(h))
	}
	return fmt.Sprintf(
		"the disk's serial %s is on %s, so we can't tell which volume is the disk. A clone or a backup restore copies a "+
			"drive line with its serial. %s Nothing acts on the disk until an operator removes serial=%s from the copy's "+
			"drive line; %s",
		e.StableID, strings.Join(guests, " and "), diskIdentityCopiedTellApart, e.StableID, DiskIdentityCopiedRunbook)
}

// recordsError is the refusal's text when no slot carries the serial and
// more than one parker keeps a record of the disk's transfer.
func (e *DiskIdentityCopiedError) recordsError() string {
	parkers := make([]string, 0, len(e.Records))
	for i := range e.Records {
		r := &e.Records[i]
		parkers = append(parkers, fmt.Sprintf("parker VM %d on node %s (recorded volume %s, source VM %q)",
			r.ParkerVMID, r.ParkerNode, r.Volid, r.SourceVMCID))
	}
	return fmt.Sprintf(
		"no slot carries the disk's serial %s, but %s each keep a record of its transfer, so we can't tell which parker "+
			"received the disk. A clone or a backup restore of a parker copies its records. %s Nothing acts on the disk "+
			"until an operator removes the record from every parker but the one that received the disk; %s",
		e.StableID, strings.Join(parkers, " and "), diskIdentityCopiedTellApart, DiskIdentityCopiedRunbook)
}

// slotAndRecordError is the refusal's text when one slot carries the serial
// and a parker's record of the disk's transfer names another volume that is
// still there.
func (e *DiskIdentityCopiedError) slotAndRecordError() string {
	records := make([]string, 0, len(e.Records))
	for i := range e.Records {
		r := &e.Records[i]
		evidence := ""
		if i < len(e.Evidence) {
			evidence = ", and " + e.Evidence[i]
		}
		records = append(records, fmt.Sprintf("parker VM %d on node %s keeps a record of the disk's transfer that names volume %s%s",
			r.ParkerVMID, r.ParkerNode, r.Volid, evidence))
	}
	return fmt.Sprintf(
		"the disk's serial %s is on %s, but %s, so we can't tell which volume is the disk. A clone or a backup restore "+
			"taken while the disk was moving to a parker copies its drive line with the serial, so the slot can be the "+
			"copy's while the disk waits for the parker. %s Nothing acts on the disk until an operator removes serial=%s "+
			"from the slot's drive line when the slot is the copy's, or removes the record from the parker when the slot "+
			"holds the disk; %s",
		e.StableID, describeSerialHolder(e.Holders[0]), strings.Join(records, "; and "), diskIdentityCopiedTellApart,
		e.StableID, DiskIdentityCopiedRunbook)
}

// describeSerialHolder names one slot the way the refusal quotes it.
func describeSerialHolder(h DiskSerialHolder) string {
	kind := "VM"
	if h.Parker {
		kind = "parker VM"
	}
	return fmt.Sprintf("slot %s of %s %d on node %s with volume %s", h.Slot, kind, h.VMID, h.Node, h.Volid)
}

// IsDiskIdentityCopied reports whether err carries a DiskIdentityCopiedError
// and returns it.
func IsDiskIdentityCopied(err error) (*DiskIdentityCopiedError, bool) {
	var copied *DiskIdentityCopiedError
	if errors.As(err, &copied) {
		return copied, true
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
// When two guests carry the disk's serial, or no slot carries it and two
// parkers each keep a record of its transfer, or one slot carries it while a
// parker's record names another volume that is still there, the resolution
// reads the cluster a second time, and when that read finds the same, it
// returns a permanent DiskIdentityCopiedError. One guest counts once,
// whichever of its views or slots carries the serial.
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

	identity, err := resolveDiskIdentityOnce(ctx, c, logger, birthVolid, stableID, cfg, match)
	if _, copied := IsDiskIdentityCopied(err); !copied {
		return identity, err
	}
	// The scan reads one guest at a time and holds no lock, so a move that
	// lands between two of its reads shows the disk on the guest it left and
	// on the guest it reached, and a move in flight can hide the disk from
	// every slot while both parkers of a mover hand-off keep a record. No
	// transfer leaves two guests carrying the serial at once, because PVE
	// writes the giving guest's config before the receiving one's, and the
	// CPI writes a serial only onto a guest that holds the volume alone. A
	// second pass therefore reads the moved disk in one place, and only a
	// copy is still there to refuse.
	if logger != nil {
		logger.Warn("disk identity read on more than one guest; reading the cluster again before refusing",
			log.String("stable_id", stableID),
			log.String("birth_volid", birthVolid),
			log.Err(err),
		)
	}
	return resolveDiskIdentityOnce(ctx, c, logger, birthVolid, stableID, cfg, match)
}

// resolveDiskIdentityOnce is one pass of resolveDiskIdentity, from the slot
// scan through the parker records to the birth volid.
func resolveDiskIdentityOnce(
	ctx context.Context, c Client, logger *log.Logger, birthVolid, stableID string, cfg ParkerConfig, match diskMatch,
) (DiskIdentity, error) {
	hit, err := findVMByDiskIdentityScan(ctx, c, birthVolid, stableID, match)
	if err == nil {
		if len(hit.SerialHolders) > 1 {
			return DiskIdentity{}, cpierrors.Wrap(
				&DiskIdentityCopiedError{StableID: stableID, Holders: hit.SerialHolders},
				"ResolveDiskIdentity: refusing to resolve a disk whose serial more than one guest carries")
		}
		if len(hit.SerialHolders) == 1 {
			if refusal := refuseRecordOfAnotherVolume(ctx, c, hit, birthVolid, stableID, cfg); refusal != nil {
				return DiskIdentity{}, refusal
			}
		}
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
	intents, provErr := findParkedDiskIntentByStableID(ctx, c, stableID, cfg)
	if provErr != nil {
		return DiskIdentity{}, cpierrors.Wrap(provErr, "ResolveDiskIdentity: parker provenance scan")
	}
	// Two parkers that each keep a record leave nothing to say which one
	// received the disk. The transfer resume refuses the same state before it
	// moves a volume or writes a serial, so we refuse it here as well, before
	// a handler acts on either record.
	if len(intents) > 1 {
		return DiskIdentity{}, cpierrors.Wrap(
			&DiskIdentityCopiedError{StableID: stableID, Records: intents},
			"ResolveDiskIdentity: refusing to resolve a disk that more than one parker records")
	}
	if len(intents) == 1 {
		intent := intents[0]
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

// refuseRecordOfAnotherVolume refuses a disk that one slot carries when a
// parker's record of the disk's transfer names another volume that is still
// there. A clone or a backup restore taken while the disk was moving to a
// parker copies the drive line with its serial, so the copy's slot is then
// the only one that carries it, and the record is all that shows the disk is
// elsewhere.
//
// The records are the ones on the parkers the identity scan read, from the
// configs that scan already holds, so a parker on a node the quorate cluster
// reports offline goes unseen here as it does in the scan, and no parker is
// read twice.
//
// A record doesn't refuse when the slot carrier keeps it, because the name in
// a parker's own record is out of date between the move and the record's
// finalize, and the slot is what the transfer resume trusts. It doesn't
// refuse when it names the slot's own volume either, because that's a
// transfer that hasn't deleted the source slot yet. Any other record refuses
// while its volume is still there, as recordPresence.stillThere decides. A
// record whose volume is gone was left behind by a finished move that renamed
// the volume, as an attach does when it lands before it removes the parker's
// record, or as a mover does before it removes the shared parker's. A read
// that can't settle whether the volume is there returns a retriable error and
// never counts as a volume that's gone.
func refuseRecordOfAnotherVolume(
	ctx context.Context, c Client, hit DiskScanHit, birthVolid, stableID string, cfg ParkerConfig,
) error {
	carrier := hit.SerialHolders[0]
	var presence *recordPresence
	var live []DiskTransferIntent
	var evidence []string
	records := scannedParkerRecords(hit, stableID, cfg)
	for i := range records {
		record := &records[i]
		volume := record.intent.Volid
		if volume == "" {
			volume = birthVolid
		}
		if record.intent.ParkerVMID == carrier.VMID || volume == carrier.Volid {
			continue
		}
		if presence == nil {
			presence = newRecordPresence(c, hit, stableID, cfg)
		}
		found, err := presence.stillThere(ctx, record, volume)
		if err != nil {
			return err
		}
		if found != "" {
			intent := record.intent
			intent.Volid = volume
			live = append(live, intent)
			evidence = append(evidence, found)
		}
	}
	if len(live) == 0 {
		return nil
	}
	return cpierrors.Wrap(
		&DiskIdentityCopiedError{StableID: stableID, Holders: []DiskSerialHolder{carrier}, Records: live, Evidence: evidence},
		"ResolveDiskIdentity: refusing to resolve a disk whose slot and transfer record name different volumes")
}

// scannedRecord is one parker's record of a disk's transfer, read from the
// parker config the identity scan holds.
type scannedRecord struct {
	intent DiskTransferIntent
	parker *scannedParker
}

// scannedParkerRecords returns the record keyed by stableID on each parker the
// identity scan read, in the order it read them. It applies the rules
// walkParkerRecords applies to the same applied view, so a band that isn't
// usable has no records, and a guest outside the band or without the parker
// tag keeps none.
func scannedParkerRecords(hit DiskScanHit, stableID string, cfg ParkerConfig) []scannedRecord {
	if cfg.VMIDRangeStart <= 0 || cfg.VMIDRangeEnd <= cfg.VMIDRangeStart {
		return nil
	}
	var records []scannedRecord
	for i := range hit.parkers {
		p := &hit.parkers[i]
		if p.vmid < cfg.VMIDRangeStart || p.vmid > cfg.VMIDRangeEnd {
			continue
		}
		_, disks, _ := parseParkerSentinel(DescriptionFromConfig(p.views.Applied()))
		entry, ok := disks[stableID]
		if !ok {
			continue
		}
		records = append(records, scannedRecord{
			intent: intentFromParkerEntry(parkerCandidate{vmid: p.vmid, node: p.node}, entry),
			parker: p,
		})
	}
	return records
}

// recordPresence decides, for the records of one resolution, whether the
// volume a record names is still there. It shares one absence proof across
// the records, so each storage is classified at most once.
type recordPresence struct {
	c        Client
	hit      DiskScanHit
	stableID string
	proof    *keepAbsenceProof
}

func newRecordPresence(c Client, hit DiskScanHit, stableID string, cfg ParkerConfig) *recordPresence {
	return &recordPresence{c: c, hit: hit, stableID: stableID, proof: newKeepAbsenceProof(c, cfg)}
}

// stillThere says where the volume a parker's record names is still found, in
// words the refusal quotes, or returns "" when the volume isn't the disk's any
// more. It checks, in this order:
//
//  1. The record's own slot on its parker holds a volume named for the parker
//     with no serial, and no other disk's unfinished record names that slot.
//     That's the landing a move leaves before its serial write, and it's the
//     rule the transfer resume proves a landing by.
//  2. A disk key on a guest names the volume as this disk's, with this disk's
//     serial or none, outside the carrier's slot.
//  3. A snapshot of the record's source VM names the volume on a drive line
//     that carries this disk's serial or none. Only the resolver checks the
//     serial, so the read isn't the one refuseSnapshotNamingVolume makes at
//     the park gate, which counts a line whatever its serial. A deferred
//     park of a volume the source doesn't own leaves only the snapshot
//     naming it. A snapshot names
//     a volume by the name it had when the snapshot was taken, and the
//     transfer resume refuses to move a volume a snapshot names, so no other
//     disk's key or record overrides it. PVE reuses a freed VMID, so the
//     source can be a new guest whose own disk took the volume's name, and a
//     snapshot line under that disk's serial is that disk's and doesn't count.
//  4. The volume is another disk's, and so not this one's. That's a key that
//     names it under another disk's serial, a key with no serial on a parker
//     slot that another disk's unfinished record names, or another disk's
//     unfinished record. PVE gives a freed name to the next volume it renames
//     onto the same guest, which is how another disk comes to hold it. A
//     record counts as unfinished under the otherUnfinishedTransfers rule,
//     and only while no guest the scan read carries its disk's serial. A
//     record whose disk's serial is on some guest is left over from a
//     finished move, so it says nothing about who holds the name now. While
//     the scan left a node out, a guest there could carry that serial, so no
//     record sets the volume aside then. On node-local storage, or storage we
//     can't classify, a key or record sets aside only the volume on the node
//     where its guest sits, because the same name names another volume on
//     every other node, and step 5 still proves those. While the snapshot read
//     of step 3 failed, no key or record sets a volume aside, because a
//     snapshot outranks both, and the answer is a retriable error.
//  5. Storage still holds the volume, proven the way the transfer resume
//     proves a volume absent. proveOnStorage says on which nodes, and it
//     applies step 4 node by node.
//
// The guest read in step 2 skips a node the quorate cluster reports offline,
// as the identity scan does. A key found settles step 2 even then, but when
// only storage shows the volume while a node was left out, a guest on that
// node could name it under another disk's serial, so the answer is a
// retriable error rather than a refusal that never clears. When the reads
// can't settle whether the volume is there, the answer is a retriable error
// too. A record whose volume names no storage refuses permanently, because no
// retry can change that record.
func (p *recordPresence) stillThere(ctx context.Context, record *scannedRecord, volume string) (string, error) {
	intent := &record.intent
	views := record.parker.views
	otherSlots := otherUnfinishedTransferSlots(p.stableID, views.Applied(), views.Current())
	if intent.Slot != "" && len(otherSlots[intent.Slot]) == 0 {
		for _, landing := range parkerUnclaimedLandings(views, intent.ParkerVMID) {
			if landing.Key == intent.Slot {
				return fmt.Sprintf("the parker holds volume %s on %s with no serial, the slot the record names, which is what a "+
					"move that landed before its serial write leaves", landing.Volume, landing.Key), nil
			}
		}
	}

	holders, excluded, err := findDiskKeyHoldersTolerant(ctx, p.c, volume, p.stableID)
	if err != nil {
		return "", cpierrors.Wrap(err, fmt.Sprintf("ResolveDiskIdentity: read the guests that name recorded volume %s", volume))
	}
	offline := unionNodes(p.hit.excludedNodes, excluded)
	carrier := p.hit.SerialHolders[0]
	// others collects the node of each guest that holds the volume as another
	// disk's, for proveOnStorage to apply node by node.
	var others []string
	for i := range holders {
		h := &holders[i]
		if !h.NamesVolume || (h.VMID == carrier.VMID && h.Slot == carrier.Slot) {
			continue
		}
		if h.OtherDisk || (h.Parker && len(p.otherUnfinishedSlotsOn(h.VMID)[h.Slot]) > 0) {
			others = unionNodes(others, []string{h.Node})
			continue
		}
		return fmt.Sprintf("%s of VM %d on node %s still names that volume", h.Slot, h.VMID, h.Node), nil
	}

	snapshotFound, snapshotErr := p.snapshotOfSource(ctx, intent, volume)
	if snapshotFound != "" {
		return snapshotFound, nil
	}
	if len(offline) == 0 {
		others = unionNodes(others, p.unfinishedRecordNodes(volume))
	}

	storage, _, err := ParseDiskCID(volume)
	if err != nil {
		return "", cpierrors.Cloud(
			"ResolveDiskIdentity: refusing to resolve disk %s, because parker VM %d on node %s keeps a record of its "+
				"transfer that names volume %q, which names no storage, so we can't tell whether that volume is still "+
				"there, and a retry won't change the record. Once the slot that carries the serial is known to hold the "+
				"disk, remove the %q entry from bosh_parked_disks in parker VM %d's description and rerun; %s",
			p.stableID, intent.ParkerVMID, intent.ParkerNode, volume, p.stableID, intent.ParkerVMID, DiskIdentityCopiedRunbook)
	}
	return p.proveOnStorage(ctx, intent, volume, storage, offline, others, snapshotErr)
}

// proveOnStorage says on which node storage still holds volume, in words the
// refusal quotes, or returns "" once every node that could hold it proves it
// gone or holds it as another disk's. Shared storage shows every node the
// same content, so one proof on the parker's node settles it, and a guest in
// others, which hold the volume as another disk's, sets it aside everywhere.
// Node-local storage holds the volume only on the node it was written on, and
// a parker migrated after its transfer leaves that volume behind, so the
// proof runs on every node where the storage is enabled, which is its node
// list or else every cluster member, and a node in others is set aside on its
// own, because the volume of that name there is another volume than the one
// on any other node. A storage we can't classify gets the node-local proof
// unless others isn't empty, because then we can't tell whether the storage
// is shared and the other disk's claim covers every node, so the answer is a
// retriable error.
// When the snapshot read failed and any node is set aside, the answer is a
// retriable error, because a snapshot would outrank the other disk's claim.
// When a node the proof needs is offline or its read fails and no other node
// holds the volume, the answer is unsettled and comes back as a retriable
// error too.
func (p *recordPresence) proveOnStorage(
	ctx context.Context, intent *DiskTransferIntent, volume, storage string, offline, others []string, snapshotErr error,
) (string, error) {
	info, classified := p.proof.classifier(storage)(ctx)
	if !classified && len(others) > 0 {
		return "", cpierrors.Retriable(
			"ResolveDiskIdentity: can't tell whether volume %s, which parker vmid %d's record of the disk's transfer names, "+
				"is still the disk's on storage %s, because the read of storage %s failed, so we can't tell whether it is "+
				"shared, and another disk's key or unfinished record names the volume on node(s) %s, which would set it "+
				"aside on every node if the storage is shared; retry once storage %s can be read",
			volume, intent.ParkerVMID, storage, storage, strings.Join(others, ","), storage)
	}
	shared := classified && info.IsShared()
	nodes, err := p.proofNodes(ctx, storage, intent.ParkerNode, info, classified, shared)
	if errors.Is(err, errStorageNodesNoMember) {
		return "", cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"ResolveDiskIdentity: can't tell whether volume %s, which parker vmid %d's record of the disk's transfer names, "+
				"is still there, because the nodes list of storage %s, %s, names no current cluster member; the operator has "+
				"to fix that list, and a retry won't change it", volume, intent.ParkerVMID, storage, strings.Join(info.Nodes, ",")))
	}
	if err != nil {
		return "", resumeReadFailure(err, fmt.Sprintf(
			"ResolveDiskIdentity: list the nodes where storage %s is enabled, to tell whether volume %s, which parker vmid "+
				"%d's record of the disk's transfer names, is still there; retry once the cluster answers",
			storage, volume, intent.ParkerVMID))
	}
	var setAside []string
	for _, node := range nodes {
		if (shared && len(others) > 0) || slices.Contains(others, node) {
			setAside = append(setAside, node)
		}
	}
	if len(setAside) > 0 && snapshotErr != nil {
		// On shared storage the set-aside is the proof node, so the message
		// names the nodes where the other disk's claim sits instead.
		claimNodes := setAside
		if shared {
			claimNodes = others
		}
		return "", cpierrors.WrapAs(snapshotErr, cpierrors.TypeRetriableCloud, fmt.Sprintf(
			"ResolveDiskIdentity: can't tell whether volume %s, which parker vmid %d's record of the disk's transfer names, "+
				"is still the disk's on storage %s, because another disk's key or unfinished record names it on node(s) %s, "+
				"and the read of source VM %s's snapshots, which would outrank that, failed; retry once the snapshots "+
				"can be read", volume, intent.ParkerVMID, storage, strings.Join(claimNodes, ","), intent.SourceVMCID))
	}
	corroborators := append([]EmptyListingCorroborator{ConfigReferenceCorroborator(p.hit.StorageReferences)},
		p.proof.secondOpinions()...)
	var down, failed []string
	var failure error
	for _, node := range nodes {
		if slices.Contains(setAside, node) {
			continue
		}
		if slices.Contains(offline, node) {
			down = append(down, node)
			continue
		}
		absent, proofErr := ProveVolumeAbsent(ctx, p.c, node, storage, volume, p.proof.classifier(storage), corroborators...)
		if proofErr != nil {
			failed = append(failed, node)
			if failure == nil {
				failure = proofErr
			}
			continue
		}
		if absent {
			continue
		}
		if len(offline) > 0 {
			return "", cpierrors.Retriable(
				"ResolveDiskIdentity: storage %s on node %s still holds volume %s, which parker vmid %d's record of the "+
					"disk's transfer names, and no guest we could read names it, but node(s) %s are offline, and a guest "+
					"there could hold it as another disk's; retry once those nodes are back",
				storage, node, volume, intent.ParkerVMID, strings.Join(offline, ","))
		}
		return fmt.Sprintf("storage %s on node %s still holds that volume and no guest names it", storage, node), nil
	}
	if failure != nil {
		msg := fmt.Sprintf("ResolveDiskIdentity: can't tell whether volume %s, which parker vmid %d's record of the disk's "+
			"transfer names, is still on storage %s on node(s) %s", volume, intent.ParkerVMID, storage, strings.Join(failed, ","))
		if snapshotErr != nil {
			msg += fmt.Sprintf(", and the snapshot read failed too (%s); retry once both answer", snapshotErr.Error())
		} else {
			msg += "; retry once storage answers"
		}
		return "", resumeReadFailure(failure, msg)
	}
	if len(down) > 0 {
		return "", cpierrors.Retriable(
			"ResolveDiskIdentity: can't tell whether volume %s, which parker vmid %d's record of the disk's transfer names, "+
				"is still on node-local storage %s, because node(s) %s are offline and could hold it; retry once those "+
				"nodes are back", volume, intent.ParkerVMID, storage, strings.Join(down, ","))
	}
	return "", nil
}

// proofNodes returns the nodes on which proveOnStorage proves volume gone,
// with the parker's node first when it's one of them. Shared storage needs
// only the parker's node. Any other storage needs every cluster member it's
// enabled on. A node its nodes list names that isn't a member any more, as
// pvecm delnode leaves storage.cfg, can't be asked and can't hold a volume
// the cluster reaches, so it's left out. A nodes list with no member left is
// errStorageNodesNoMember.
func (p *recordPresence) proofNodes(
	ctx context.Context, storage, parkerNode string, info StorageInfo, classified, shared bool,
) ([]string, error) {
	if shared {
		return []string{parkerNode}, nil
	}
	members, err := ListClusterMemberNames(ctx, p.c)
	if err != nil {
		return nil, err
	}
	enabled := members
	if classified && len(info.Nodes) > 0 {
		enabled = nil
		for _, node := range info.Nodes {
			if slices.Contains(members, node) {
				enabled = append(enabled, node)
			}
		}
	}
	var nodes []string
	if slices.Contains(enabled, parkerNode) {
		nodes = append(nodes, parkerNode)
	}
	for _, node := range enabled {
		if node != "" && !slices.Contains(nodes, node) {
			nodes = append(nodes, node)
		}
	}
	if len(nodes) == 0 {
		if classified && len(info.Nodes) > 0 {
			return nil, errStorageNodesNoMember
		}
		return nil, cpierrors.Retriable("no cluster member is listed for storage %s", storage)
	}
	return nodes, nil
}

// errStorageNodesNoMember is what proofNodes returns when a storage's nodes
// list names no current cluster member. A retry doesn't change that, so the
// caller words the error for the operator who has to fix the list.
var errStorageNodesNoMember = errors.New("the storage's nodes list names no current cluster member")

// otherUnfinishedSlotsOn returns the slots of a parker the scan read that
// another disk's unfinished record names, or nil when the scan read no such
// parker.
func (p *recordPresence) otherUnfinishedSlotsOn(vmid int) map[string][]string {
	for i := range p.hit.parkers {
		if parker := &p.hit.parkers[i]; parker.vmid == vmid {
			return otherUnfinishedTransferSlots(p.stableID, parker.views.Applied(), parker.views.Current())
		}
	}
	return nil
}

// unfinishedRecordNodes returns the node of each parker the scan read that
// keeps an unfinished record of another disk naming volume, once each. A
// record is unfinished under the otherUnfinishedTransfers rule, and only
// while no guest the scan read carries its disk's serial.
func (p *recordPresence) unfinishedRecordNodes(volume string) []string {
	var nodes []string
	for i := range p.hit.parkers {
		parker := &p.hit.parkers[i]
		transfers := otherUnfinishedTransfers(p.stableID, parker.views.Applied(), parker.views.Current())
		for id := range transfers {
			if !p.hit.carriedSerials[id] && provEntryVolid(id, transfers[id]) == volume {
				nodes = unionNodes(nodes, []string{parker.node})
				break
			}
		}
	}
	return nodes
}

// snapshotOfSource says which snapshot of the record's source VM names volume
// on a drive line that carries this disk's serial or none, in words the
// refusal quotes, or returns "" with the reason it couldn't tell. A record
// that names no source VM has no snapshot to read. A source the scan didn't
// list is gone and has no snapshots, unless the scan left a node out, and
// then its snapshots can't be read. A source that's gone by the time of the
// read has none either.
func (p *recordPresence) snapshotOfSource(ctx context.Context, intent *DiskTransferIntent, volume string) (string, error) {
	vmid, err := strconv.Atoi(intent.SourceVMCID)
	if err != nil || vmid <= 0 {
		return "", nil
	}
	node, listed := p.hit.guestNodes[vmid]
	if !listed {
		if len(p.hit.excludedNodes) > 0 {
			return "", fmt.Errorf("source VM %d isn't on a node we could read, and node(s) %s are offline",
				vmid, strings.Join(p.hit.excludedNodes, ","))
		}
		return "", nil
	}
	name, key, failed, err := snapshotNamingVolume(ctx, p.c, node, vmid, volume, p.stableID)
	switch {
	case err != nil && parkerConfigGone(err):
		return "", nil
	case err != nil && failed == "":
		return "", fmt.Errorf("listing the snapshots of source VM %d on node %s: %w", vmid, node, err)
	case err != nil:
		return "", fmt.Errorf("reading snapshot %q of source VM %d on node %s: %w", failed, vmid, node, err)
	case name == "":
		return "", nil
	}
	return fmt.Sprintf("snapshot %q of source VM %d on node %s names that volume on %s", name, vmid, node, key), nil
}

// unionNodes returns the node names in a and then those in b that a doesn't
// have, once each.
func unionNodes(a, b []string) []string {
	var out []string
	for _, list := range [][]string{a, b} {
		for _, node := range list {
			if !slices.Contains(out, node) {
				out = append(out, node)
			}
		}
	}
	return out
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
//
// It reads every parker even after it finds a record, and it returns one
// intent for each parker that keeps one, in the order it read them. More than
// one means it can't tell which parker received the disk. A parker that the
// listing names twice is one parker.
func findParkedDiskIntentByStableID(
	ctx context.Context, c Client, stableID string, cfg ParkerConfig,
) ([]DiskTransferIntent, error) {
	var intents []DiskTransferIntent
	err := walkParkerRecords(ctx, c, cfg, func(p parkerCandidate, _ map[string]any, disks map[string]parkerProvEntry) bool {
		entry, ok := disks[stableID]
		if !ok {
			return false
		}
		for i := range intents {
			if intents[i].ParkerVMID == p.vmid {
				return false
			}
		}
		intents = append(intents, intentFromParkerEntry(p, entry))
		return false
	})
	if err != nil {
		return nil, err
	}
	return intents, nil
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
// It leaves out a record whose transfer has landed, which is one whose parker
// carries the record's serial on a disk key. It reads that from the same
// parker config it reads the record from, so a landed record costs no extra
// read, and a caller never resolves a disk whose move is already finished. A
// record whose transfer is still in flight names the volume by the name it has
// on the source VM.
func FindSourceTransferRecords(ctx context.Context, c Client, sourceVMCID string, cfg ParkerConfig) ([]SourceTransferRecord, error) {
	if c == nil {
		return nil, cpierrors.Cloud("FindSourceTransferRecords: client must not be nil")
	}
	if sourceVMCID == "" {
		return nil, cpierrors.Cloud("FindSourceTransferRecords: source VM CID must not be empty")
	}
	var out []SourceTransferRecord
	err := walkParkerRecords(ctx, c, cfg, func(p parkerCandidate, vmCfg map[string]any, disks map[string]parkerProvEntry) bool {
		for key := range disks {
			entry := disks[key]
			if !strings.HasPrefix(key, DiskStableIDPrefix) || entry.SourceVMCID != sourceVMCID || entry.Volid == "" {
				continue
			}
			if configCarriesSerial(vmCfg, key) {
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

// configCarriesSerial reports whether any disk key in vmCfg carries
// serial=<stableID>.
func configCarriesSerial(vmCfg map[string]any, stableID string) bool {
	for key := range vmCfg {
		if !isQemuDiskKey(key) {
			continue
		}
		text, _ := ConfigString(vmCfg, key)
		if serial, has := StableIDFromDriveOptStr(text); has && serial == stableID {
			return true
		}
	}
	return false
}

// walkParkerRecords reads every tagged parker in the band and passes each
// one's config and bosh_parked_disks map to visit, stopping early when visit
// returns true. A band that isn't usable has no parkers to read, which isn't an error.
// A parker whose config vanished between the listing and the read is skipped,
// and any other read failure stops the walk and comes back, because a record
// we never read must not count as a record that isn't there.
func walkParkerRecords(
	ctx context.Context, c Client, cfg ParkerConfig, visit func(parkerCandidate, map[string]any, map[string]parkerProvEntry) bool,
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
		if visit(p, vmCfg, disks) {
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
