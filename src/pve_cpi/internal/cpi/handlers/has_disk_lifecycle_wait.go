package handlers

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// managedDiskRefusal is the identity check's refusal of a managed disk whose
// allocation record it read. It carries that record, so has_disk can wait for
// the record's lifecycle and tell an abandoned step from a live one. The
// error it wraps is the refusal the caller sees, word for word.
type managedDiskRefusal struct {
	err    error
	record aj.Record
	// node is the node whose certificates opened the journal the record was
	// read from, so the wait opens it the same way.
	node string
	// carried reports whether a holder or a transfer record still carries
	// the disk's identity.
	carried bool
	// conflict reports whether what PVE shows contradicts the record, rather
	// than only showing a step the record hasn't settled.
	conflict bool
}

func (r *managedDiskRefusal) Error() string { return r.err.Error() }
func (r *managedDiskRefusal) Unwrap() error { return r.err }

// managedIdentityConflict marks an identity check refusal that says what PVE
// shows contradicts the disk's allocation record.
type managedIdentityConflict struct{ err error }

func (c *managedIdentityConflict) Error() string { return c.err.Error() }
func (c *managedIdentityConflict) Unwrap() error { return c.err }

// identityConflict wraps err as a contradiction between PVE and the record.
func identityConflict(err error) error {
	if err == nil {
		return nil
	}
	return &managedIdentityConflict{err: err}
}

// asManagedDiskRefusal wraps err with the record the identity check read for
// rd through the journal it opened on node, unless err already carries a
// record from a later read.
func asManagedDiskRefusal(err error, record aj.Record, rd resolvedDisk, node string) error {
	if err == nil {
		return nil
	}
	var typed *managedDiskRefusal
	if errors.As(err, &typed) {
		return err
	}
	var conflict *managedIdentityConflict
	return &managedDiskRefusal{
		err:      err,
		record:   record,
		node:     node,
		carried:  rd.holder != nil || rd.intent != nil,
		conflict: errors.As(err, &conflict),
	}
}

// hasDiskWaitDefault caps has_disk's wait for a disk's lifecycle when the
// request carries no deadline.
const hasDiskWaitDefault = 120 * time.Second

// hasDiskSecondLookFloor is the time has_disk keeps for its second look at a
// disk once a wait for a lifecycle ends. A full identity scan of the cluster,
// and a claim-only resume that may wait for a parker's lock, fit in it. When
// less than this would remain after a wait, has_disk doesn't start the second
// look, and it returns its retriable wait error instead. A second look that
// needs no wait runs on whatever time is left.
const hasDiskSecondLookFloor = 10 * time.Second

// hasDiskLookMargin is how long before the request's end has_disk's second
// look must stop. A look that runs into the request's deadline gives the
// dispatcher's generic timeout instead of has_disk's own error, so the look
// stops this much earlier and has_disk's error, which says what it waited
// for, reaches the Director first.
const hasDiskLookMargin = time.Second

// hasDiskWaitKey carries a hasDiskWaitSeam on a context.
type hasDiskWaitKey struct{}

// hasDiskWaitSeam lets a test shorten the wait and run code at its two
// points. waiting runs after the first answer and before has_disk opens the
// journal to wait, with the first answer's refusal, or nil when the first
// answer was a false that the wait checks. held runs once has_disk holds the
// disk's lock, whether or not it had to wait for it, and before it asks
// again.
type hasDiskWaitSeam struct {
	limit   time.Duration
	waiting func(refusal error)
	held    func()
}

// withHasDiskWaitForTest returns ctx carrying seam.
func withHasDiskWaitForTest(ctx context.Context, seam hasDiskWaitSeam) context.Context {
	return context.WithValue(ctx, hasDiskWaitKey{}, seam)
}

// hasDiskWaitLimit returns how long has_disk waits for a disk's lifecycle
// that it starts waiting for at start, and when its second look must end.
// The second look ends at the request's deadline, or, when the request has
// none, hasDiskWaitDefault plus hasDiskSecondLookFloor after start. The wait
// is the time left before that end less hasDiskSecondLookFloor, so it is
// zero or less when the second look has too little time left to start.
func hasDiskWaitLimit(ctx context.Context, start time.Time) (time.Duration, time.Time) {
	end, ok := ctx.Deadline()
	if !ok {
		end = start.Add(hasDiskWaitDefault + hasDiskSecondLookFloor)
	}
	wait := end.Sub(start) - hasDiskSecondLookFloor
	if seam, ok := ctx.Value(hasDiskWaitKey{}).(hasDiskWaitSeam); ok && seam.limit > 0 && wait > 0 {
		wait = min(wait, seam.limit)
	}
	return wait, end
}

// hasDiskRefusedAllocation returns the allocation whose lifecycle may explain
// a refusal of the disk, and the node the identity check opened its journal
// on, from the record the refusal carries or, failing that, from the disk's
// birth name with no node.
func hasDiskRefusedAllocation(err error, birth string) (string, string, bool) {
	var refusal *managedDiskRefusal
	if errors.As(err, &refusal) && refusal.record.ID != "" {
		return refusal.record.ID, refusal.node, true
	}
	_, id, managed := pve.ParseAllocationVolumeID(birth)
	return id, "", managed
}

// hasDiskFirstLook is has_disk's first answer for a managed disk that the
// wait checks again: a refusal, or a disk found absent while a record that
// isn't terminal names it. id is the disk's allocation, and node is the node
// whose certificates opened the journal for the first answer. probe, when
// set, answers for a disk the second look resolves without a holder, a
// transfer record, or an allocation, under the same hold as the second look,
// and it reads the disk's record from the journal the hold was taken on.
type hasDiskFirstLook struct {
	rd    resolvedDisk
	err   error
	id    string
	node  string
	probe func(ctx context.Context, journal *aj.Journal, rd resolvedDisk) (bool, error)
}

// waitOutHasDiskRefusal hands has_disk's first resolution to
// resolveHasDiskAfterLifecycle when a lifecycle may have been partway through
// the disk while it ran. That is a refusal that names a managed allocation,
// or a managed disk found absent while its record isn't terminal, because a
// disk on its way between two guests can leave no carrier in any read of the
// scan. A held birth name and a copied identity keep their own answers in
// has_disk, so they come back unchanged, and so does every other first
// resolution.
func waitOutHasDiskRefusal(ctx context.Context, deps Deps, diskCID, bareDiskCID string, meta *pve.DiskCIDMeta, rd resolvedDisk, refused error) (resolvedDisk, bool, error) {
	if refused == nil {
		allocation := rd.allocation
		if allocation == nil || !allocation.absent || allocation.terminalAbsent || allocation.record.ID == "" {
			return rd, false, nil
		}
		first := hasDiskFirstLook{rd: rd, id: allocation.record.ID, node: allocation.journalNode}
		return resolveHasDiskAfterLifecycle(ctx, deps, diskCID, bareDiskCID, meta, first)
	}
	if _, held := pve.IsDiskBirthNameHeld(refused); held {
		return rd, false, refused
	}
	if _, copied := pve.IsDiskIdentityCopied(refused); copied {
		return rd, false, refused
	}
	id, node, ok := hasDiskRefusedAllocation(refused, bareDiskCID)
	if !ok {
		return resolvedDisk{}, false, refused
	}
	return resolveHasDiskAfterLifecycle(ctx, deps, diskCID, bareDiskCID, meta, hasDiskFirstLook{err: refused, id: id, node: node})
}

// waitOutUnresolvedHasDisk checks a false answer for a stable-ID disk that
// resolved without a holder, a transfer record, or an allocation. Such a disk
// may still be managed, through an allocation that only its holder's
// description or its parker's transfer record names, and a lifecycle that
// moves it between two guests can leave neither for the scan to read. So we
// look for the allocation record whose disk token is the disk's stable ID.
// When one that isn't terminal names it, we wait for any lifecycle that holds
// that record and look again, the same way as for a managed disk found
// absent. When no record names it, or the record is terminal, the false
// answer stands.
//
// When the second look still finds no carrier, it probes the name the record
// last gave the disk rather than the name in the CID, because a lifecycle
// that stopped after a move renamed the disk and before it wrote the disk's
// serial leaves the disk under that name with nothing else to find it by.
func waitOutUnresolvedHasDisk(ctx context.Context, deps Deps, diskCID, bareDiskCID string, meta *pve.DiskCIDMeta, rd resolvedDisk) (bool, error) {
	record, found, err := hasDiskTokenRecord(deps, diskCID, rd.stableID)
	if err != nil || !found {
		return false, err
	}
	first := hasDiskFirstLook{rd: rd, id: record.ID, probe: func(ctx context.Context, journal *aj.Journal, rd resolvedDisk) (bool, error) {
		return probeHasDiskRecordVolume(ctx, deps, diskCID, journal, record.ID, rd)
	}}
	rd, present, err := resolveHasDiskAfterLifecycle(ctx, deps, diskCID, bareDiskCID, meta, first)
	if present {
		return true, nil
	}
	if present, answered, err := answerHasDiskResolution(ctx, deps, diskCID, rd, err); answered {
		return present, err
	}
	// The second look resolved the disk without a carrier again, and its
	// probe, which ran under the record's hold, answered false.
	return false, nil
}

// probeHasDiskRecordVolume answers for a disk that has_disk's second look
// resolved without a carrier, while it holds the disk's record id in shared
// mode. It reads the record again under that hold, because a lifecycle that
// ended during the wait may have renamed the disk, and it probes the name
// the record last gave the disk. A record that has become terminal answers
// false. A record that names no volume leaves only the resolved name to
// probe.
//
// When storage doesn't list that name and an unsettled move of the record
// set out from it, the move may have landed the disk under a name the record
// doesn't hold yet, so has_disk can't report the disk missing. No lifecycle
// holds the record while we do, so nothing will settle that move, and
// has_disk refuses for audit, the same way the identity check refuses a
// disk whose move isn't settled.
func probeHasDiskRecordVolume(ctx context.Context, deps Deps, diskCID string, journal *aj.Journal, id string, rd resolvedDisk) (bool, error) {
	record, err := journal.Inspect(id)
	if err != nil {
		return false, cpierrors.Retriable("has_disk: couldn't read allocation record %s, which names disk %s (%v); rerun the cloud check once the allocation journal can be read", id, diskCID, err)
	}
	if record.State == aj.Deleted || record.State == aj.Cleaned {
		return false, nil
	}
	volume, pending := hasDiskRecordVolume(record)
	if volume == "" {
		volume = rd.volid
	}
	present, err := probeHasDiskVolume(ctx, deps, diskCID, volume)
	if err != nil || present {
		return present, err
	}
	if pending != nil {
		return false, hasDiskUnsettledMoveRefusal(pending)
	}
	return false, nil
}

// hasDiskUnsettledMoveRefusal is has_disk's refusal for a disk whose record
// last named a volume that storage doesn't list, while move step pending,
// which set out from that name, isn't settled. It is the identity check's
// refusal for the same record, so the runbook entry it names covers both.
func hasDiskUnsettledMoveRefusal(pending *aj.Step) error {
	return cpierrors.Cloud("%s, because move step %s of the disk's record isn't settled; %s", missingVolumeAudit, pending.ID, unsettledMoveRunbook)
}

// probeHasDiskAllocationVolume answers for a disk whose CID names its
// allocation, when has_disk's second look, under the record's hold, found the
// CID's name gone and no holder or transfer record carrying the disk. A
// lifecycle that stopped after a move renamed the disk and before it wrote
// the disk's serial and notes leaves the disk under the name the record last
// gave it, with nothing else that leads to it. So we probe that name with the
// same node-scoped exact read, cluster check, and format check that the
// identity check gave the CID's name. present is true when storage holds the
// disk under that name. When neither name is there and an unsettled move set
// out from the record's name, we refuse for audit, because the record doesn't
// say where that move landed. Otherwise present is false and err is nil, and
// the second look's own answer stands. The CID's name isn't probed again,
// because the exact read already showed it gone, and a looser probe could
// find a volume of the same name on another node.
func probeHasDiskAllocationVolume(ctx context.Context, deps Deps, rd resolvedDisk) (bool, error) {
	allocation := rd.allocation
	if allocation == nil || !allocation.absent || allocation.terminalAbsent || rd.holder != nil || rd.intent != nil {
		return false, nil
	}
	volume, pending := hasDiskRecordVolume(allocation.record)
	if volume != "" && volume != rd.volid {
		_, exists, err := observeManagedFreeDisk(ctx, deps, allocation.record, volume, allocation.provenance.Backing, rd.meta, allocation.provenance.Node)
		if err != nil || exists {
			return exists, err
		}
	}
	if pending != nil {
		return false, hasDiskUnsettledMoveRefusal(pending)
	}
	return false, nil
}

// hasDiskRecordVolume returns the name record last gave its disk, and the
// unsettled move that set out from that name, if any. A step of a closed
// attempt that its completion proof settles left nothing behind, so the walk
// skips it. That covers a create that PVE rejected before create_disk fell
// back to a second attempt, whose planned step names a volume the disk never
// had. The disk starts under the first volume another step of the record
// names for this allocation, which is the one its create step made. Each
// observed move, and each observed migrate, that set out from the current
// name and landed under another gives the disk that name, and it shows the
// disk was still under the old name when it set out, so it clears an earlier
// unsettled move. A move or migrate that holds only the name it set out from
// landed nowhere else. A move that isn't settled keeps the current name,
// because the record holds no landing for it, and comes back as pending.
func hasDiskRecordVolume(record aj.Record) (string, *aj.Step) {
	current := ""
	var pending *aj.Step
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.Target.External || storageDecisionClosedAttemptStepSettled(record, *step) {
			continue
		}
		from := step.Target.IntendedVolume
		if from == "" && len(step.VolIDs) > 0 {
			from = step.VolIDs[0]
		}
		if current == "" {
			current = from
		}
		if current == "" || from != current {
			continue
		}
		move := strings.HasSuffix(step.Kind, "_Nodes_CreateQemuMoveDisk")
		if !move && !strings.HasSuffix(step.Kind, "_Nodes_CreateQemuMigrate") {
			continue
		}
		if step.State != aj.Observed {
			if move {
				pending = step
			}
			continue
		}
		if len(step.VolIDs) < 2 {
			continue
		}
		if landed := step.VolIDs[len(step.VolIDs)-1]; landed != "" && landed != current {
			current, pending = landed, nil
		}
	}
	return current, pending
}

// hasDiskTokenRecord returns the allocation record that names the disk token
// and isn't terminal. It reads only the record whose ID gives the token, so a
// record of another allocation that can't be read doesn't matter. A
// deployment without an enrolled journal, and a token no such record names,
// find nothing. A journal that is enrolled but can't be listed, and a record
// for the token that can't be read, give a retriable error, because nothing
// can show that no lifecycle holds a record for the disk.
func hasDiskTokenRecord(deps Deps, diskCID, token string) (aj.Record, bool, error) {
	if deps.Config == nil || token == "" {
		return aj.Record{}, false, nil
	}
	directory := strings.TrimSpace(deps.Config.StorageAllocationJournalDir)
	namespace := strings.TrimSpace(deps.Config.StoragePlacementNamespace)
	if directory == "" || namespace == "" {
		return aj.Record{}, false, nil
	}
	record, found, err := inspectAllocationJournalDiskToken(directory, namespace, token)
	if err != nil {
		return aj.Record{}, false, cpierrors.Retriable("has_disk: couldn't read the allocation journal to check whether another operation holds disk %s (%v); rerun the cloud check once the journal can be read", diskCID, err)
	}
	if !found || record.Kind != allocationKindDisk || record.State == aj.Deleted || record.State == aj.Cleaned {
		return aj.Record{}, false, nil
	}
	return record, true, nil
}

// inspectAllocationJournalDiskToken opens the enrolled journal read-only, the
// way readAllocationJournalRecords does, and returns the record whose disk
// token is token. found is false when there is no enrollment to read or no
// record names the token, and an error is a journal that exists and would
// not answer.
func inspectAllocationJournalDiskToken(directory, namespace, token string) (aj.Record, bool, error) {
	status, err := aj.InspectEnrollment(directory, namespace)
	if err != nil {
		if journalNotEnrolled(directory, namespace, err) {
			return aj.Record{}, false, nil
		}
		return aj.Record{}, false, fmt.Errorf("inspect allocation journal enrollment in namespace %s: %w", namespace, err)
	}
	journal, err := aj.Open(directory, namespace, status.Enrollment.ClusterID)
	if err != nil {
		if journalNotEnrolled(directory, namespace, err) {
			return aj.Record{}, false, nil
		}
		return aj.Record{}, false, fmt.Errorf("open allocation journal namespace %s: %w", namespace, err)
	}
	record, found, inspectErr := journal.InspectDiskToken(token)
	if err := errors.Join(inspectErr, journal.Close()); err != nil {
		return aj.Record{}, false, fmt.Errorf("read allocation journal namespace %s: %w", namespace, err)
	}
	return record, found, nil
}

// resolveHasDiskAfterLifecycle resolves again a managed disk whose first
// answer may come from a lifecycle partway through it. A lifecycle that holds
// the disk's journal lock may be halfway through a move, a landing, or a
// delete, and what it has written so far can contradict the record, or hide
// the disk from every read, so the first answer isn't final. We wait in
// shared mode for that lifecycle to finish, and resolve the disk again while
// we hold the lock, so no other lifecycle starts between the wait and the
// answer. We always look again, even when the record reads as it did,
// because a lifecycle can finish between the identity scan and the record
// read, and then the first answer rests on what PVE showed before it
// finished.
//
// When no lifecycle holds the lock, we take it at once and look again on
// whatever time the request has left. When one does, we wait only while
// hasDiskSecondLookFloor still remains for the second look. A wait that runs
// out, a request that ends while we wait or look again, and a second look
// that has too little of the request's time left to start each return a
// retriable error. That error names the operation the record says is running
// only when we waited for one. A journal or lock that can't be read returns a
// retriable error for a first answer of false, because we can't show that no
// lifecycle holds the record, and it keeps a first refusal unless the
// request has ended.
//
// A second resolution that still refuses comes back as it is, except for a
// step no live call holds any longer. When the record isn't terminal, has an
// unsettled step, has no unsettled delete, and a holder or a transfer record
// still carries the disk's identity, the disk exists, and present comes back
// true. The other disk calls keep refusing it until an operator settles the
// step.
func resolveHasDiskAfterLifecycle(ctx context.Context, deps Deps, diskCID, bareDiskCID string, meta *pve.DiskCIDMeta, first hasDiskFirstLook) (resolvedDisk, bool, error) {
	if deps.Config == nil {
		return first.rd, false, first.err
	}
	look := hasDiskSecondLook{deps: deps, diskCID: diskCID, bareDiskCID: bareDiskCID, meta: meta, first: first, start: time.Now()}
	// The journal is opened through the same node's certificates the first
	// answer used, which is the holder's or the parker's node when the disk
	// has one, so a configured node that can't be reached doesn't end the
	// wait.
	look.node = first.node
	if look.node == "" {
		look.node = deps.Config.Node
	}
	seam, _ := ctx.Value(hasDiskWaitKey{}).(hasDiskWaitSeam)
	if seam.waiting != nil {
		seam.waiting(first.err)
	}
	journal, err := openStorageAllocationJournal(ctx, deps, []string{look.node})
	if err != nil {
		return first.rd, false, look.unchecked(ctx, "the allocation journal couldn't be opened", err)
	}
	defer func() { _ = journal.Close() }()
	look.journal = journal
	release, end, operation, err := look.hold(ctx, seam)
	if err != nil {
		var fault *hasDiskLockFault
		if errors.As(err, &fault) {
			return first.rd, false, look.unchecked(ctx, "the disk's journal lock couldn't be read", fault.err)
		}
		return resolvedDisk{}, false, err
	}
	defer func() { _ = release() }()
	return look.again(ctx, end, operation)
}

// hasDiskSecondLook is has_disk's wait for a disk's lifecycle and its second
// look at the disk.
type hasDiskSecondLook struct {
	deps        Deps
	diskCID     string
	bareDiskCID string
	meta        *pve.DiskCIDMeta
	first       hasDiskFirstLook
	node        string
	journal     *aj.Journal
	start       time.Time
}

// hasDiskLockFault is a failure to read the disk's journal lock, as opposed
// to a wait that ended.
type hasDiskLockFault struct{ err error }

func (f *hasDiskLockFault) Error() string { return f.err.Error() }
func (f *hasDiskLockFault) Unwrap() error { return f.err }

// hold takes the disk's journal lock in shared mode. It takes the lock at
// once when no lifecycle holds it, with no floor on the time left. Otherwise
// it waits for the lifecycle while the second look still has
// hasDiskSecondLookFloor left after the wait. It returns the release, when
// the second look must end, and the operation it waited for, which is empty
// when it took the lock at once. A lock it can't read comes back as a
// hasDiskLockFault.
//
// The shared hold keeps every lifecycle off the record until the second look
// ends. Operator tooling that takes every record's lock, which is
// RecoverAuthority and the journal's authority rotation, refuses with
// "allocation writer still active" while has_disk holds one, so a rerun of
// that tooling after has_disk answers goes through.
func (l hasDiskSecondLook) hold(ctx context.Context, seam hasDiskWaitSeam) (func() error, time.Time, string, error) {
	id := l.first.id
	limit, end := hasDiskWaitLimit(ctx, l.start)
	release, held, err := l.journal.TryHoldShared(id)
	if err != nil {
		return nil, end, "", &hasDiskLockFault{err: err}
	}
	if held {
		if seam.held != nil {
			seam.held()
		}
		return release, end, "", nil
	}
	// The record names the operation that holds it now, and the record says
	// it ended once the lock is free, so we read the name before we wait.
	operation := hasDiskHeldOperation(l.journal, id)
	if limit <= 0 {
		return nil, end, operation, hasDiskWaitExpired(operation, l.diskCID, time.Since(l.start), hasDiskNoTimeToLook)
	}
	waitCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	release, err = l.journal.HoldShared(waitCtx, id)
	switch {
	case err != nil && ctx.Err() != nil:
		return nil, end, operation, hasDiskWaitExpired(operation, l.diskCID, time.Since(l.start), hasDiskRequestEnded)
	case errors.Is(err, context.DeadlineExceeded):
		return nil, end, operation, hasDiskWaitExpired(operation, l.diskCID, time.Since(l.start), hasDiskStillHeld)
	case err != nil:
		return nil, end, operation, &hasDiskLockFault{err: err}
	}
	if seam.held != nil {
		seam.held()
	}
	if time.Until(end) < hasDiskSecondLookFloor {
		return nil, end, operation, errors.Join(hasDiskWaitExpired(operation, l.diskCID, time.Since(l.start), hasDiskNoTimeToLook), release())
	}
	return release, end, operation, nil
}

// again resolves the disk a second time, under the hold, on a context that
// ends hasDiskLookMargin before end. operation is the operation has_disk
// waited for before it, if any, so an error names an operation only when one
// ran. A disk whose CID names its allocation and that the second look finds
// absent with no carrier gets probeHasDiskAllocationVolume, and a disk that
// resolves without an allocation or a carrier gets the first look's probe.
func (l hasDiskSecondLook) again(ctx context.Context, end time.Time, operation string) (resolvedDisk, bool, error) {
	lookCtx, cancel := context.WithDeadline(ctx, end.Add(-hasDiskLookMargin))
	defer cancel()
	rd, err := resolveDiskForOp(lookCtx, l.deps, "has_disk", l.diskCID, l.bareDiskCID, l.meta)
	if err == nil {
		var present bool
		if present, err = probeHasDiskAllocationVolume(lookCtx, l.deps, rd); present {
			return rd, true, nil
		}
	}
	if err == nil && l.first.probe != nil && hasDiskUnresolved(rd) {
		var present bool
		if present, err = l.first.probe(lookCtx, l.journal, rd); err == nil {
			return rd, present, nil
		}
	}
	if err != nil && lookCtx.Err() != nil {
		return resolvedDisk{}, false, l.ended(operation)
	}
	if step, ok := abandonedStepCarriesDisk(err); ok {
		l.deps.Log(ctx).Warn("has_disk: reporting the disk present, because no live call holds its record and a holder or transfer record still carries it, but every other disk call refuses it until its step is settled",
			log.String("disk_cid", l.diskCID),
			log.String("allocation_id", l.first.id),
			log.String("step", step),
			log.Err(err),
		)
		return resolvedDisk{}, true, nil
	}
	return rd, false, err
}

// ended is the retriable error for a request that ended during the second
// look. It names the operation has_disk waited for, and names none when
// has_disk took the lock without waiting.
func (l hasDiskSecondLook) ended(operation string) error {
	if operation != "" {
		return hasDiskWaitExpired(operation, l.diskCID, time.Since(l.start), hasDiskRequestEnded)
	}
	return cpierrors.Retriable("has_disk: no other operation held disk %s, but %s; rerun the cloud check", l.diskCID, string(hasDiskRequestEnded))
}

// unchecked answers a wait that couldn't read the disk's journal or its lock,
// for the reason given. A first answer of false becomes a retriable error,
// because we can't show that no lifecycle holds the disk's record, and so
// does any first answer once the request has ended. A first refusal with the
// request still running comes back unchanged.
func (l hasDiskSecondLook) unchecked(ctx context.Context, reason string, cause error) error {
	l.deps.Log(ctx).Warn("has_disk: can't wait for the disk's lifecycle, because "+reason,
		log.String("disk_cid", l.diskCID),
		log.String("allocation_id", l.first.id),
		log.String("node", l.node),
		log.Err(cause),
	)
	if ctx.Err() != nil {
		return cpierrors.Retriable("has_disk: the request ended before has_disk could check whether another operation holds disk %s; rerun the cloud check", l.diskCID)
	}
	if l.first.err != nil {
		return l.first.err
	}
	return cpierrors.Retriable("has_disk: couldn't check whether another operation holds disk %s, because %s (%v); rerun the cloud check once the allocation journal can be read", l.diskCID, reason, cause)
}

// hasDiskUnresolved reports whether rd names no holder, transfer record,
// unused entry, or allocation, so only a storage probe can answer for it.
func hasDiskUnresolved(rd resolvedDisk) bool {
	return rd.allocation == nil && rd.holder == nil && rd.intent == nil && len(rd.unused) == 0
}

// abandonedStepCarriesDisk reports whether err refuses a disk whose record
// stopped partway with no live call holding it, while a holder or transfer
// record still carries the disk. It returns a description of what stopped.
// A contradiction between PVE and the record, a terminal record, or an
// unsettled step that may delete the disk never counts, because each of them
// can mean the disk is gone.
func abandonedStepCarriesDisk(err error) (string, bool) {
	var refusal *managedDiskRefusal
	if !errors.As(err, &refusal) || refusal.conflict || !refusal.carried {
		return "", false
	}
	record := refusal.record
	if record.State == aj.Deleted || record.State == aj.Cleaned {
		return "", false
	}
	unsettled := ""
	// A record that needs reconciliation with every step settled stopped
	// for a reason no step names, so we keep refusing it.
	for i := range record.Steps {
		step := record.Steps[i]
		if step.State == aj.Observed || storageDecisionClosedAttemptStepSettled(record, step) {
			continue
		}
		if deleteStepKind(step.Kind) || configWriteMayDestroyVolume(step) {
			return "", false
		}
		if unsettled == "" {
			unsettled = unsettledStepName(step)
		}
	}
	return unsettled, unsettled != ""
}

// deleteStepKind reports whether a journal step of kind deletes the disk's
// volume or the VM that holds it.
func deleteStepKind(kind string) bool {
	return strings.Contains(kind, "_Storage_DeleteVolume") ||
		strings.HasSuffix(kind, "_Nodes_DeleteQemu") ||
		strings.HasPrefix(kind, "lifecycle_delete_disk_")
}

// configWriteMayDestroyVolume reports whether step is a lifecycle's
// configuration write that may have deleted an unused entry for a volume its
// VM owns. A detach deletes the unused entries that name the disk's volume
// through these writes, and PVE destroys a volume whose unused entry it
// deletes when the VM owns that volume. The step records no parameters for
// such a write, so any configuration write on the VM that owns the volume
// counts, except a protection-only write, whose recorded parameters prove it
// deleted nothing.
func configWriteMayDestroyVolume(step aj.Step) bool {
	if !strings.HasPrefix(step.Kind, "lifecycle_") || !strings.HasSuffix(step.Kind, "_Nodes_UpdateQemuConfig") {
		return false
	}
	if isParkerProtectionParameters(step.Parameters) {
		return false
	}
	owner, ok := pve.EmbeddedDiskVMID(step.Target.IntendedVolume)
	return ok && step.Target.VMID > 0 && owner == step.Target.VMID
}

// unsettledMoveLandsOnHolder reports whether a volume that no step of record
// names may still be the disk, because a move of the disk to holderVMID
// hasn't settled. That is a record that names the disk's birth, with an
// unsettled move on backing whose task PVE started for a move to holderVMID.
// The move's landing name comes back only when the step is observed, so the
// holder's volume can't match the record until then. Any other volume the
// record doesn't name contradicts it.
func unsettledMoveLandsOnHolder(record aj.Record, birth, backing string, holderVMID int) bool {
	birthRecorded, moving := false, false
	for i := range record.Steps {
		step := record.Steps[i]
		if step.Target.IntendedVolume == birth || containsString(step.VolIDs, birth) {
			birthRecorded = true
		}
		if !strings.HasSuffix(step.Kind, "_Nodes_CreateQemuMoveDisk") || step.Target.Backing != backing {
			continue
		}
		if step.State == aj.Observed || storageDecisionClosedAttemptStepSettled(record, step) {
			continue
		}
		if target, ok := moveTaskTargetVMID(step); ok && target == holderVMID {
			moving = true
		}
	}
	return birthRecorded && moving
}

// moveTaskTargetVMID reads the VM a move step's task moves the disk to from
// the task's UPID, "UPID:<node>:<pid>:<pstart>:<starttime>:qmmove:<id>:<user>:".
// PVE gives a move to another VM the ID "<vmid>-<disk>><target-vmid>-<target-disk>"
// (qemu-server Qemu.pm:5196-5199). A step without a UPID, a task on another
// node, or an ID that names another source VM gives no target.
func moveTaskTargetVMID(step aj.Step) (int, bool) {
	parts := strings.Split(step.UPID, ":")
	if len(parts) != 9 || parts[0] != "UPID" || parts[5] != "qmmove" || step.Target.Node == "" || parts[1] != step.Target.Node {
		return 0, false
	}
	source, target, ok := strings.Cut(parts[6], ">")
	if !ok || source == strconv.Itoa(step.Target.VMID) || !pve.MoveTaskIDNamesVM(source, step.Target.VMID) {
		return 0, false
	}
	vmidText, disk, ok := strings.Cut(target, "-")
	if !ok || disk == "" {
		return 0, false
	}
	vmid, err := strconv.Atoi(vmidText)
	if err != nil || vmid <= 0 || vmid == step.Target.VMID {
		return 0, false
	}
	return vmid, true
}

// hasDiskWaitOutcome says why has_disk stopped waiting without an answer.
type hasDiskWaitOutcome string

const (
	// hasDiskStillHeld is a wait that ran out while the lifecycle still held
	// the disk's record.
	hasDiskStillHeld hasDiskWaitOutcome = "it still holds the disk's allocation record"
	// hasDiskRequestEnded is a request that was cancelled, or whose deadline
	// passed, during the wait or the second look.
	hasDiskRequestEnded hasDiskWaitOutcome = "the request ended before has_disk could look at the disk again"
	// hasDiskNoTimeToLook is a request with too little time left for the
	// second look.
	hasDiskNoTimeToLook hasDiskWaitOutcome = "too little of the request's time is left to look at the disk again"
)

// hasDiskHeldOperation names the operation that holds allocation id's
// record, from the reason a lifecycle saves when it starts, or says "another
// disk operation" when the record names none.
func hasDiskHeldOperation(journal *aj.Journal, id string) string {
	if record, err := journal.Inspect(id); err == nil {
		if op, ok := lifecycleOperation(record.Reason); ok {
			return "lifecycle " + op
		}
	}
	return "another disk operation"
}

// hasDiskWaitExpired is the retriable error for a wait for operation that
// ended without an answer. It says why the wait ended.
func hasDiskWaitExpired(operation, diskCID string, waited time.Duration, outcome hasDiskWaitOutcome) error {
	return cpierrors.Retriable("has_disk: waited %s for %s on disk %s to finish, and %s; rerun the cloud check once that operation completes",
		waited.Round(time.Millisecond), operation, diskCID, string(outcome))
}

// lifecycleOperation reads the operation from the reason a lifecycle saves on
// the record it admits.
func lifecycleOperation(reason string) (string, bool) {
	rest, ok := strings.CutPrefix(reason, "lifecycle ")
	if !ok {
		return "", false
	}
	op, ok := strings.CutSuffix(rest, " admitted; completion pending")
	if !ok || op == "" {
		return "", false
	}
	return op, true
}
