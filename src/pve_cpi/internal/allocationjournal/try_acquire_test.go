package allocationjournal

import (
	"context"
	"testing"
)

// TryAcquire takes a record's lock when it is free and returns at once, with
// no handle and no error, when another holder has it.
func TestTryAcquireTakesAFreeRecordAndRefusesAHeldOne(t *testing.T) {
	j, _ := fixture(t)
	h := retryVM(t, j)
	id := h.Record().ID

	if got, ok, err := j.TryAcquire(context.Background(), id); err != nil || ok || got != nil {
		t.Fatalf("TryAcquire of a held record: handle %v ok %v err %v; want a refusal with no error", got, ok, err)
	}
	closeHandle(t, h)

	got, ok, err := j.TryAcquire(context.Background(), id)
	if err != nil || !ok || got == nil {
		t.Fatalf("TryAcquire of a free record: handle %v ok %v err %v", got, ok, err)
	}
	defer closeHandle(t, got)
	if got.Record().ID != id || !got.Resumed {
		t.Fatalf("record %q resumed %v; want the record Acquire returns", got.Record().ID, got.Resumed)
	}
	if _, ok, err := j.TryAcquire(context.Background(), "not-a-uuid"); err == nil || ok {
		t.Fatal("TryAcquire took an invalid allocation ID")
	}
}
