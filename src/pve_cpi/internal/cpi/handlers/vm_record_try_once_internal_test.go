package handlers

import (
	"context"
	"errors"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
)

// TestVMRecordTryOnceSkipsAHeldRecordWithoutWaiting holds a live journal
// record the way a concurrent call does. A try-once acquire returns
// errVMRecordBusy at once and doesn't run fn, and once the holder lets go the
// same acquire runs fn with the record.
func TestVMRecordTryOnceSkipsAHeldRecordWithoutWaiting(t *testing.T) {
	deps, _, journal, id, _ := lifecycleFlowFixture(t)
	pick := func(records []aj.Record) (*aj.Record, error) {
		for i := range records {
			if records[i].ID == id {
				return &records[i], nil
			}
		}
		return nil, errors.New("record missing")
	}
	held, err := journal.Acquire(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	ran := false
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	start := time.Now()
	err = withVMRecordWhere(ctx, deps, pick, vmRecordTryOnce, func(*aj.Handle) error { ran = true; return nil })
	if !errors.Is(err, errVMRecordBusy) || ran {
		t.Fatalf("held record: err=%v ran=%v", err, ran)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("try-once waited %s for a held record", elapsed)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	err = withVMRecordWhere(ctx, deps, pick, vmRecordTryOnce, func(h *aj.Handle) error {
		ran = h != nil
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("free record: err=%v ran=%v", err, ran)
	}
}
