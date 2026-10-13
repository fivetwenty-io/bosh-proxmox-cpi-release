package handlers

import (
	"context"
	"errors"
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
// only in how they spell a default produce the same binding.
func JournalEntryBinding(cfg *config.CPIConfig) aj.EntryBinding {
	if cfg == nil {
		return aj.EntryBinding{ParkerPrefix: cfg.ParkerPrefixValue()}
	}
	return aj.EntryBinding{
		VMPrefix:             cfg.VMPrefix,
		ParkerPrefix:         cfg.ParkerPrefixValue(),
		VMIDRangeStart:       cfg.VMIDRangeStart,
		VMIDRangeEnd:         cfg.VMIDRangeEnd,
		DiskVMIDRangeStart:   cfg.DiskVMIDRangeStart,
		DiskVMIDRangeEnd:     cfg.DiskVMIDRangeEnd,
		ParkedVMIDRangeStart: cfg.ParkedDiskVMIDRangeStartValue(),
		ParkedVMIDRangeEnd:   cfg.ParkedDiskVMIDRangeEndValue(),
	}
}

// noteJournalEntryBinding compares the effective entry's binding with the one
// the namespace last recorded, warns when they differ, and records the current
// one. Admission calls it once the audit has admitted a create, so it runs only
// on the paths that are about to write a new record.
//
// Two cpi_config entries on one cluster that resolve to the same journal
// namespace share admission state. When their prefixes or VMID bands differ,
// each entry's parker and orphan handling reads the other's resources as
// foreign, which is the configuration this check exists to surface. An honest
// edit to the prefix or a band trips it once, and the next create finds the
// new binding already recorded.
//
// The sidecar is advisory. A failed read or write is logged as a warning and
// never fails the CPI call, and an unchanged binding is not rewritten, so the
// steady state costs one small file read and no fsync.
func noteJournalEntryBinding(ctx context.Context, deps Deps, journal *aj.Journal) {
	if journal == nil || deps.Config == nil {
		return
	}
	logger := deps.Log(ctx)
	namespace := deps.Config.StoragePlacementNamespace
	current := JournalEntryBinding(deps.Config)
	recorded, found, err := journal.EntryBinding()
	switch {
	case err != nil && !errors.Is(err, aj.ErrCorrupt):
		logger.Warn("allocation journal entry binding could not be read; the shared-namespace check is skipped for this call",
			log.String("namespace", namespace), log.ErrScrubbed(err))
		return
	case err != nil:
		logger.Warn("allocation journal entry binding is unreadable; recording the current entry's binding in its place",
			log.String("namespace", namespace), log.ErrScrubbed(err),
			log.String("current_binding", current.String()))
		found = false
	case found && recorded.Binding == current:
		return
	case found:
		logger.Warn("allocation journal entry binding differs from the one this namespace last recorded; "+
			"two cpi_config entries on one cluster may share this storage_placement_namespace, or a prefix or VMID band was edited. "+
			"Give each entry its own namespace, prefixes, and bands, or disregard this once after an intended edit",
			log.String("namespace", namespace),
			log.String("changed_fields", strings.Join(recorded.Binding.Differences(current), ",")),
			log.String("recorded_binding", recorded.Binding.String()),
			log.String("recorded_at", recorded.RecordedAt.UTC().Format(time.RFC3339)),
			log.String("current_binding", current.String()))
	}
	var replaced *aj.EntryBindingState
	if found {
		replaced = &recorded
	}
	if err := journal.RecordEntryBinding(current, replaced, time.Now()); err != nil {
		logger.Warn("allocation journal entry binding could not be recorded; allocation continues",
			log.String("namespace", namespace), log.ErrScrubbed(err))
	}
}
