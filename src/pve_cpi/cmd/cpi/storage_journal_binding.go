package main

import (
	"errors"
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

// storageJournalBindingJobLevel tells the operator which configuration the
// audit compared against. The command loads the job-level properties only and
// never resolves a cpi_config entry, so a binding that a cpi_config entry
// wrote can look foreign here even when that entry is configured correctly.
const storageJournalBindingJobLevel = "This audit compares against the job-level configuration only, not the cpi_config entries"

// auditStorageJournalEntryBinding reports every binding the namespace has
// recorded and compares the set with the binding cfg resolves to.
//
// It reports four things. A sidecar written by a newer release, or one that
// can't be read, is a finding because the shared-namespace check could not
// run. A set holding more than one binding is a finding, because either two
// cpi_config entries share the namespace or a prefix or band was edited, and
// the audit can't tell those apart. A single binding that differs from cfg
// names the fields that differ. A single binding that matches cfg is clean.
// Every finding that compares with cfg says the comparison is against the
// job-level configuration.
func auditStorageJournalEntryBinding(journal *aj.Journal, cfg *config.CPIConfig) storageJournalBindingAudit {
	if journal == nil {
		return storageJournalBindingAudit{}
	}
	state, found, err := journal.EntryBinding()
	switch {
	case errors.Is(err, aj.ErrUnsupportedEntryBinding):
		return storageJournalBindingAudit{Finding: "entry binding was written by a newer CPI release, which this audit cannot read, " +
			"so the shared-namespace check could not run (" + log.ScrubMessage(err.Error()) + ")"}
	case err != nil:
		return storageJournalBindingAudit{Finding: "entry binding unreadable, so the shared-namespace check could not run (" +
			log.ScrubMessage(err.Error()) + ")"}
	case !found:
		return storageJournalBindingAudit{}
	}
	current := handlers.JournalEntryBinding(cfg)
	out := storageJournalBindingAudit{State: &state}
	match := state.Index(current)
	if len(state.Bindings) > 1 {
		standing := "the job-level configuration matches none of them"
		if match >= 0 {
			standing = fmt.Sprintf("the job-level configuration matches binding %d", match+1)
		}
		out.Finding = fmt.Sprintf("namespace has recorded %d entry bindings, and %s. "+
			"Two cpi_config entries on this cluster may share storage_placement_namespace, or a prefix or VMID band was edited. %s. "+
			"After confirming every binding but the current one came from an intended edit, delete entry-binding.json from the namespace directory to reset the set",
			len(state.Bindings), standing, storageJournalBindingJobLevel)
		return out
	}
	if match < 0 {
		recorded := state.Bindings[0]
		out.Finding = fmt.Sprintf("recorded binding differs from this configuration in %s; recorded %s first seen %s, configured %s. "+
			"Another cpi_config entry on this cluster may share storage_placement_namespace. %s",
			strings.Join(recorded.Binding.Differences(current), ","), recorded.Binding,
			recorded.FirstSeen.UTC().Format(time.RFC3339), current, storageJournalBindingJobLevel)
	}
	return out
}

// storageJournalBindingLines renders the recorded bindings for the text
// summary, one line each and oldest first, followed by the finding.
func storageJournalBindingLines(state *aj.EntryBindingState, finding string) []string {
	var lines []string
	if state != nil {
		for i, seen := range state.Bindings {
			lines = append(lines, fmt.Sprintf("entry binding %d: %s first seen %s", i+1, seen.Binding,
				seen.FirstSeen.UTC().Format(time.RFC3339)))
		}
	}
	if finding != "" {
		lines = append(lines, "entry binding finding: "+finding)
	}
	return lines
}
