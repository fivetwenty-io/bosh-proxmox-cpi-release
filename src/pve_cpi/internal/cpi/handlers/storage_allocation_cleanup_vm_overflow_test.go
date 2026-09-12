package handlers

import (
	"encoding/json"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	inv "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/storageinventory"
	"math"
	"testing"
)

func TestCleanupPendingVMAllocationChargeIntegerBoundaries(t *testing.T) {
	for _, role := range []string{storageRoleRoot, storageRoleEphemeral} {
		t.Run(role, func(t *testing.T) {
			for _, tc := range []struct {
				name         string
				bytes        uint64
				journalBytes int64
				allowed      bool
			}{
				{"maximum_signed", math.MaxInt64, math.MaxInt64, true},
				{"first_overflow", uint64(math.MaxInt64) + 1, math.MinInt64, false},
				{"maximum_unsigned", math.MaxUint64, -1, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					_, _, _, record, decision := unknownVMAllocationFixture(t, role)
					plan, err := activeStorageAllocationPlan(record)
					if err != nil {
						t.Fatal(err)
					}
					step := record.Steps[len(record.Steps)-1]
					if role == storageRoleRoot {
						step.State = aj.Submitted
						step.UPID = decision.RecoveredTaskUPID
					}
					plan.Charges = []inv.ChargeRecord{{Charge: inv.Charge{Role: role, Bytes: tc.bytes}, CapacityKey: step.Target.Backing}}
					payload, err := json.Marshal(plan)
					if err != nil {
						t.Fatal(err)
					}
					if len(record.Attempts) == 0 {
						record.Intent.Plan = payload
					} else {
						record.Attempts[len(record.Attempts)-1].Plan.Plan = payload
					}
					step.Charges = []aj.Charge{{Backing: step.Target.Backing, PlannedBytes: tc.journalBytes, OutstandingBytes: tc.journalBytes}}
					if actual := cleanupPendingVMAllocation(step, record); actual != tc.allowed {
						t.Fatalf("cleanup admission = %v; want %v", actual, tc.allowed)
					}
				})
			}
		})
	}
}
