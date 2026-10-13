package main

import (
	"fmt"
	"strings"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

// storageJournalBindingAudit is what the audit learned from the namespace's
// entry-binding sidecar. State is nil when no create has recorded a binding
// yet or the sidecar could not be read. Finding is empty when there is
// nothing to report. Neither field affects the audit's exit code, because the
// binding is advisory and a mismatch never blocks allocation.
type storageJournalBindingAudit struct {
	State   *aj.EntryBindingState
	Finding string
}

// auditStorageJournalEntryBinding compares the recorded entry binding with
// the one cfg resolves to. The command reads the job-level configuration, so
// in a deployment with several cpi_config entries the comparison is against
// the job's own values; the finding says so rather than claim which entry
// wrote the sidecar.
//
// It reports three things. A sidecar that can't be read is a finding, because
// the shared-namespace check could not run. A recorded binding that differs
// from cfg names the fields that differ. A recorded binding that matches but
// replaced an earlier, different one says when that happened, because two
// entries sharing the namespace overwrite each other on every create and the
// job-level view would otherwise look clean whenever the job's entry wrote
// last.
func auditStorageJournalEntryBinding(journal *aj.Journal, cfg *config.CPIConfig) storageJournalBindingAudit {
	if journal == nil {
		return storageJournalBindingAudit{}
	}
	state, found, err := journal.EntryBinding()
	if err != nil {
		return storageJournalBindingAudit{Finding: "entry binding unreadable, so the shared-namespace check could not run (" +
			log.ScrubMessage(err.Error()) + ")"}
	}
	if !found {
		return storageJournalBindingAudit{}
	}
	current := handlers.JournalEntryBinding(cfg)
	out := storageJournalBindingAudit{State: &state}
	if differences := state.Binding.Differences(current); len(differences) > 0 {
		out.Finding = fmt.Sprintf("recorded binding differs from this configuration in %s; recorded %s at %s, configured %s. "+
			"Another cpi_config entry on this cluster may share storage_placement_namespace",
			strings.Join(differences, ","), state.Binding, state.RecordedAt.UTC().Format(time.RFC3339), current)
		return out
	}
	if state.Previous != nil {
		previousAt := "an unknown time"
		if state.PreviousRecordedAt != nil {
			previousAt = state.PreviousRecordedAt.UTC().Format(time.RFC3339)
		}
		out.Finding = fmt.Sprintf("binding matches this configuration and replaced a different one at %s; previous %s recorded at %s. "+
			"If this repeats, two cpi_config entries on this cluster share storage_placement_namespace",
			state.RecordedAt.UTC().Format(time.RFC3339), *state.Previous, previousAt)
	}
	return out
}
