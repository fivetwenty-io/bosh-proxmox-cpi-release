package handlers

import (
	"context"
	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"testing"
)

func TestExplicitCleanupUnreturnedManagedDisk(t *testing.T) {
	deps, client, journal, id, _ := lifecycleFlowFixtureState(t, false)
	delete(client.state.configs[777], "scsi1")
	handle, err := journal.Acquire(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := handle.Close(); err != nil {
			t.Error(err)
		}
	}()
	proof, err := cleanupManagedDiskAllocation(context.Background(), deps, journal, handle)
	if err != nil {
		t.Fatal(err)
	}
	if !proof.Complete || !proof.AbsenceVerified || !proof.ArtifactDispositionVerified || client.deletes != 1 {
		t.Fatal("cleanup lacks complete disposition")
	}
	record := handle.Record()
	if record.CID != "" || record.ID != id {
		t.Fatal("unreturned cleanup invented caller CID or new identity")
	}
	record.State = aj.Cleaned
	record.Reason = ""
	record.Verifications = append(record.Verifications, proof)
	if err := handle.Save(record); err != nil {
		t.Fatal(err)
	}
}

func TestExplicitCleanupAttachedManagedDiskRefusesBeforeMutation(t *testing.T) {
	deps, client, journal, id, _ := lifecycleFlowFixtureState(t, false)
	handle, err := journal.Acquire(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := handle.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := cleanupManagedDiskAllocation(context.Background(), deps, journal, handle); err == nil {
		t.Fatal("attached disk cleanup accepted")
	}
	if client.deletes != 0 || client.moves != 0 {
		t.Fatal("attached disk mutated")
	}
}
