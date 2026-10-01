package pve

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// provenanceClockKey carries the clock WithProvenanceClock installs.
type provenanceClockKey struct{}

// WithProvenanceClock returns a derived context whose provenance writes stamp
// parked_at from now, and whose collector judges record ages against it. It
// lets a test age a transfer intent through the real handler path instead of
// editing a parker's description by hand.
//
// Production code MUST NOT call this. A ParkerConfig.NowFunc, when set, still
// wins, so the tests that pin the clock that way are unaffected.
func WithProvenanceClock(ctx context.Context, now func() time.Time) context.Context {
	return context.WithValue(ctx, provenanceClockKey{}, now)
}

// provenanceNow returns the time a provenance write stamps and collects
// against: cfg.NowFunc when set, then the context clock, then the wall clock.
func provenanceNow(ctx context.Context, cfg ParkerConfig) time.Time {
	if cfg.NowFunc != nil {
		return cfg.NowFunc()
	}
	if ctx != nil {
		if now, ok := ctx.Value(provenanceClockKey{}).(func() time.Time); ok && now != nil {
			return now().UTC()
		}
	}
	return time.Now().UTC()
}

// provenanceSourceVMID returns the source VM a record names when the record is
// source-bearing: a stable-ID record (only those carry a volid) whose
// source_vm_cid parses as a positive VMID. Every transfer intent a guest's
// detach, delete_vm, or ephemeral retention writes is one, in every release
// that collects records. Other records keep the age-and-reference rule alone.
func provenanceSourceVMID(entry parkerProvEntry) (int, bool) {
	if entry.Volid == "" || entry.SourceVMCID == "" {
		return 0, false
	}
	vmid, err := strconv.Atoi(entry.SourceVMCID)
	if err != nil || vmid <= 0 {
		return 0, false
	}
	return vmid, true
}

// parkerProvenanceSourceKeeps is the one keep rule both the capacity probe and
// the provenance write apply, so the two can never disagree about which
// records survive. It returns the keys of records that would otherwise be
// collected but whose source VM still names the record's volume on any slot,
// active or unused.
//
// That state is a detach-side transfer that deleted the source slot and then
// failed to move the volume. PVE demoted the volume to an unused entry, which
// carries no serial, so the intent on this parker is the only link from the
// disk's stable ID to its volume, and collecting it strands the disk where no
// CID can find it.
//
// A record whose source no longer names the volume is kept too, while the
// volume still exists and no guest names it in either view. That's a transfer
// of a volume the source doesn't own, which PVE drops from the source without
// an unused entry, cut off between the slot delete and the parker attach. The
// record is again the only link to the volume. A guest that names it releases
// the record, because the disk lives there now. A proven absence releases it
// too. This needs no ownership test, because an owned volume its source no
// longer names was either renamed by a finished move or freed, and the
// absence proof settles both.
//
// Reads run only for source-bearing collection candidates, so a parker with no
// stale intent costs nothing extra. Any read that fails keeps the record: a
// kept record only delays a collection, and a wrongly collected one loses a
// disk.
func parkerProvenanceSourceKeeps(
	ctx context.Context, c Client, logger *log.Logger,
	vmCfg map[string]any, keepKey string, now time.Time, cfg ParkerConfig,
) map[string]bool {
	_, disks, _ := parseParkerSentinel(DescriptionFromConfig(vmCfg))
	held := map[string]bool{}
	proof := newKeepAbsenceProof(c, cfg)
	for _, key := range staleParkerProvenanceKeys(disks, vmCfg, keepKey, now) {
		entry := disks[key]
		vmid, ok := provenanceSourceVMID(entry)
		if !ok {
			continue
		}
		names, err := provenanceSourceNamesVolume(ctx, c, entry, vmid)
		if err == nil && !names {
			names, err = provenanceVolumeUnclaimed(ctx, c, entry, proof)
		}
		if err != nil && logger != nil {
			logger.Warn("parker provenance: could not tell whether a stale transfer record still links its volume; keeping the record",
				log.String("key", key),
				log.Int("source_vmid", vmid),
				log.String("volid", entry.Volid),
				log.Err(err),
			)
		}
		if names || err != nil {
			held[key] = true
		}
	}
	return held
}

// provenanceSourceNamesVolume reports whether the source VM names entry.Volid
// on an active slot or an unused entry, in either the current or the pending
// view, so a source whose delete is still pending keeps its record. An error
// means the answer is unknown, and the caller keeps the record.
//
// The record's node is where the transfer ran, which is where the source was
// then, not necessarily where it is now. So a config that is gone at that node
// is not yet proof of absence: FindVMAuthoritative either proves the guest is
// gone cluster-wide or finds it on another node, and only proven absence lets
// the record go.
func provenanceSourceNamesVolume(ctx context.Context, c Client, entry parkerProvEntry, vmid int) (bool, error) {
	if entry.Node != "" {
		views, err := ReadQemuViews(ctx, c, entry.Node, vmid)
		if err == nil {
			return views.NamesVolume(entry.Volid), nil
		}
		if !parkerConfigGone(err) {
			return false, err
		}
	}
	loc, err := FindVMAuthoritative(ctx, c, vmid)
	if err != nil {
		return false, err
	}
	if !loc.Found {
		return false, nil
	}
	views, err := ReadQemuViews(ctx, c, loc.Node, vmid)
	if err != nil {
		return false, err
	}
	return views.NamesVolume(entry.Volid), nil
}

// keepAbsenceProof is what one keep pass shares across its candidates when it
// proves a volume gone. Each storage is classified at most once per pass,
// through LiveStorageInfo, the same source the handlers' classifier reads, and
// any error leaves that storage unclassified, so ProveVolumeAbsent refuses and
// the record stays. The second opinions come from the parker config's
// supplier, which is the one the local backend's sweep uses, and they're
// built at most once per pass too.
type keepAbsenceProof struct {
	client         Client
	supply         func() []EmptyListingCorroborator
	classified     map[string]StorageInfo
	unclassified   map[string]bool
	corroborators  []EmptyListingCorroborator
	corroboratorOK bool
}

func newKeepAbsenceProof(c Client, cfg ParkerConfig) *keepAbsenceProof {
	return &keepAbsenceProof{client: c, supply: cfg.EmptyListingCorroborators, classified: map[string]StorageInfo{}, unclassified: map[string]bool{}}
}

// classifier returns the memoized classifier for storage.
func (p *keepAbsenceProof) classifier(storage string) StorageClassifier {
	return func(ctx context.Context) (StorageInfo, bool) {
		if info, ok := p.classified[storage]; ok {
			return info, true
		}
		if p.unclassified[storage] {
			return StorageInfo{}, false
		}
		info, err := LiveStorageInfo(ctx, p.client, storage)
		if err != nil {
			p.unclassified[storage] = true
			return StorageInfo{}, false
		}
		p.classified[storage] = info
		return info, true
	}
}

// secondOpinions returns the pass's corroborators, building them once. With
// no supplier it returns the one corroborator that always errors, so an empty
// listing, which is the case corroboration exists for, never reads as an
// absence. A wrongly collected record loses a disk, while a kept one only
// delays a collection.
func (p *keepAbsenceProof) secondOpinions() []EmptyListingCorroborator {
	if !p.corroboratorOK {
		p.corroboratorOK = true
		if p.supply != nil {
			p.corroborators = p.supply()
		} else {
			p.corroborators = []EmptyListingCorroborator{noSecondOpinion}
		}
	}
	return p.corroborators
}

// noSecondOpinion answers every empty-listing question with an error, because
// the parker config supplied no second opinion to ask.
var noSecondOpinion = CorroboratorFunc("parker keep rule",
	func(context.Context, EmptyListingProbe) (Corroboration, error) {
		return Corroboration{}, fmt.Errorf("no second opinion was supplied for an empty content listing, so it can't prove the volume gone")
	})

// provenanceVolumeUnclaimed reports whether entry.Volid still exists while no
// guest in the cluster names it in either view, which is when a record whose
// source let go of the volume is the volume's only link. A guest that names
// it, or a proof that the volume is gone, answers false. An error means the
// answer is unknown, and the caller keeps the record.
//
// FindVolumeReferences counts no per-storage references, so there's no
// config-reference second opinion to add here, unlike a handler that ran the
// identity scan.
func provenanceVolumeUnclaimed(ctx context.Context, c Client, entry parkerProvEntry, proof *keepAbsenceProof) (bool, error) {
	refs, err := FindVolumeReferences(ctx, c, entry.Volid)
	if err != nil {
		return false, err
	}
	if len(refs) > 0 {
		return false, nil
	}
	if entry.Node == "" {
		return false, fmt.Errorf("record for %q names no node to prove its volume absent from", entry.Volid)
	}
	storage, _, err := ParseDiskCID(entry.Volid)
	if err != nil {
		return false, err
	}
	absent, err := ProveVolumeAbsent(ctx, c, entry.Node, storage, entry.Volid, proof.classifier(storage), proof.secondOpinions()...)
	if err != nil {
		return false, err
	}
	return !absent, nil
}

// staleParkerProvenanceKeys returns, sorted, the keys the age-and-reference
// rule would collect: every record other than keepKey whose volume nothing on
// the parker names and whose parked_at is older than the grace window or does
// not parse. The collector and the keep rule share it, so the source reads run
// for exactly the records the collector would otherwise remove.
func staleParkerProvenanceKeys(disks map[string]parkerProvEntry, vmCfg map[string]any, keepKey string, now time.Time) []string {
	referenced := parkerReferencedVolids(vmCfg)
	var stale []string
	for key := range disks {
		entry := disks[key]
		if key == keepKey {
			continue
		}
		if referenced[provEntryVolid(key, entry)] {
			continue
		}
		if parsed, parseErr := time.Parse(time.RFC3339, entry.ParkedAt); parseErr == nil {
			if now.Sub(parsed) < parkerProvenanceGraceWindow {
				continue
			}
		}
		stale = append(stale, key)
	}
	sort.Strings(stale)
	return stale
}

// heldKeys returns the keys of held, sorted, for a log line.
func heldKeys(held map[string]bool) []string {
	keys := make([]string, 0, len(held))
	for key := range held {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
