package handlers

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// JournalEntryBinding returns the identity cfg writes under in its allocation
// journal namespace: the VM name prefix, the resolved parker prefix, and the
// VM, disk, and parked-disk VMID bands. The parker prefix and parked band go
// through the same resolution the parker code uses, so two entries that differ
// only in how they spell a default produce the same binding. The parker
// prefix is lowercased because the parker code matches prefixes without
// regard to case; the VM prefix is kept as written because PVE VM names are
// case-sensitive. A nil cfg resolves every default, as an empty configuration
// would.
func JournalEntryBinding(cfg *config.CPIConfig) aj.EntryBinding {
	if cfg == nil {
		cfg = &config.CPIConfig{}
	}
	return aj.EntryBinding{
		VMPrefix:             cfg.VMPrefix,
		ParkerPrefix:         strings.ToLower(cfg.ParkerPrefixValue()),
		VMIDRangeStart:       cfg.VMIDRangeStart,
		VMIDRangeEnd:         cfg.VMIDRangeEnd,
		DiskVMIDRangeStart:   cfg.DiskVMIDRangeStart,
		DiskVMIDRangeEnd:     cfg.DiskVMIDRangeEnd,
		ParkedVMIDRangeStart: cfg.ParkedDiskVMIDRangeStartValue(),
		ParkedVMIDRangeEnd:   cfg.ParkedDiskVMIDRangeEndValue(),
	}
}

// formatEntryBindingSightings renders a set of bindings for one log field or
// audit line, oldest first, each with the time it first admitted a create.
func formatEntryBindingSightings(sightings []aj.EntryBindingSighting) string {
	parts := make([]string, 0, len(sightings))
	for _, seen := range sightings {
		parts = append(parts, seen.Binding.String()+" first seen "+seen.FirstSeen.UTC().Format(time.RFC3339))
	}
	return strings.Join(parts, "; ")
}

// noteJournalEntryBinding adds the effective entry's binding to the set of
// bindings its journal namespace has seen, and warns when a binding first
// appears beside others. Admission calls it once the audit has admitted a
// create, so a refused create never records anything.
//
// Two cpi_config entries on one cluster that resolve to the same journal
// namespace share admission state. When their prefixes or VMID bands differ,
// each entry's parker and orphan handling reads the other's resources as
// foreign, which is the configuration this check exists to surface. An
// intended edit to a prefix or a band adds one binding and warns once.
//
// The sidecar is advisory. Every failure is logged and never fails the CPI
// call. A binding already in the set costs one small file read, with no lock
// and no write.
func noteJournalEntryBinding(ctx context.Context, deps Deps, journal *aj.Journal) {
	if journal == nil || deps.Config == nil {
		return
	}
	logger := deps.Log(ctx)
	namespace := deps.Config.StoragePlacementNamespace
	current := JournalEntryBinding(deps.Config)
	note, err := journal.NoteEntryBinding(ctx, current, time.Now())
	if note.Replaced != nil {
		logger.Warn("allocation journal entry binding is unreadable; recording the current entry's binding in a fresh set",
			log.String("namespace", namespace), log.ErrScrubbed(note.Replaced),
			log.String("current_binding", current.String()))
	}
	if note.Added && len(note.Others) > 0 {
		newest := note.Others[len(note.Others)-1].Binding
		fields := []log.Field{
			log.String("namespace", namespace),
			log.String("new_binding", current.String()),
			log.String("changed_fields", strings.Join(newest.Differences(current), ",")),
			log.Int("recorded_bindings_count", len(note.Others)),
			log.String("recorded_bindings", formatEntryBindingSightings(note.Others)),
		}
		if len(note.Dropped) > 0 {
			fields = append(fields, log.String("dropped_bindings", formatEntryBindingSightings(note.Dropped)))
		}
		logger.Warn("allocation journal entry binding is new to this namespace, which has already recorded "+
			strconv.Itoa(len(note.Others))+" other binding(s); two cpi_config entries on one cluster may share this "+
			"storage_placement_namespace, or a prefix or VMID band was edited. Give each entry its own namespace or "+
			"matching prefixes and bands. This warning appears once for each new binding, and storage-journal audit lists them all",
			fields...)
	}
	var durability *aj.DurabilityError
	switch {
	case err == nil:
	case errors.Is(err, aj.ErrUnsupportedEntryBinding):
		logger.Warn("allocation journal entry binding was written by a newer CPI release; it is left untouched and the shared-namespace check is skipped",
			log.String("namespace", namespace), log.ErrScrubbed(err))
	case errors.Is(err, aj.ErrEntryBindingBusy):
		logger.Debug("allocation journal entry binding lock is busy; a later create records this entry's binding",
			log.String("namespace", namespace), log.String("current_binding", current.String()))
	case errors.As(err, &durability):
		logger.Warn("allocation journal entry binding could not be recorded; allocation continues",
			log.String("namespace", namespace), log.ErrScrubbed(err))
	default:
		logger.Warn("allocation journal entry binding could not be read; the shared-namespace check is skipped for this call",
			log.String("namespace", namespace), log.ErrScrubbed(err))
	}
}
