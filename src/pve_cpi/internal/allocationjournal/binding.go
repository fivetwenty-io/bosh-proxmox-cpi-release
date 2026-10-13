package allocationjournal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// entryBindingFile is the sidecar that remembers every cpi_config entry
// binding that has admitted a create in this namespace. The name is chosen
// against every reader of the namespace directory: List, lookupRecords, and
// InspectDiskToken only decode names carrying the "allocation-" prefix and
// ".json" suffix, the fixed names authority.json and index.json are read by
// exact name, lock files end in ".lock", and atomicJSON's scratch files start
// with ".tmp-". The sidecar matches none of those, so no reader in this
// release or in releases that predate the sidecar decodes it as a record,
// counts it, or removes it.
const entryBindingFile = "entry-binding.json"

// entryBindingLockFile serializes the sidecar's read-modify-write between CPI
// processes. It is a lock of its own rather than index.lock, because
// AcquireVM holds index.lock across planning and the binding note must never
// wait on that. Like every other lock name it ends in ".lock" and carries no
// record prefix, so the record scans skip it on this release and earlier ones.
const entryBindingLockFile = "entry-binding.lock"

// entryBindingVersion is the sidecar's envelope schema. It is independent of
// the record schema Version, because the sidecar is advisory and evolves on
// its own. A reader that meets a higher version leaves the file alone.
const entryBindingVersion = 1

// maxEntryBindings bounds the sidecar. A namespace gains a binding only when a
// new combination of prefixes and bands admits a create, so a real namespace
// holds a handful; past the bound the oldest bindings are dropped and the
// caller is told which.
const maxEntryBindings = 32

// entryBindingLockWait bounds how long a create waits for another process's
// binding update. The update is one small read and one atomic write, so the
// wait only runs out when the filesystem is badly stalled, and then the
// binding is left for a later create rather than delaying this one further.
const entryBindingLockWait = time.Second

// ErrUnsupportedEntryBinding means the sidecar was written with a newer schema
// than this release reads. The file is left untouched, so a downgrade never
// erases what a newer release recorded.
var ErrUnsupportedEntryBinding = errors.New("journal: entry binding written by a newer release")

// ErrEntryBindingBusy means another process held the sidecar lock for longer
// than entryBindingLockWait. Nothing was read or written.
var ErrEntryBindingBusy = errors.New("journal: entry binding lock is busy")

// ErrEntryBindingWaitAbandoned means the caller's context ended while the note
// waited for the sidecar lock. Nothing was read under the lock or written. The
// returned error also wraps the context's own error.
var ErrEntryBindingWaitAbandoned = errors.New("journal: entry binding lock wait ended with the caller's context")

// ErrEntryBindingUnlock means the sidecar lock could not be released cleanly
// after the update ran. The update's own outcome is reported beside it, so a
// caller that sees only this error knows the sidecar was handled and only the
// release failed. The kernel drops the flock when the descriptor finally
// closes or the process exits, so the lock can't outlive the CPI call.
var ErrEntryBindingUnlock = errors.New("journal: entry binding lock could not be released")

// entryBindingTuning lets tests stretch the lock wait or fail the release
// without touching the production values. Its zero value is production
// behavior: an entryBindingLockWait budget and fileLock.close.
type entryBindingTuning struct {
	lockWait time.Duration
	release  func(*fileLock) error
}

func (t entryBindingTuning) wait() time.Duration {
	if t.lockWait > 0 {
		return t.lockWait
	}
	return entryBindingLockWait
}

func (t entryBindingTuning) unlock(l *fileLock) error {
	if t.release != nil {
		return t.release(l)
	}
	return l.close()
}

// EntryBinding is the identity of the cpi_config entry that writes into a
// journal namespace: the VM name prefix, the resolved parker prefix, and the
// three VMID bands the entry allocates from. Two entries that share a
// namespace on one cluster share admission state, so a namespace that sees two
// different bindings is a configuration worth an operator's attention.
type EntryBinding struct {
	VMPrefix             string `json:"vm_prefix"`
	ParkerPrefix         string `json:"parker_prefix"`
	VMIDRangeStart       int    `json:"vmid_range_start"`
	VMIDRangeEnd         int    `json:"vmid_range_end"`
	DiskVMIDRangeStart   int    `json:"disk_vmid_range_start"`
	DiskVMIDRangeEnd     int    `json:"disk_vmid_range_end"`
	ParkedVMIDRangeStart int    `json:"parked_vmid_range_start"`
	ParkedVMIDRangeEnd   int    `json:"parked_vmid_range_end"`
}

// String renders the binding for log lines and audit findings. An empty VM
// prefix is shown as "(none)" so the reader can tell it apart from a field the
// renderer dropped.
func (b EntryBinding) String() string {
	vmPrefix := b.VMPrefix
	if vmPrefix == "" {
		vmPrefix = "(none)"
	}
	return fmt.Sprintf("vm_prefix=%s parker_prefix=%s vmids=%d-%d disk_vmids=%d-%d parked_vmids=%d-%d",
		vmPrefix, b.ParkerPrefix,
		b.VMIDRangeStart, b.VMIDRangeEnd,
		b.DiskVMIDRangeStart, b.DiskVMIDRangeEnd,
		b.ParkedVMIDRangeStart, b.ParkedVMIDRangeEnd)
}

// Differences names the binding fields where other differs from b, in a fixed
// order, so a warning can say what changed rather than print two long lines
// for the reader to compare. The VMID bands are reported as one name each.
func (b EntryBinding) Differences(other EntryBinding) []string {
	var fields []string
	if b.VMPrefix != other.VMPrefix {
		fields = append(fields, "vm_prefix")
	}
	if b.ParkerPrefix != other.ParkerPrefix {
		fields = append(fields, "parker_prefix")
	}
	if b.VMIDRangeStart != other.VMIDRangeStart || b.VMIDRangeEnd != other.VMIDRangeEnd {
		fields = append(fields, "vmid_range")
	}
	if b.DiskVMIDRangeStart != other.DiskVMIDRangeStart || b.DiskVMIDRangeEnd != other.DiskVMIDRangeEnd {
		fields = append(fields, "disk_vmid_range")
	}
	if b.ParkedVMIDRangeStart != other.ParkedVMIDRangeStart || b.ParkedVMIDRangeEnd != other.ParkedVMIDRangeEnd {
		fields = append(fields, "parked_disk_vmid_range")
	}
	return fields
}

func validateEntryBinding(b EntryBinding) error {
	if strings.TrimSpace(b.ParkerPrefix) == "" {
		return errors.New("entry binding has no parker prefix")
	}
	if strings.ContainsAny(b.VMPrefix+b.ParkerPrefix, "\x00\n\r") {
		return errors.New("entry binding prefix carries control characters")
	}
	bands := [][2]int{
		{b.VMIDRangeStart, b.VMIDRangeEnd},
		{b.DiskVMIDRangeStart, b.DiskVMIDRangeEnd},
		{b.ParkedVMIDRangeStart, b.ParkedVMIDRangeEnd},
	}
	for _, band := range bands {
		if band[0] < 0 || band[1] < 0 || (band[1] != 0 && band[0] > band[1]) {
			return fmt.Errorf("entry binding band %d-%d is not a range", band[0], band[1])
		}
	}
	return nil
}

// EntryBindingSighting is one binding in the set and the time a create under
// it was first admitted into the namespace.
type EntryBindingSighting struct {
	Binding   EntryBinding `json:"binding"`
	FirstSeen time.Time    `json:"first_seen"`
}

// EntryBindingState is the sidecar payload: every binding that has admitted a
// create into the namespace, oldest first. A binding already in the set is
// never rewritten, so the steady state writes nothing.
type EntryBindingState struct {
	Namespace string                 `json:"namespace"`
	Bindings  []EntryBindingSighting `json:"bindings"`
}

// Index returns the position of b in the set, or -1.
func (s EntryBindingState) Index(b EntryBinding) int {
	for i, seen := range s.Bindings {
		if seen.Binding == b {
			return i
		}
	}
	return -1
}

func (j *Journal) validateEntryBindingState(s EntryBindingState) error {
	if s.Namespace != j.auth.Namespace {
		return errors.New("misplaced entry binding")
	}
	if len(s.Bindings) == 0 || len(s.Bindings) > maxEntryBindings {
		return fmt.Errorf("entry binding set holds %d bindings", len(s.Bindings))
	}
	for i, seen := range s.Bindings {
		if err := validateEntryBinding(seen.Binding); err != nil {
			return err
		}
		if seen.FirstSeen.IsZero() {
			return errors.New("entry binding has no first-seen time")
		}
		if (EntryBindingState{Bindings: s.Bindings[:i]}).Index(seen.Binding) >= 0 {
			return errors.New("entry binding set repeats a binding")
		}
	}
	return nil
}

// EntryBinding reads the namespace's entry-binding sidecar. found is false
// with a nil error when no sidecar has been written. A sidecar from a newer
// schema returns ErrUnsupportedEntryBinding. A sidecar that fails the envelope
// checks, names another namespace, or holds an unusable set returns an error
// wrapping ErrCorrupt. Any other error is an I/O or permission failure. The
// sidecar is advisory, so callers report these errors and carry on rather
// than failing a CPI call on them.
func (j *Journal) EntryBinding() (EntryBindingState, bool, error) {
	if j == nil || j.root == nil {
		return EntryBindingState{}, false, ErrClosed
	}
	env, err := readEnvelope(j.root, entryBindingFile)
	if errors.Is(err, os.ErrNotExist) {
		return EntryBindingState{}, false, nil
	}
	if err != nil {
		return EntryBindingState{}, false, err
	}
	if env.Version > entryBindingVersion {
		return EntryBindingState{}, false, fmt.Errorf("%w: version %d, this release reads %d",
			ErrUnsupportedEntryBinding, env.Version, entryBindingVersion)
	}
	var state EntryBindingState
	if err := decodeEnvelope(env, entryBindingVersion, &state); err != nil {
		return EntryBindingState{}, false, err
	}
	if err := j.validateEntryBindingState(state); err != nil {
		return EntryBindingState{}, false, fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	return state, true, nil
}

// EntryBindingNote reports what NoteEntryBinding did.
type EntryBindingNote struct {
	// Added is true when the binding was new to the namespace and was
	// written, or would have been had the write not failed.
	Added bool
	// Others holds the bindings the namespace had already recorded when an
	// added binding first appeared, oldest first. It is empty for the first
	// binding a namespace ever records.
	Others []EntryBindingSighting
	// Dropped holds the oldest bindings removed to keep the set within
	// maxEntryBindings.
	Dropped []EntryBindingSighting
	// Replaced is the error that made an existing sidecar unreadable as
	// journal evidence, when the note replaced it. It is nil otherwise.
	Replaced error
}

// NoteEntryBinding adds binding to the namespace's set of bindings when it is
// not already there. A binding already in the set costs one read and no lock
// or write. A new binding is added under entry-binding.lock after a second
// read, so two processes adding different bindings at once both land in the
// set and neither overwrites the other.
//
// It never touches a sidecar from a newer schema (ErrUnsupportedEntryBinding)
// or one it can't read for a reason other than corruption, such as an unsafe
// mode or owner. A corrupt sidecar is replaced and the reason is returned in
// Replaced. When the lock stays busy past entryBindingLockWait it returns
// ErrEntryBindingBusy and changes nothing, and when ctx ends during the wait
// it returns ErrEntryBindingWaitAbandoned with the context's error. A failed
// write returns the journal's DurabilityError with Added set. A failed lock
// release returns ErrEntryBindingUnlock, joined with the write's error when
// that failed too, and the note still describes what the update did.
func (j *Journal) NoteEntryBinding(ctx context.Context, binding EntryBinding, now time.Time) (EntryBindingNote, error) {
	if j == nil || j.root == nil {
		return EntryBindingNote{}, ErrClosed
	}
	if err := validateEntryBinding(binding); err != nil {
		return EntryBindingNote{}, err
	}
	if now.IsZero() {
		return EntryBindingNote{}, errors.New("journal: entry binding needs a recording time")
	}
	state, found, err := j.EntryBinding()
	if err != nil && !errors.Is(err, ErrCorrupt) {
		return EntryBindingNote{}, err
	}
	if err == nil && found && state.Index(binding) >= 0 {
		return EntryBindingNote{}, nil
	}

	waitCtx, cancel := context.WithTimeout(ctx, j.binding.wait())
	defer cancel()
	lock, lockErr := j.waitEntryBindingLock(waitCtx)
	if lockErr != nil {
		waitEnded := errors.Is(lockErr, context.Canceled) || errors.Is(lockErr, context.DeadlineExceeded)
		switch parentErr := ctx.Err(); {
		case waitEnded && parentErr != nil:
			return EntryBindingNote{}, fmt.Errorf("%w: %w", ErrEntryBindingWaitAbandoned, parentErr)
		case waitEnded:
			return EntryBindingNote{}, ErrEntryBindingBusy
		default:
			return EntryBindingNote{}, lockErr
		}
	}
	note, addErr := j.addEntryBindingLocked(binding, now)
	if releaseErr := j.binding.unlock(lock); releaseErr != nil {
		return note, errors.Join(addErr, fmt.Errorf("%w: %w", ErrEntryBindingUnlock, releaseErr))
	}
	return note, addErr
}

func (j *Journal) addEntryBindingLocked(binding EntryBinding, now time.Time) (EntryBindingNote, error) {
	var note EntryBindingNote
	state, found, err := j.EntryBinding()
	switch {
	case errors.Is(err, ErrCorrupt):
		note.Replaced = err
		state, found = EntryBindingState{}, false
	case err != nil:
		return EntryBindingNote{}, err
	case found && state.Index(binding) >= 0:
		return EntryBindingNote{}, nil
	}
	if !found {
		state = EntryBindingState{Namespace: j.auth.Namespace}
	}
	note.Added = true
	note.Others = append([]EntryBindingSighting(nil), state.Bindings...)
	bindings := make([]EntryBindingSighting, 0, len(state.Bindings)+1)
	bindings = append(bindings, state.Bindings...)
	bindings = append(bindings, EntryBindingSighting{Binding: binding, FirstSeen: now.UTC()})
	if excess := len(bindings) - maxEntryBindings; excess > 0 {
		note.Dropped = append([]EntryBindingSighting(nil), bindings[:excess]...)
		bindings = bindings[excess:]
	}
	state.Bindings = bindings
	return note, atomicEnvelope(j.root, entryBindingFile, entryBindingVersion, state, j.ops)
}

// waitEntryBindingLock waits for entry-binding.lock. Unlike index.lock, which
// Initialize creates once, this lock file is created by whichever process
// first needs it, so several creates can race to create it. macOS can fail
// the loser's O_CREATE open with ENOENT while the winner's create is in
// flight; the file exists a moment later, so that error is retried a few
// times within the same deadline. A namespace directory that is really gone
// keeps failing and the error is returned.
func (j *Journal) waitEntryBindingLock(ctx context.Context) (*fileLock, error) {
	const createRaceRetries = 5
	for attempt := 0; ; attempt++ {
		lock, err := waitLock(ctx, j.root, entryBindingLockFile)
		if !errors.Is(err, os.ErrNotExist) || attempt == createRaceRetries {
			return lock, err
		}
		if err := pause(ctx); err != nil {
			return nil, err
		}
	}
}
