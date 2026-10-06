package pve

// The resume's identity proof. A transfer's record keeps the pre-move volid
// until the finalize write, and a move that lands before its serial write
// leaves the record naming a volume PVE has already renamed off that name.
// Another disk can then take the freed name on the same source VM, and its own
// unfinished detach can leave it on an unused entry there. Neither an unused
// entry nor a landing carries a serial, so the resume can't tell the two disks
// apart by the source alone. These checks read the receiving side and the
// whole cluster the way the settlement of a move step does, and they run before
// the resume moves a volume or writes the disk's serial onto one.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// parkerLanding is one value of a parker's disk key, current or pending, that
// names a volume named for the parker and carries no stable-ID serial.
type parkerLanding struct {
	Key    string
	Volume string
}

// parkerUnclaimedLandings returns every value of a parker's disk keys, bus
// slot or unused entry, that names a volume named for the parker with no
// stable-ID serial. They come in key order, with a key's current value before
// its pending one, and a value both views share comes once.
func parkerUnclaimedLandings(views QemuViews, parkerVMID int) []parkerLanding {
	keys := make([]string, 0, len(views.entries))
	for key := range views.entries {
		if isQemuDiskKey(key) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var landings []parkerLanding
	for _, key := range keys {
		entry := views.entries[key]
		for _, value := range []struct {
			present bool
			text    string
		}{{entry.hasValue, entry.value}, {entry.hasPending, entry.pending}} {
			if !value.present {
				continue
			}
			volume := bareDriveVolid(value.text)
			if embedded, named := EmbeddedDiskVMID(volume); !named || embedded != parkerVMID {
				continue
			}
			if _, hasSerial := StableIDFromDriveOptStr(value.text); hasSerial {
				continue
			}
			landing := parkerLanding{Key: key, Volume: volume}
			if len(landings) == 0 || landings[len(landings)-1] != landing {
				landings = append(landings, landing)
			}
		}
	}
	return landings
}

// otherUnfinishedTransfers returns the unfinished transfer records of other
// disks on a parker, keyed by stable ID. A record is unfinished while no disk
// key of the parker carries its stable ID as a serial, which holds from its
// intent write until its serial write. The caller's own stableID never counts,
// and neither does a legacy record, because it names no slot. The records and
// the serials come from every configuration given, so a caller that has both
// views of the parker passes both, and a record found in an earlier one wins.
func otherUnfinishedTransfers(stableID string, configs ...map[string]any) map[string]parkerProvEntry {
	carried := make(map[string]bool)
	transfers := make(map[string]parkerProvEntry)
	for _, cfg := range configs {
		for key := range cfg {
			if !isQemuDiskKey(key) {
				continue
			}
			text, _ := ConfigString(cfg, key)
			if serial, has := StableIDFromDriveOptStr(text); has {
				carried[serial] = true
			}
		}
		_, records, _ := parseParkerSentinel(DescriptionFromConfig(cfg))
		for id := range records {
			if _, seen := transfers[id]; seen || id == stableID || records[id].Slot == "" || !strings.HasPrefix(id, DiskStableIDPrefix) {
				continue
			}
			transfers[id] = records[id]
		}
	}
	for id := range transfers {
		if carried[id] {
			delete(transfers, id)
		}
	}
	return transfers
}

// transferSlots maps each slot that a record in transfers names to the stable
// IDs of those records, in order.
func transferSlots(transfers map[string]parkerProvEntry) map[string][]string {
	slots := make(map[string][]string)
	for id := range transfers {
		slot := transfers[id].Slot
		slots[slot] = append(slots[slot], id)
	}
	for slot := range slots {
		sort.Strings(slots[slot])
	}
	return slots
}

// otherUnfinishedTransferSlots returns each parker slot that another disk's
// unfinished transfer record names, with the stable IDs of those disks in
// order, as otherUnfinishedTransfers defines them.
func otherUnfinishedTransferSlots(stableID string, configs ...map[string]any) map[string][]string {
	return transferSlots(otherUnfinishedTransfers(stableID, configs...))
}

// slotSet returns the slots of an otherUnfinishedTransferSlots answer as the
// exclude set chooseParkSlotExcluding takes.
func slotSet(slots map[string][]string) map[string]bool {
	set := make(map[string]bool, len(slots))
	for slot := range slots {
		set[slot] = true
	}
	return set
}

// resumeParker is the resume's read of its parker in both views. landings are
// the parker's unclaimed landings, less any set aside as the landing of another
// disk whose move the reader proved has run. others maps each slot that
// another disk's unfinished record names to those disks. neighbours are the
// other disks' unfinished transfers whose slots hold no landing and whose
// moves the reader can't rule out, because their sources still name their
// recorded volumes with no serial or because their status is unknown. ownCID
// is the disk CID this disk's own record keeps, and reader is the one reader
// the whole resume shares.
type resumeParker struct {
	views      QemuViews
	landings   []parkerLanding
	others     map[string][]string
	neighbours []transferStatus
	ownCID     string
	reader     *transferReader
}

// transferKind is what the resume could prove about one unfinished transfer.
type transferKind int

const (
	// transferUnknown is a transfer the reader can prove neither moved nor
	// unmoved. Its source is gone, its record names no source, a snapshot of
	// its source names its volume, its source names the volume under another
	// disk's serial, its volume still exists while no guest names it, or its
	// move reads as proved while some guest already carries its disk's serial.
	transferUnknown transferKind = iota
	// transferMoved is a transfer whose source exists and names the recorded
	// volume nowhere, in either view or any snapshot, while storage no longer
	// holds that volume or another guest names it now.
	transferMoved
	// transferUnmoved is a transfer whose source still names the recorded
	// volume with no serial or with the transfer's own serial.
	transferUnmoved
	// transferLandsByName is a transfer whose park attaches the recorded
	// volume under its own name, because the source doesn't own it, so a
	// volume named for the parker is never its landing.
	transferLandsByName
)

// transferStatus is the reader's answer for one unfinished transfer. bare is
// true for an unmoved transfer whose source names the volume on a key with no
// serial, and why says what the reader found, in words a refusal can quote.
type transferStatus struct {
	id, diskCID, slot string
	kind              transferKind
	bare              bool
	why               string
}

// transferSource is one read of a transfer's source VM. node is where the
// cluster finds it, and gone is true when the cluster can't find it at all.
type transferSource struct {
	views QemuViews
	node  string
	gone  bool
}

// transferReader classifies the unfinished transfers on the resume's parker.
// It reads each source VM at most once, on the node the cluster finds it on,
// and it proves a volume absent with ProveVolumeAbsent.
type transferReader struct {
	c        Client
	intent   DiskTransferIntent
	stableID string
	parker   QemuViews
	proof    *keepAbsenceProof
	sources  map[int]transferSource
}

func newTransferReader(c Client, intent DiskTransferIntent, stableID string, parker QemuViews, cfg ParkerConfig) *transferReader {
	return &transferReader{
		c: c, intent: intent, stableID: stableID, parker: parker,
		proof: newKeepAbsenceProof(c, cfg), sources: make(map[int]transferSource),
	}
}

// readResumeParker reads the resume's parker in both views and sorts out its
// landings and the slots other disks' unfinished records name.
//
// It refuses first when another unfinished record names the same source VM and
// recorded volume as this disk's, because one of the two names a volume that
// took the other's old name. When the parker holds a landing, it classifies
// every other unfinished transfer whose slot isn't this disk's. A landing on
// the slot of a transfer whose move it proved is set aside as that transfer's.
// A landing on the slot of any other transfer is never counted as that
// transfer's, so the resume refuses. It also refuses when a transfer whose
// move it proved has no landing on its slot while any landing is left, and
// when it set a landing aside while any landing is left, because only a serial
// could then say which landing is whose. Each of those refusals is permanent
// and asks for an audit. A failed read is retriable. The transfers whose slots
// hold no landing and whose moves it can't rule out come back as neighbours,
// for proveRecordedLanding's check of this disk's own move.
func readResumeParker(
	ctx context.Context, c Client, intent DiskTransferIntent, stableID string, cfg ParkerConfig,
) (resumeParker, error) {
	views, err := ReadQemuViews(ctx, c, intent.ParkerNode, intent.ParkerVMID)
	if err != nil {
		return resumeParker{}, cpierrors.Wrap(WrapConfigReadError(err),
			fmt.Sprintf("transfer resume: read parker vmid %d in both views", intent.ParkerVMID))
	}
	transfers := otherUnfinishedTransfers(stableID, views.Applied(), views.Current())
	parker := resumeParker{
		views: views, others: transferSlots(transfers), ownCID: recordedDiskCID(views, stableID),
		reader: newTransferReader(c, intent, stableID, views, cfg),
	}
	ids := make([]string, 0, len(transfers))
	for id := range transfers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		other := transfers[id]
		if intent.Volid != "" && other.Volid == intent.Volid && other.SourceVMCID == intent.SourceVMCID {
			return resumeParker{}, resumeProofRefusal(intent, stableID, fmt.Sprintf(
				"disk %s's unfinished record (disk cid %s, slot %q) names the same source vm %s and recorded volume %s "+
					"as this disk's record (disk cid %s, slot %q), so one of the two names a volume that took the other's "+
					"old name (audit required)",
				id, recordedCID(other.DiskCID), other.Slot, other.SourceVMCID, other.Volid,
				recordedCID(parker.ownCID), intent.Slot))
		}
	}
	landings := parkerUnclaimedLandings(views, intent.ParkerVMID)
	if len(landings) == 0 {
		return parker, nil
	}
	landed := make(map[string][]parkerLanding)
	for _, landing := range landings {
		landed[landing.Key] = append(landed[landing.Key], landing)
	}
	moved := make(map[string][]transferStatus)
	for _, id := range ids {
		if transfers[id].Slot == intent.Slot {
			continue
		}
		status, classifyErr := parker.reader.classify(ctx, id, transfers[id], "its", false)
		if classifyErr != nil {
			return resumeParker{}, classifyErr
		}
		switch {
		case status.kind == transferMoved:
			moved[status.slot] = append(moved[status.slot], status)
		case len(landed[status.slot]) > 0:
			landing := landed[status.slot][0]
			return resumeParker{}, resumeProofRefusal(intent, stableID, fmt.Sprintf(
				"%s on %s sits on the slot disk %s's unfinished record names (disk cid %s), but %s, so it can't count "+
					"as that disk's, and this disk's record (disk cid %s) names slot %q (audit required)",
				landing.Volume, landing.Key, id, recordedCID(status.diskCID), status.why,
				recordedCID(parker.ownCID), intent.Slot))
		case status.kind == transferUnknown || (status.kind == transferUnmoved && status.bare):
			parker.neighbours = append(parker.neighbours, status)
		}
	}
	slots := make([]string, 0, len(moved))
	for slot := range moved {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	var aside, unaccounted []transferStatus
	for _, slot := range slots {
		if len(landed[slot]) > 0 {
			aside = append(aside, moved[slot][0])
		}
		if len(moved[slot]) > len(landed[slot]) {
			unaccounted = append(unaccounted, moved[slot]...)
		}
	}
	for _, landing := range landings {
		if len(moved[landing.Key]) == 0 {
			parker.landings = append(parker.landings, landing)
		}
	}
	if len(parker.landings) == 0 {
		return parker, nil
	}
	left := parker.landings[0]
	if len(unaccounted) > 0 {
		other := unaccounted[0]
		return resumeParker{}, resumeProofRefusal(intent, stableID, fmt.Sprintf(
			"disk %s's unfinished record (disk cid %s, slot %q) reads as moved, because %s, and no landing on its slot "+
				"accounts for it, so %s on %s may be that disk's and not this disk's (disk cid %s, slot %q) (audit required)",
			other.id, recordedCID(other.diskCID), other.slot, other.why,
			left.Volume, left.Key, recordedCID(parker.ownCID), intent.Slot))
	}
	if len(aside) > 0 {
		other := aside[0]
		set := landed[other.slot][0]
		return resumeParker{}, resumeProofRefusal(intent, stableID, fmt.Sprintf(
			"%s on %s counts as disk %s's (disk cid %s, slot %q) only because its record reads as moved, since %s, "+
				"and no serial says so, so %s on %s may be that disk's and not this disk's (disk cid %s, slot %q) "+
				"(audit required)",
			set.Volume, set.Key, other.id, recordedCID(other.diskCID), other.slot, other.why,
			left.Volume, left.Key, recordedCID(parker.ownCID), intent.Slot))
	}
	return parker, nil
}

// classify reads what one unfinished transfer left behind and says whether its
// move is proved. whose is the possessive the answer's words use for the
// transfer, and own is true when the transfer is the resume's own, whose
// source may name its recorded volume under another disk's serial once that
// name was reused. Only positive evidence counts as moved. The source has to
// exist and name the recorded volume nowhere, in either view or any snapshot,
// and the volume has to be absent from storage or named by another guest now.
// For another disk's transfer, no guest may carry that disk's serial either,
// as staleRecord says. Anything the reader can't prove either way is unknown.
// A failed read returns an error, which is retriable unless it already
// carries a class.
func (r *transferReader) classify(
	ctx context.Context, id string, entry parkerProvEntry, whose string, own bool,
) (transferStatus, error) {
	status := transferStatus{id: id, diskCID: entry.DiskCID, slot: entry.Slot, kind: transferUnknown}
	srcVMID, convErr := strconv.Atoi(entry.SourceVMCID)
	if convErr != nil || srcVMID <= 0 || entry.Volid == "" {
		status.why = fmt.Sprintf("%s record names no source vm and volume", whose)
		return status, nil
	}
	if owner, named := EmbeddedDiskVMID(entry.Volid); !named || owner != srcVMID {
		status.kind = transferLandsByName
		status.why = fmt.Sprintf("%s recorded volume %s isn't named for %s source vm %d, so its park attaches it "+
			"under that name", whose, entry.Volid, whose, srcVMID)
		return status, nil
	}
	if r.parker.NamesVolume(entry.Volid) {
		status.kind = transferLandsByName
		status.why = fmt.Sprintf("parker vmid %d names %s recorded volume %s", r.intent.ParkerVMID, whose, entry.Volid)
		return status, nil
	}
	source, err := r.source(ctx, id, srcVMID)
	if err != nil {
		return status, err
	}
	if source.gone {
		status.why = fmt.Sprintf("%s source vm %d is gone from the cluster", whose, srcVMID)
		return status, nil
	}
	var keys []string
	bare := false
	otherKey, otherSerial := "", ""
	for _, key := range source.views.SlotsNaming(entry.Volid) {
		values := source.views.entries[key]
		for _, value := range []struct {
			present bool
			text    string
		}{{values.hasValue, values.value}, {values.hasPending, values.pending}} {
			if !value.present || bareDriveVolid(value.text) != entry.Volid {
				continue
			}
			serial, carries := StableIDFromDriveOptStr(value.text)
			switch {
			case carries && serial != id:
				if otherKey == "" {
					otherKey, otherSerial = key, serial
				}
			default:
				bare = bare || !carries
				if len(keys) == 0 || keys[len(keys)-1] != key {
					keys = append(keys, key)
				}
			}
		}
	}
	if len(keys) > 0 {
		status.kind, status.bare = transferUnmoved, bare
		status.why = fmt.Sprintf("%s source vm %d still names %s recorded volume %s on %s",
			whose, srcVMID, whose, entry.Volid, strings.Join(keys, ", "))
		return status, nil
	}
	if otherKey != "" {
		if own {
			status.kind = transferMoved
			status.why = fmt.Sprintf("%s source vm %d names %s recorded volume %s only on %s, under another disk's serial %s",
				whose, srcVMID, whose, entry.Volid, otherKey, otherSerial)
			return status, nil
		}
		status.why = fmt.Sprintf("%s source vm %d names %s recorded volume %s on %s under another disk's serial %s",
			whose, srcVMID, whose, entry.Volid, otherKey, otherSerial)
		return status, nil
	}
	snapshot, snapshotKey, err := r.snapshotNaming(ctx, id, srcVMID, source.node, entry.Volid)
	if err != nil {
		return status, err
	}
	if snapshot != "" {
		status.why = fmt.Sprintf("snapshot %q of %s source vm %d names %s recorded volume %s on %s",
			snapshot, whose, srcVMID, whose, entry.Volid, snapshotKey)
		return status, nil
	}
	status, err = r.volumeFate(ctx, status, id, entry.Volid, whose, srcVMID, source.node)
	if err != nil || status.kind != transferMoved || own {
		return status, err
	}
	return r.staleRecord(ctx, status, id, entry.Volid, whose)
}

// staleRecord finishes classify for another disk's transfer whose move reads
// as proved. When some guest already carries that disk's stable ID as a
// serial, the transfer finished elsewhere, and the record is stale. An unpark
// that failed after its rename leaves a record like that, and it reads as
// moved, because PVE renamed the volume off the recorded name. Nothing then
// ties a landing on that record's slot to the record, so the transfer reads
// as unknown. The resume's own transfer never comes here, because the proof
// refuses any guest that carries this disk's serial before it claims or moves
// anything.
//
// A guest that carries the serial can also be the parker itself. The parker
// read showed the record unfinished, so the serial arrived after that read,
// which means another call finished the transfer in the meantime. That isn't
// a stale record, and a rerun settles it, so the resume refuses retriably.
func (r *transferReader) staleRecord(ctx context.Context, status transferStatus, id, volid, whose string) (transferStatus, error) {
	holders, err := FindDiskKeyHolders(ctx, r.c, volid, id)
	if err != nil {
		return status, resumeReadFailure(err,
			fmt.Sprintf("transfer resume: read the guests that hold disk %s or volume %s", id, volid))
	}
	for _, holder := range holders {
		if holder.CarriesSerial && holder.VMID == r.intent.ParkerVMID {
			return status, cpierrors.Retriable(
				"transfer resume: parker vmid %d changed after the resume read it, "+
					"because it now carries disk %s's serial on %s; retry",
				r.intent.ParkerVMID, id, holder.Slot)
		}
	}
	for _, holder := range holders {
		if holder.CarriesSerial {
			status.kind = transferUnknown
			status.why = fmt.Sprintf("vm %d carries %s disk's serial on %s, which shows %s record is stale",
				holder.VMID, whose, holder.Slot, whose)
			return status, nil
		}
	}
	return status, nil
}

// volumeFate finishes classify for a source that names the recorded volume
// nowhere. Another guest naming the volume proves the name was reused. A volume
// that ProveVolumeAbsent finds gone shows that PVE renamed it off that name. A
// volume that's still there while no guest names it is unknown, because a
// released volume and a stale record both leave it so.
func (r *transferReader) volumeFate(
	ctx context.Context, status transferStatus, id, volid, whose string, srcVMID int, node string,
) (transferStatus, error) {
	refs, err := FindVolumeReferences(ctx, r.c, volid)
	if err != nil {
		return status, resumeReadFailure(err,
			fmt.Sprintf("transfer resume: read the guests that name volume %s of disk %s's transfer", volid, id))
	}
	for _, ref := range refs {
		if ref.VMID != srcVMID {
			status.kind = transferMoved
			status.why = fmt.Sprintf("%s source vm %d names %s recorded volume %s nowhere, and vm %d names it on %s now",
				whose, srcVMID, whose, volid, ref.VMID, ref.Slot)
			return status, nil
		}
	}
	storage, _, err := ParseDiskCID(volid)
	if err != nil {
		status.why = fmt.Sprintf("%s recorded volume %s names no storage", whose, volid)
		return status, nil
	}
	absent, err := ProveVolumeAbsent(ctx, r.c, node, storage, volid, r.proof.classifier(storage), r.proof.secondOpinions()...)
	if err != nil {
		return status, resumeReadFailure(err,
			fmt.Sprintf("transfer resume: prove volume %s of disk %s's transfer absent from storage %s on node %s", volid, id, storage, node))
	}
	if absent {
		status.kind = transferMoved
		status.why = fmt.Sprintf("%s source vm %d names %s recorded volume %s nowhere, and storage %s no longer holds it",
			whose, srcVMID, whose, volid, storage)
		return status, nil
	}
	status.why = fmt.Sprintf("%s recorded volume %s still exists on storage %s and no guest names it", whose, volid, storage)
	return status, nil
}

// source returns the read of a transfer's source VM, reading it the first time
// it's asked for.
func (r *transferReader) source(ctx context.Context, id string, vmid int) (transferSource, error) {
	if source, read := r.sources[vmid]; read {
		return source, nil
	}
	source, err := r.readSource(ctx, id, vmid)
	if err != nil {
		return transferSource{}, err
	}
	r.sources[vmid] = source
	return source, nil
}

// readSource reads a transfer's source VM on the parker's node, where the
// transfer ran. A source that reads as gone there is looked up in the cluster
// and read on the node it's found on, because this read only inspects, so a
// migrated source is an ordinary answer. A source the cluster can't find is
// gone. A failed read keeps a class it already carries and is otherwise
// retriable.
func (r *transferReader) readSource(ctx context.Context, id string, vmid int) (transferSource, error) {
	views, err := ReadQemuViews(ctx, r.c, r.intent.ParkerNode, vmid)
	if err == nil {
		return transferSource{views: views, node: r.intent.ParkerNode}, nil
	}
	if !parkerConfigGone(err) {
		return transferSource{}, resumeReadFailure(err,
			fmt.Sprintf("transfer resume: read source vm %d of disk %s's transfer", vmid, id))
	}
	location, err := FindVMAuthoritative(ctx, r.c, vmid)
	if err != nil {
		return transferSource{}, resumeReadFailure(err,
			fmt.Sprintf("transfer resume: find source vm %d of disk %s's transfer in the cluster", vmid, id))
	}
	if !location.Found {
		return transferSource{gone: true}, nil
	}
	if location.Node == r.intent.ParkerNode {
		return transferSource{}, cpierrors.Retriable(
			"transfer resume: source vm %d of disk %s's transfer read as gone on node %s, but the cluster still finds it there; retry",
			vmid, id, r.intent.ParkerNode)
	}
	views, err = ReadQemuViews(ctx, r.c, location.Node, vmid)
	if err != nil {
		return transferSource{}, resumeReadFailure(err,
			fmt.Sprintf("transfer resume: read source vm %d of disk %s's transfer on node %s", vmid, id, location.Node))
	}
	return transferSource{views: views, node: location.Node}, nil
}

// snapshotNaming returns the first snapshot of a source VM, and its key, that
// names volid, or empty strings when none does. A failed read keeps a class
// it already carries and is otherwise retriable.
func (r *transferReader) snapshotNaming(ctx context.Context, id string, vmid int, node, volid string) (string, string, error) {
	name, key, failed, err := snapshotNamingVolume(ctx, r.c, node, vmid, volid, "")
	switch {
	case err == nil:
		return name, key, nil
	case failed == "":
		return "", "", resumeReadFailure(err,
			fmt.Sprintf("transfer resume: list the snapshots of source vm %d of disk %s's transfer", vmid, id))
	default:
		return "", "", resumeReadFailure(err,
			fmt.Sprintf("transfer resume: read snapshot %q of source vm %d of disk %s's transfer", failed, vmid, id))
	}
}

// snapshotNamingVolume returns the first snapshot of a VM, in name order, and
// the first of its drive or vmstate keys, in key order, that names volid, or
// empty strings when none does. It lists and reads the snapshots the way
// refuseSnapshotNamingVolume does. Only the resolver checks the serial: when
// serial isn't empty, a key counts only when its value carries that stable-ID
// serial or none, so a snapshot line of another disk that took the volume's
// name doesn't count. The transfer resume passes no serial and counts every
// line, as the park gate does. When a read fails, failed names the
// snapshot whose configuration couldn't be read, and it is empty when the
// listing itself failed. The error is the read's own, with no class added.
func snapshotNamingVolume(
	ctx context.Context, c Client, node string, vmid int, volid, serial string,
) (name, key, failed string, err error) {
	names, err := HasSnapshots(ctx, c, node, vmid)
	if err != nil {
		return "", "", "", err
	}
	sort.Strings(names)
	for _, snapshot := range names {
		cfg, cfgErr := SnapshotConfig(ctx, c, node, vmid, snapshot)
		if cfgErr != nil {
			return "", "", snapshot, cfgErr
		}
		keys := make([]string, 0, len(cfg))
		for k := range cfg {
			if isQemuDiskKey(k) || k == "vmstate" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			text, ok := ConfigStringValue(cfg[k])
			if !ok || bareDriveVolid(text) != volid {
				continue
			}
			if found, hasSerial := StableIDFromDriveOptStr(text); serial != "" && hasSerial && found != serial {
				continue
			}
			return snapshot, k, "", nil
		}
	}
	return "", "", "", nil
}

// recordedDiskCID returns the disk CID that stableID's record on the parker
// keeps, from whichever view has one.
func recordedDiskCID(views QemuViews, stableID string) string {
	for _, cfg := range []map[string]any{views.Applied(), views.Current()} {
		_, records, _ := parseParkerSentinel(DescriptionFromConfig(cfg))
		if record, ok := records[stableID]; ok && record.DiskCID != "" {
			return record.DiskCID
		}
	}
	return ""
}

// recordedCID is a disk CID as a refusal quotes it.
func recordedCID(cid string) string {
	if cid == "" {
		return "not recorded"
	}
	return cid
}

// resumeReadFailure classifies a read the resume made only to inspect. An
// error that already carries a CPI class keeps it, so a permanent finding
// stays permanent, and any other failed read is retriable, because reading
// again may answer.
func resumeReadFailure(err error, msg string) error {
	var typed *cpierrors.Error
	if errors.As(err, &typed) {
		return cpierrors.Wrap(err, msg)
	}
	return cpierrors.WrapAs(err, cpierrors.TypeRetriableCloud, msg)
}

// proveSourceNamesDisk runs before the move window touches a source that names
// the recorded volid on any key, so it comes ahead of a found pending delete
// the window would apply as well as the move. It refuses when a source key
// naming the volume carries another disk's serial, when a parker other than
// the resume's keeps a record of the disk's transfer, and when any guest
// carries the disk's serial or any parker names the volume. The source's own
// keys that name the volume are left to this check and to proveSourceKey,
// which governs the one the move takes.
func proveSourceNamesDisk(
	ctx context.Context, c Client, intent DiskTransferIntent, stableID string, srcVMID int, srcViews QemuViews,
) error {
	for _, key := range srcViews.SlotsNaming(intent.Volid) {
		entry := srcViews.entries[key]
		for _, value := range []struct {
			present bool
			text    string
		}{{entry.hasValue, entry.value}, {entry.hasPending, entry.pending}} {
			if !value.present || bareDriveVolid(value.text) != intent.Volid {
				continue
			}
			if found, carries := StableIDFromDriveOptStr(value.text); carries && found != stableID {
				return resumeProofRefusal(intent, stableID,
					fmt.Sprintf("%s of source vm %d names volume %s with another disk's serial %s", key, srcVMID, intent.Volid, found))
			}
		}
	}
	if err := refuseOtherTransferRecords(ctx, c, intent, stableID); err != nil {
		return err
	}
	return refuseOtherDiskHolders(ctx, c, intent, stableID, intent.Volid, func(holder DiskKeyHolder) bool {
		return holder.VMID == srcVMID && holder.NamesVolume
	})
}

// proveSourceKey is the move window's check on the key it's about to move. The
// key has to be the only one of the source that names the volume in either
// view, and its applied value has to name it with no pending delete and no
// pending replacement. appliedSlot is the bus slot whose pending delete the
// window applied first, if it applied one, and a refusal says so.
func proveSourceKey(intent DiskTransferIntent, stableID string, srcVMID int, srcViews QemuViews, key, appliedSlot string) error {
	done := resumeProofNothingDone
	if appliedSlot != "" {
		done = fmt.Sprintf("It applied the pending delete of %s on source vm %d, but it moved nothing and wrote no serial", appliedSlot, srcVMID)
	}
	keys := srcViews.SlotsNaming(intent.Volid)
	if len(keys) != 1 || keys[0] != key {
		return resumeProofRefusalAfter(intent, stableID,
			fmt.Sprintf("disk keys %s of source vm %d all name volume %s", strings.Join(keys, ", "), srcVMID, intent.Volid), done)
	}
	if !MoveSourceStillNames(srcViews, key, intent.Volid) {
		return resumeProofRefusalAfter(intent, stableID,
			fmt.Sprintf("%s of source vm %d has a pending change to volume %s", key, srcVMID, intent.Volid), done)
	}
	return nil
}

// proveRecordedLanding is the check before the resume claims a landing and
// writes the disk's serial onto it. No other disk's unfinished record on the
// parker may name the slot this record names, because that disk's move could
// have landed there. No other parker may keep a record of the disk's transfer,
// no guest may carry the disk's serial, and no other key of a parker may name
// the landed volume. The disk's own transfer can't be one whose park attaches
// the recorded volume under its own name, because a landing under any other
// name is never that disk's. Once we leave out the landings readResumeParker
// set aside for other disks' transfers whose moves it proved, the landing has
// to meet three conditions. It has to be the only volume on the parker, across
// both views, that's named for the parker and carries no serial. It has to sit
// on the slot the record names. And that slot's applied value has to name it
// with no pending delete and no pending replacement. refuseUnprovedOwnMove then
// refuses while another transfer's move can't be ruled out and this disk's own
// move can't be proved. It returns the landed volume.
func proveRecordedLanding(ctx context.Context, c Client, intent DiskTransferIntent, stableID string, parker resumeParker) (string, error) {
	if ids := parker.others[intent.Slot]; len(ids) > 0 {
		return "", resumeProofRefusal(intent, stableID,
			fmt.Sprintf("parker vmid %d also keeps an unfinished transfer record of disk %s naming slot %q",
				intent.ParkerVMID, strings.Join(ids, ", "), intent.Slot))
	}
	views, landings := parker.views, parker.landings
	landing := landings[0]
	for _, other := range landings[1:] {
		if other != landing {
			return "", resumeProofRefusal(intent, stableID,
				fmt.Sprintf("parker vmid %d holds more than one volume named for it with no serial, %s on %s and %s on %s",
					intent.ParkerVMID, landing.Volume, landing.Key, other.Volume, other.Key))
		}
	}
	if landing.Key != intent.Slot {
		return "", resumeProofRefusal(intent, stableID,
			fmt.Sprintf("%s of parker vmid %d holds %s, a volume named for the parker with no serial, while the record names slot %q",
				landing.Key, intent.ParkerVMID, landing.Volume, intent.Slot))
	}
	if !MoveSourceStillNames(views, landing.Key, landing.Volume) {
		return "", resumeProofRefusal(intent, stableID,
			fmt.Sprintf("%s of parker vmid %d has a pending change to volume %s", landing.Key, intent.ParkerVMID, landing.Volume))
	}
	if srcVMID, convErr := strconv.Atoi(intent.SourceVMCID); convErr == nil && srcVMID > 0 && intent.Volid != "" {
		if owner, named := EmbeddedDiskVMID(intent.Volid); !named || owner != srcVMID {
			return "", resumeProofRefusal(intent, stableID, fmt.Sprintf(
				"%s on %s is named for the parker, while this disk's recorded volume %s isn't named for its source vm %d, "+
					"so this disk's park attaches that volume under its own name and %s isn't this disk's (disk cid %s)",
				landing.Volume, landing.Key, intent.Volid, srcVMID, landing.Volume, recordedCID(parker.ownCID)))
		}
	}
	if err := refuseUnprovedOwnMove(ctx, intent, stableID, parker, landing); err != nil {
		return "", err
	}
	if err := refuseOtherTransferRecords(ctx, c, intent, stableID); err != nil {
		return "", err
	}
	err := refuseOtherDiskHolders(ctx, c, intent, stableID, landing.Volume, func(holder DiskKeyHolder) bool {
		return holder.VMID == intent.ParkerVMID && holder.Slot == landing.Key
	})
	if err != nil {
		return "", err
	}
	return landing.Volume, nil
}

// refuseUnprovedOwnMove runs before the resume claims a landing on its own
// recorded slot while another disk's transfer has no landing on its slot and
// the reader can't rule out its move, because its source still names its
// recorded volume with no serial or its status is unknown. An older release's
// fallback could land that other disk on this slot, and a new disk could then
// take that disk's old name on its source, so that disk only looks unmoved.
// The claim goes ahead only when the reader proves this disk's own move ran.
// Its source counts as naming the recorded volume only on keys with no serial
// or with this disk's serial, because a key with another disk's serial shows
// the name was reused. Anything short of that proof leaves nothing in the
// metadata to say whose landing it is, so the refusal is permanent and asks
// for an audit. A failed read is retriable.
func refuseUnprovedOwnMove(ctx context.Context, intent DiskTransferIntent, stableID string, parker resumeParker, landing parkerLanding) error {
	if len(parker.neighbours) == 0 {
		return nil
	}
	own := parkerProvEntry{DiskCID: parker.ownCID, SourceVMCID: intent.SourceVMCID, Volid: intent.Volid, Slot: intent.Slot}
	status, err := parker.reader.classify(ctx, stableID, own, "this disk's", true)
	if err != nil {
		return err
	}
	if status.kind == transferMoved {
		return nil
	}
	other := parker.neighbours[0]
	return resumeProofRefusal(intent, stableID, fmt.Sprintf(
		"%s on %s, the slot this record names, may be this disk's (disk cid %s) or disk %s's (disk cid %s), because %s, "+
			"while disk %s's unfinished record names slot %q, which holds no landing, and %s (audit required)",
		landing.Volume, landing.Key, recordedCID(parker.ownCID), other.id, recordedCID(other.diskCID), status.why,
		other.id, other.slot, other.why))
}

// proveReleasedVolume runs before the resume attaches a volume its source
// released onto the parker with the disk's serial baked in. No other parker
// may keep a record of the disk's transfer, no guest may carry the disk's
// serial, and no guest at all may name the volume, because this window's
// premise is that nothing does. A source that migrated to another node still
// names it there.
func proveReleasedVolume(ctx context.Context, c Client, intent DiskTransferIntent, stableID string) error {
	if err := refuseOtherTransferRecords(ctx, c, intent, stableID); err != nil {
		return err
	}
	holders, err := FindDiskKeyHolders(ctx, c, intent.Volid, stableID)
	if err != nil {
		return cpierrors.Wrap(err, fmt.Sprintf("transfer resume: read the guests that hold disk %s or volume %s", stableID, intent.Volid))
	}
	for _, holder := range holders {
		if holder.CarriesSerial {
			return resumeProofRefusal(intent, stableID, fmt.Sprintf("vm %d holds the disk's serial on %s", holder.VMID, holder.Slot))
		}
		if holder.NamesVolume {
			return resumeProofRefusal(intent, stableID, fmt.Sprintf("vm %d names volume %s on %s", holder.VMID, intent.Volid, holder.Slot))
		}
	}
	return nil
}

// resumeSourceGone reports whether a failed read of the source proves the
// source VM gone. The resume reads the source on the parker's node, and a
// source that migrated answers there the way a destroyed one does, so the
// answer counts only when FindVMAuthoritative can't find the VM anywhere in
// the cluster either. When it finds the source on another node, it refuses,
// because the volume moved with the VM and a move between nodes isn't possible.
// A source it still finds on the parker's node, or a lookup that can't prove
// absence, is retriable.
func resumeSourceGone(ctx context.Context, c Client, intent DiskTransferIntent, stableID string, srcVMID int, readErr error) (bool, error) {
	if !parkerConfigGone(readErr) {
		return false, nil
	}
	location, err := FindVMAuthoritative(ctx, c, srcVMID)
	if err != nil {
		return false, cpierrors.Wrap(err, fmt.Sprintf("transfer resume: confirm source vm %d is gone", srcVMID))
	}
	if !location.Found {
		return true, nil
	}
	if location.Node != intent.ParkerNode {
		return false, resumeProofRefusal(intent, stableID,
			fmt.Sprintf("source vm %d has no configuration on node %s and the cluster finds it on node %s", srcVMID, intent.ParkerNode, location.Node))
	}
	return false, cpierrors.Retriable(
		"transfer resume: source vm %d read as gone on node %s, but the cluster still finds it there; retry",
		srcVMID, intent.ParkerNode)
}

// refuseOtherTransferRecords refuses when a parker other than the resume's
// keeps a record of the disk's transfer, because the resume then can't tell
// which parker received the disk. A failed read keeps its class and never
// counts as no other record.
func refuseOtherTransferRecords(ctx context.Context, c Client, intent DiskTransferIntent, stableID string) error {
	records, err := FindDiskTransferRecords(ctx, c, stableID)
	if err != nil {
		return cpierrors.Wrap(err, fmt.Sprintf("transfer resume: read the parkers' records of disk %s", stableID))
	}
	for _, record := range records {
		if record.ParkerVMID != intent.ParkerVMID {
			return resumeProofRefusal(intent, stableID,
				fmt.Sprintf("parker vmid %d keeps a record of the disk's transfer as well as parker vmid %d", record.ParkerVMID, intent.ParkerVMID))
		}
	}
	return nil
}

// refuseOtherDiskHolders refuses when a disk key that own doesn't claim
// carries the disk's serial, or when a disk key of a parker names the given
// volume. Those are the same rules the settlement of a move applies to the
// guests around it. A failed read keeps its class and never counts as no
// holder.
func refuseOtherDiskHolders(
	ctx context.Context, c Client, intent DiskTransferIntent, stableID, volume string, own func(DiskKeyHolder) bool,
) error {
	holders, err := FindDiskKeyHolders(ctx, c, volume, stableID)
	if err != nil {
		return cpierrors.Wrap(err, fmt.Sprintf("transfer resume: read the guests that hold disk %s or volume %s", stableID, volume))
	}
	for _, holder := range holders {
		if own(holder) {
			continue
		}
		if holder.CarriesSerial {
			return resumeProofRefusal(intent, stableID, fmt.Sprintf("vm %d holds the disk's serial on %s", holder.VMID, holder.Slot))
		}
		if holder.Parker && holder.NamesVolume {
			return resumeProofRefusal(intent, stableID, fmt.Sprintf("parker vmid %d names volume %s on %s", holder.VMID, volume, holder.Slot))
		}
	}
	return nil
}

// resumeProofRunbookHeading is the docs/troubleshooting.md heading a refusal
// points the operator at.
const resumeProofRunbookHeading = "A transfer resume can't prove which volume is the disk"

// resumeProofNothingDone is what a refusal says the resume did when it
// changed nothing.
const resumeProofNothingDone = "It moved nothing and wrote no serial"

// resumeProofRefusal is the permanent error a failed proof returns. Retrying
// can't change what it found, so it names the finding and leaves the state for
// an operator to inspect.
func resumeProofRefusal(intent DiskTransferIntent, stableID, finding string) error {
	return resumeProofRefusalAfter(intent, stableID, finding, resumeProofNothingDone)
}

// resumeProofRefusalAfter is resumeProofRefusal for a refusal that follows a
// change the resume made, which done describes.
func resumeProofRefusalAfter(intent DiskTransferIntent, stableID, finding, done string) error {
	return cpierrors.Cloud(
		"transfer resume: disk %s has an intent record on parker vmid %d (node %s, slot %q, recorded volid %q, source %q), "+
			"but %s, so the resume can't prove which volume is this disk. %s; "+
			"see %q in docs/troubleshooting.md of bosh-proxmox-cpi-release",
		stableID, intent.ParkerVMID, intent.ParkerNode, intent.Slot, intent.Volid, intent.SourceVMCID, finding, done,
		resumeProofRunbookHeading)
}
