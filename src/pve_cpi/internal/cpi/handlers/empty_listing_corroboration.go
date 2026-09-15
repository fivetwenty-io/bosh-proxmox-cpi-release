// Second opinions on an empty storage content listing. pve.ProveVolumeAbsent
// reads a listing that omits a volume as proof the volume is gone, and on an
// nfs or cifs export that mounts but serves the wrong tree the listing is empty
// for every volume the storage really holds. The corroborators here are the
// answer to "does anything else know of volumes that listing should have
// carried?", and each one that says yes turns a proven absence back into an
// unproven one.
package handlers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// emptyListingCorroborators composes the second opinions a handler hands to
// pve.ProveVolumeAbsent, cheapest first.
//
//  1. The cluster's VM configs, counted by a holder scan the caller already
//     paid for. refs is nil on every caller that never scanned, and this source
//     is left out entirely there rather than added with nothing to say.
//  2. The CPI's allocation journal, a record on local disk that costs no API
//     call and answers on callers that never scanned.
//  3. PVE's own status for the storage, the one source that spends an API call.
//     It contradicts a listing the storage was not even active for, and it
//     warns about a used figure that disagrees with an empty listing without
//     drawing a contradiction from it.
//
// The proof consults them in order and stops at the first one that has
// something to say, so the status read happens only when the record-based
// sources are silent.
func emptyListingCorroborators(deps Deps, refs pve.StorageReferenceCounts) []pve.EmptyListingCorroborator {
	return composeEmptyListingCorroborators(deps.Config, deps.PVE, refs)
}

// BackendCorroborators is the supplier the production backend resolver hands to
// pve.WithEmptyListingCorroborators. The local backend's cluster sweep proves a
// volume absent from every node it probes, so it needs the same corroboration
// the handlers apply, but it runs inside package pve, which cannot see the
// allocation journal. It carries no config-reference counts: the sweep is
// reached from callers that never scanned as well as from ones that did, and
// the caller that holds counts passes them per call through
// pve.NodeForExistingCorroborated.
func BackendCorroborators(cfg *config.CPIConfig, client pve.Client) func() []pve.EmptyListingCorroborator {
	return func() []pve.EmptyListingCorroborator {
		return composeEmptyListingCorroborators(cfg, client, nil)
	}
}

// composeEmptyListingCorroborators is the shared body of the two constructors
// above, taking the two Deps fields the sources actually read so the backend
// resolver, which is built before any Deps exists, can use it too.
func composeEmptyListingCorroborators(
	cfg *config.CPIConfig, client pve.Client, refs pve.StorageReferenceCounts,
) []pve.EmptyListingCorroborator {
	out := make([]pve.EmptyListingCorroborator, 0, 3)
	if refs != nil {
		out = append(out, pve.ConfigReferenceCorroborator(refs))
	}
	return append(out, journalCorroborator(cfg), pve.StorageStatusCorroborator(client))
}

// journalCorroborator contradicts an empty content listing when the CPI's own
// allocation journal holds volumes it allocated on the storage and never
// recorded deleting. It reads the records storageAuditRecordIndex reads, under
// the narrower rules journalVolumesOnStorage documents, so what it claims still
// exists is a subset of what the audit does.
//
// It is the corroborator that works where no holder scan ran: has_disk, the
// delete_vm slot loop, the orphan sweeps, and the local backend's node sweep
// all reach it. What it cannot see is a disk born before the journal was
// enrolled, or one whose records a wiped journal directory took with it, and
// nothing after it can see those either, so a storage in that state proves
// absent from an empty listing.
//
// The allocation that owns the volume under proof never counts against it. Its
// own record is what every caller here is trying to settle: delete_disk reaches
// this proof for a disk whose record is still open precisely because the delete
// has not been recorded yet, and both orphan sweeps run while the allocation
// that named the volume is mid-flight. Counting that record would turn every
// genuinely completed delete into a refusal on any deployment that enabled the
// journal, which is the opposite of what this source is for. Volumes other
// allocations put on the same storage are what it speaks to.
//
// Silence and failure are different answers. A deployment that configured no
// journal has nothing to say, and so does one whose journal directory exists
// but was never enrolled: neither is evidence that the storage is empty. A
// journal that is configured and enrolled but cannot be read is a check that
// did not land, and the proof fails closed on it, because a delete path is
// about to conclude that a volume is gone.
func journalCorroborator(cfg *config.CPIConfig) pve.EmptyListingCorroborator {
	return &journalSource{cfg: cfg}
}

// journalSource is one corroborator's worth of journal reading. The records are
// read at most once per instance, because the local backend's sweep hands the
// same instance to every node it probes and the handlers hand it to every slot
// in a loop, and re-reading the directory for each of those would turn one
// second opinion into a read per candidate. One read also keeps the whole sweep
// weighing the same evidence, rather than a set of records a concurrent CPI
// process rewrote partway through.
type journalSource struct {
	cfg      *config.CPIConfig
	once     sync.Once
	records  []aj.Record
	enrolled bool
	err      error
}

// CorroborationSource names this source in a refusal that carried no verdict.
func (j *journalSource) CorroborationSource() string { return pve.CorroborationSourceJournal }

// CorroborateEmptyListing answers for one probe: the volumes this journal says
// it put on the probed storage, minus the allocation that owns the volume under
// proof, and minus what lives on a node the probe cannot see.
func (j *journalSource) CorroborateEmptyListing(
	_ context.Context, probe pve.EmptyListingProbe,
) (pve.Corroboration, error) {
	if j.cfg == nil {
		return pve.Corroboration{}, nil
	}
	directory := strings.TrimSpace(j.cfg.StorageAllocationJournalDir)
	namespace := strings.TrimSpace(j.cfg.StoragePlacementNamespace)
	if directory == "" || namespace == "" {
		// The journal needs both to be opened at all, so a half-set pair is a
		// deployment that never enabled it rather than one whose records we
		// failed to read.
		return pve.Corroboration{}, nil
	}
	records, enrolled, err := j.read(directory, namespace)
	if err != nil {
		return pve.Corroboration{}, err
	}
	if !enrolled {
		return pve.Corroboration{}, nil
	}
	allocated := journalVolumesOnStorage(records, probe)
	if allocated == 0 {
		return pve.Corroboration{}, nil
	}
	return pve.Corroboration{
		Contradicted: true,
		Source:       pve.CorroborationSourceJournal,
		Detail:       journalContradictionDetail(allocated, probe),
	}, nil
}

// read performs the one journal read this source is willing to pay for and
// replays its outcome, failure included, to every later probe.
func (j *journalSource) read(directory, namespace string) ([]aj.Record, bool, error) {
	j.once.Do(func() {
		j.records, j.enrolled, j.err = readAllocationJournalRecords(directory, namespace)
	})
	return j.records, j.enrolled, j.err
}

// readAllocationJournalRecords opens the enrolled journal read-only and lists
// its records. The second return is false when there is no enrollment to read,
// which is the "nothing to say" case; an error is a journal that exists and
// would not answer.
//
// It opens against the enrollment's own recorded cluster ID rather than
// observing the live cluster identity, which is what openStorageAllocationJournal
// does. That check belongs to a path about to allocate, and this one only reads
// what was written: an enrollment pointing at a different cluster would still
// be evidence of volumes somebody allocated on this storage, and spending a
// cluster API call to reject it would make the cheap corroborator the expensive
// one.
func readAllocationJournalRecords(directory, namespace string) ([]aj.Record, bool, error) {
	status, err := aj.InspectEnrollment(directory, namespace)
	if err != nil {
		if journalNotEnrolled(directory, namespace, err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("inspect allocation journal enrollment in namespace %s: %w", namespace, err)
	}
	journal, err := aj.Open(directory, namespace, status.Enrollment.ClusterID)
	if err != nil {
		if journalNotEnrolled(directory, namespace, err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("open allocation journal namespace %s: %w", namespace, err)
	}
	records, listErr := journal.List()
	closeErr := journal.Close()
	if err := errors.Join(listErr, closeErr); err != nil {
		return nil, false, fmt.Errorf("read allocation journal namespace %s: %w", namespace, err)
	}
	return records, true, nil
}

// journalNotEnrolled reports whether err means there is no journal to read yet,
// as opposed to one that failed to answer. A missing directory and a namespace
// directory carrying no authority file are both states a deployment that never
// turned on storage placement sits in permanently.
//
// The permission check the journal makes before it will open a directory lands
// here too, and it is the reason this looks past the error. The journal refuses
// a directory any other user can reach, so a path an operator created with the
// umask default of 0755, or pointed at while planning to enable the feature,
// fails that check rather than reporting nothing is there. Refusing every empty
// listing on such a deployment would wedge delete_disk and has_disk over a
// directory that holds no records at all. So when the error is not already a
// plain absence, the enrollment file is looked for directly: no enrollment
// means nothing to say, whatever stopped the open.
//
// A directory that is enrolled still fails closed. The check that refused it is
// then refusing a journal with records in it, and a delete path may not read
// "we could not open the record" as "the record says nothing". So does a
// presence check that could not itself answer.
func journalNotEnrolled(directory, namespace string, err error) bool {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, aj.ErrNotInitialized) {
		return true
	}
	recorded, checkErr := aj.EnrollmentRecorded(directory, namespace)
	return checkErr == nil && !recorded
}

// journalVolumesOnStorage counts the distinct volumes the journal says were
// allocated on the probed storage and never recorded as deleted.
//
// The rules match storageAuditRecordIndex where the two ask the same question:
// a record in a terminal state (deleted or cleaned) is a completed lifecycle
// and claims nothing, a step whose target is external is preservation work for
// another allocation rather than ownership, and a step's volumes are the volids
// it recorded plus the volume it intended to create, which is the only name a
// step that failed mid-flight carries. Volumes are deduplicated because one
// allocation's attempts record the same volid on more than one step.
//
// Three rules are this source's own, and each exists to stop a contradiction
// that would be wrong.
//
// The allocation that owns the volume under proof is dropped whole, not just
// the one name. A parker reassignment renames the volume, so the birth volid
// the journal recorded and the name the proof carries are two names for one
// disk and never meet by string comparison. The retained-ephemeral cleanup is
// the same shape from the other side: its record names the ephemeral disk under
// proof and a config ISO on the same storage, and counting that ISO would refuse
// every retained cleanup the journal ever recorded.
//
// A step on another node is dropped unless the storage is shared. A node-local
// storage is a different tree on every node, and PVE gives every node a dir
// storage called "local", so a volume the journal recorded on one node's
// "local" says nothing about the listing another node's "local" just served.
// A step that recorded no node at all matches any node, because dropping it
// would silently discard evidence rather than scope it.
//
// A step that never got past Planned is dropped. The journal persists a step's
// intent before it submits the API call that would create the volume, and the
// record format enforces that: a new step must first appear as Planned with no
// UPID and no volids, carrying only the volume it means to create. Counting
// that intended name would contradict an empty listing with a volume nothing
// ever created. Submitted and the states past it are counted, because from
// Submitted onward the create call has gone to PVE and a volume may exist
// whatever the outcome was.
func journalVolumesOnStorage(records []aj.Record, probe pve.EmptyListingProbe) int {
	if strings.TrimSpace(probe.Storage) == "" {
		return 0
	}
	seen := make(map[string]struct{})
	for recordIndex := range records {
		record := records[recordIndex]
		if record.State == aj.Deleted || record.State == aj.Cleaned {
			continue
		}
		if journalRecordOwnsVolume(record, probe) {
			continue
		}
		for stepIndex := range record.Steps {
			step := record.Steps[stepIndex]
			if !journalStepCountsForProbe(step, probe) {
				continue
			}
			for _, volume := range journalStepVolumes(step) {
				if volume == "" {
					continue
				}
				seen[volume] = struct{}{}
			}
		}
	}
	return len(seen)
}

// journalRecordOwnsVolume reports whether any step in the record names the
// volume under proof, by either a recorded volid or an intended one. A record
// that does is the allocation whose outcome the caller is trying to settle, and
// nothing it holds may be evidence against that.
//
// Every step is examined, whatever state it reached and whatever node it
// targeted, because this is a question of identity rather than of existence: a
// planned step that named the volume still marks the record as the volume's
// own.
func journalRecordOwnsVolume(record aj.Record, probe pve.EmptyListingProbe) bool {
	for stepIndex := range record.Steps {
		for _, volume := range journalStepVolumes(record.Steps[stepIndex]) {
			if volume != "" && sameStorageVolume(probe.Storage, volume, probe.Volume) {
				return true
			}
		}
	}
	return false
}

// journalStepVolumes is every volume name one step carries: the volids it
// recorded, plus the volume it intended to create.
func journalStepVolumes(step aj.Step) []string {
	return append(slices.Clone(step.VolIDs), step.Target.IntendedVolume)
}

// journalStepCountsForProbe reports whether a step's volumes are evidence about
// the listing the probe read: the right storage, ownership rather than
// preservation, a node the probe can see, and a state in which the create call
// had already been made.
func journalStepCountsForProbe(step aj.Step, probe pve.EmptyListingProbe) bool {
	if step.Target.External || step.Target.Storage != probe.Storage {
		return false
	}
	if !journalStepNodeInScope(step, probe) {
		return false
	}
	return step.State != aj.Planned
}

// journalStepNodeInScope reports whether a step's node is one the probe's
// listing would have covered. A shared storage shows the same tree everywhere,
// so every node is in scope. A node-local or unclassified storage is in scope
// only for the node that was probed, and a step that recorded no node is in
// scope for all of them.
func journalStepNodeInScope(step aj.Step, probe pve.EmptyListingProbe) bool {
	if probe.Classified && probe.Info.IsShared() {
		return true
	}
	return step.Target.Node == "" || step.Target.Node == probe.Node
}

// sameStorageVolume reports whether two names on the same storage refer to one
// volume. The names arrive in three shapes and the comparison has to see
// through all of them: the proof is handed a bare volume name on some paths and
// a storage-qualified volid on others, while the journal records the qualified
// form, and file storage qualifies a volume further with the VMID directory it
// sits in. So the storage prefix comes off both sides first, and when what
// remains still differs the file names are compared, which is where
// "9000/vm-9000-disk-0.qcow2" and "vm-9000-disk-0.qcow2" meet.
//
// Two different volumes on one storage would have to share a file name to be
// confused here, and PVE names a volume after the VMID that owns it, so the
// only way to produce that pair is to place the same VMID's disk under two
// different directories on one storage. Erring this way costs a volume from a
// count that only ever contradicts, which is the side to err on.
func sameStorageVolume(storage, a, b string) bool {
	left, leftOnStorage := volumeNameOnStorage(storage, a)
	right, rightOnStorage := volumeNameOnStorage(storage, b)
	if !leftOnStorage || !rightOnStorage {
		return false
	}
	if left == right {
		return true
	}
	return volumeFileName(left) == volumeFileName(right)
}

// volumeNameOnStorage strips the storage qualifier from a volid, leaving the
// name the storage knows the volume by. The bool is false for a name that
// belongs to another storage and for an empty one, both of which must match
// nothing here rather than fall through to the file-name comparison.
func volumeNameOnStorage(storage, volume string) (string, bool) {
	trimmed := strings.TrimSpace(volume)
	if trimmed == "" {
		return "", false
	}
	if storage == "" {
		return trimmed, true
	}
	// PVE qualifies a volid as "<storage>:<name>" and a volume name carries no
	// colon of its own, so the first colon is the storage boundary when there
	// is one at all.
	prefix, name, qualified := strings.Cut(trimmed, ":")
	if !qualified {
		return trimmed, true
	}
	if prefix != storage || name == "" {
		return "", false
	}
	return name, true
}

// volumeFileName is the last path segment of a volume name: the file itself on
// file storage, and the whole name on block storage, which has no directories.
func volumeFileName(volume string) string {
	if index := strings.LastIndex(volume, "/"); index >= 0 {
		return volume[index+1:]
	}
	return volume
}

// journalContradictionDetail renders the count as the clause the refusal reads,
// agreeing the verb with the number so the whole message stays a sentence and
// naming the node when the count was scoped to one.
func journalContradictionDetail(n int, probe pve.EmptyListingProbe) string {
	scope := ""
	if !probe.Classified || !probe.Info.IsShared() {
		scope = " on node " + probe.Node
	}
	if n == 1 {
		return "1 volume allocated on the storage" + scope + " has no recorded delete"
	}
	return fmt.Sprintf("%d volumes allocated on the storage%s have no recorded delete", n, scope)
}
