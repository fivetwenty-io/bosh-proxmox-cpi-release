package allocationjournal

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// entryBindingFile is the sidecar that remembers which cpi_config entry last
// created a record in this namespace. The name is chosen against every reader
// of the namespace directory: List, lookupRecords, and InspectDiskToken only
// decode names carrying the "allocation-" prefix and ".json" suffix, the
// fixed names authority.json and index.json are read by exact name, lock files
// end in ".lock", and atomicJSON's scratch files start with ".tmp-". The
// sidecar matches none of those, so no reader in this release or in releases
// that predate the sidecar decodes it as a record, counts it, or removes it.
const entryBindingFile = "entry-binding.json"

// entryBindingVersion is the sidecar payload schema. It is separate from the
// record schema Version because the sidecar is advisory and evolves on its
// own.
const entryBindingVersion = 1

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

// EntryBindingState is the sidecar payload. RecordedAt is when Binding was
// first written; an unchanged binding is never rewritten. Previous and
// PreviousRecordedAt carry the binding this one replaced, so an audit can name
// both sides of a change after the warning has scrolled out of the logs.
type EntryBindingState struct {
	Version            int           `json:"version"`
	Namespace          string        `json:"namespace"`
	Binding            EntryBinding  `json:"binding"`
	RecordedAt         time.Time     `json:"recorded_at"`
	Previous           *EntryBinding `json:"previous,omitempty"`
	PreviousRecordedAt *time.Time    `json:"previous_recorded_at,omitempty"`
}

// EntryBinding reads the namespace's entry-binding sidecar. found is false
// with a nil error when no sidecar has been written. A sidecar that fails the
// envelope checks, names another namespace, or carries an unusable binding
// returns an error wrapping ErrCorrupt; any other error is an I/O or
// permission failure. The sidecar is advisory, so callers report these errors
// and carry on rather than failing a CPI call on them.
func (j *Journal) EntryBinding() (state EntryBindingState, found bool, err error) {
	if j == nil || j.root == nil {
		return state, false, ErrClosed
	}
	if err := readJSON(j.root, entryBindingFile, &state); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return EntryBindingState{}, false, nil
		}
		return EntryBindingState{}, false, err
	}
	if state.Version != entryBindingVersion {
		return EntryBindingState{}, false, fmt.Errorf("%w: entry binding version %d", ErrCorrupt, state.Version)
	}
	if state.Namespace != j.auth.Namespace {
		return EntryBindingState{}, false, fmt.Errorf("%w: misplaced entry binding", ErrCorrupt)
	}
	if err := validateEntryBinding(state.Binding); err != nil {
		return EntryBindingState{}, false, fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	if state.Previous != nil {
		if err := validateEntryBinding(*state.Previous); err != nil {
			return EntryBindingState{}, false, fmt.Errorf("%w: previous %w", ErrCorrupt, err)
		}
	}
	return state, true, nil
}

// RecordEntryBinding replaces the sidecar with binding, recorded at now. When
// replaced is non-nil its binding and timestamp become the new state's
// Previous fields. The write uses the same temp-file, fsync, rename, and
// directory-fsync sequence as every journal record, so a crash leaves either
// the old sidecar or the new one and never a torn file.
//
// The write takes no journal lock. Two entries racing to record different
// bindings each land a complete file and the last rename wins; the loser's
// binding is the one a later admission compares against and warns about, so
// nothing a lock would protect is lost.
func (j *Journal) RecordEntryBinding(binding EntryBinding, replaced *EntryBindingState, now time.Time) error {
	if j == nil || j.root == nil {
		return ErrClosed
	}
	if err := validateEntryBinding(binding); err != nil {
		return err
	}
	if now.IsZero() {
		return errors.New("journal: entry binding needs a recording time")
	}
	state := EntryBindingState{
		Version:    entryBindingVersion,
		Namespace:  j.auth.Namespace,
		Binding:    binding,
		RecordedAt: now.UTC(),
	}
	if replaced != nil {
		previous := replaced.Binding
		recordedAt := replaced.RecordedAt.UTC()
		state.Previous = &previous
		state.PreviousRecordedAt = &recordedAt
	}
	return atomicJSON(j.root, entryBindingFile, state, j.ops)
}
