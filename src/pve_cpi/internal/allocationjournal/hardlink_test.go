package allocationjournal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// privateInfo refuses a journal file that something else also links, because a
// second link lets whoever holds it read or rewrite the record behind the
// journal's back. Nothing pinned that refusal before, so relaxing the predicate
// from "!= 1" to "> 1" to stop it firing on the zero-link inode an in-flight
// rename leaves behind is pinned here instead of trusted.
func TestHardLinkedRecordIsRejected(t *testing.T) {
	j, dir := fixture(t)
	ctx := context.Background()

	h, err := j.AcquireVM(ctx, "agent", intent())
	if err != nil {
		t.Fatal(err)
	}
	id := h.Record().ID
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	ns := filepath.Join(dir, pathKey("namespace"))
	record := filepath.Join(ns, recordName(id))
	if _, err := os.Lstat(record); err != nil {
		t.Fatalf("record file missing: %v", err)
	}
	if err := os.Link(record, filepath.Join(ns, "attacker-copy")); err != nil {
		t.Skipf("filesystem does not support hard links: %v", err)
	}

	_, _, err = j.InspectVMContext(ctx, "agent")
	if err == nil {
		t.Fatal("a multiply linked record must be refused")
	}
	if !strings.Contains(err.Error(), "multiply linked file rejected") {
		t.Fatalf("wrong refusal: %v", err)
	}
}

// The zero-link inode a concurrent rename leaves behind is not a second link
// and must not be refused as one.
func TestUnlinkedInodeIsNotTreatedAsMultiplyLinked(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "doomed")
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	// Drop the only name while the descriptor stays open, which is the state a
	// reader observes mid-rename: a live inode with a link count of zero.
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := privateInfo(info, false); err != nil {
		t.Fatalf("an unlinked inode must not be refused: %v", err)
	}
}

// A reader that loses the rename race reopens instead of demanding
// reconciliation, so a record saved while another handle reads it does not fail
// the read outright.
func TestReadJSONSurvivesConcurrentRename(t *testing.T) {
	j, _ := fixture(t)
	ctx := context.Background()

	h, err := j.AcquireVM(ctx, "agent", intent())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			r := h.Record()
			if err := h.Save(r); err != nil {
				return
			}
		}
	}()

	for i := 0; i < 200; i++ {
		if _, _, err := j.InspectVMContext(ctx, "agent"); err != nil {
			if errors.Is(err, ErrReconciliationRequired) {
				continue
			}
			t.Fatalf("read failed during concurrent save: %v", err)
		}
	}
	<-done
}
