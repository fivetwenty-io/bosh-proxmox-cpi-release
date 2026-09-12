package storageinventory

import (
	"context"
	"testing"
	"time"
)

func TestRestoreLedgerDoesNotDoubleChargeAcquiredBase(t *testing.T) {
	source := sourceFor(t, "a")
	source.set("n1", status(t, "a", 1000, 900))
	clock := newClock()
	collector := collector(t, source, clock)
	snapshot := discover(t, collector, policy("a"), "n1")
	initial := plan(t, NewLedger(), snapshot, "root_base", "a", 600)
	initial = plan(t, initial, snapshot, "root_growth", "a", 200)
	initial = submit(t, initial, "root_base")
	initial = submit(t, initial, "root_growth")
	records := initial.Records()
	proof, err := collector.MarkCompletion(records[0], "a:vm-101-root", true, true)
	if err != nil {
		t.Fatal(err)
	}
	records[0].Acquired = true
	records[0].VolumeID = "a:vm-101-root"
	source.set("n1", status(t, "a", 1000, 300))
	clock.Advance(time.Second)
	fresh, err := collector.Refresh(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := RestoreLedger(fresh, records, map[string]CompletionEvidence{"root_base": proof}, clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = restored.Candidate(fresh, "n1", "a", 100, Limits{}); err != nil {
		t.Fatalf("base charged twice: %v", err)
	}
	if _, err = restored.Candidate(fresh, "n1", "a", 101, Limits{}); err == nil {
		t.Fatal("growth reservation lost")
	}
	if !restored.Records()[0].Reflected || restored.Records()[1].Acquired {
		t.Fatal("incorrect restored charge states")
	}
	for _, bad := range []string{"missing-proof", "rebound", "changed-bytes", "extra-proof", "stale"} {
		t.Run(bad, func(t *testing.T) {
			mutated := append([]ChargeRecord(nil), records...)
			proofs := map[string]CompletionEvidence{"root_base": proof}
			at := clock.Now()
			switch bad {
			case "missing-proof":
				delete(proofs, "root_base")
			case "rebound":
				mutated[0].CapacityKey = "other"
			case "changed-bytes":
				mutated[0].Charge.Bytes++
			case "extra-proof":
				proofs["other"] = proof
			case "stale":
				at = at.Add(time.Hour)
			}
			if _, err := RestoreLedger(fresh, mutated, proofs, at); err == nil {
				t.Fatal("unsafe recovery accepted")
			}
		})
	}
	// The old snapshot predates the fresh proof. It may not release base bytes.
	source.set("n1", status(t, "a", 1000, 300))
	beforeProof, err := collector.Refresh(context.Background(), fresh)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	laterProof, err := collector.MarkCompletion(initial.Records()[0], "a:vm-101-root", true, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = RestoreLedger(beforeProof, records, map[string]CompletionEvidence{"root_base": laterProof}, clock.Now()); err == nil {
		t.Fatal("snapshot predating completion retired outstanding base")
	}
}
