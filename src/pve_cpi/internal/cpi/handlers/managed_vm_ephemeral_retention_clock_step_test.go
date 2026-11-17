package handlers

import (
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
)

// TestEphemeralRetentionSurvivesWallClockStepBack steps the journal's clock a
// minute behind the one that created the allocation record, as a host that
// resyncs its clock does between two saves. The retention's guarded writes
// still pass admission and the delete completes, because a save never stamps
// the record earlier than its creation.
func TestEphemeralRetentionSurvivesWallClockStepBack(t *testing.T) {
	c := newRetentionCase(t, true)
	t.Cleanup(aj.SetClockForTest(func() time.Time { return time.Now().Add(-time.Minute) }))
	if body := c.deleteVM(); body != nil {
		t.Fatalf("the clean delete failed: %+v", body)
	}
	if record := c.record(t); record.State != aj.VMDeletedRetained {
		t.Fatalf("the clean delete left %s", record.State)
	}
}
