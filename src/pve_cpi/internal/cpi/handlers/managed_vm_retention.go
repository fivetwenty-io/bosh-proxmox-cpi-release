package handlers

import (
	"fmt"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
)

// Retention is an operator-policy decision, not a claim that an uncertain
// remote mutation completed. Existing resource evidence remains untouched.
func (m *managedVMAllocation) retainFailedAttempt() error {
	record := m.handle.Record()
	if len(record.Steps) == 0 {
		return nil
	}
	for i := range record.Steps {
		step := &record.Steps[i]
		if step.Attempt == record.ActiveAttempt() && step.Kind == "vm.keep_failed" {
			return nil
		}
	}
	record.Steps = append(record.Steps, aj.Step{ID: fmt.Sprintf("attempt-%d-step-%d", record.ActiveAttempt(), len(record.Steps)), Attempt: record.ActiveAttempt(), Kind: "vm.keep_failed", Target: aj.Target{Node: m.shape.node, VMID: m.vmid}, State: aj.Planned})
	record.State = aj.ReconciliationRequired
	record.Reason = "keep_failed_vms retains this attempt for operator inspection; automatic replacement is prohibited"
	if err := m.handle.Save(record); err != nil {
		return err
	}
	record = m.handle.Record()
	record.Steps[len(record.Steps)-1].State = aj.Observed
	return m.handle.Save(record)
}
