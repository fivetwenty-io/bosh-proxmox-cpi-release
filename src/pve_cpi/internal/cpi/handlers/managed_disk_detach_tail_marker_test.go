package handlers

// These rows check which detach tail failures carry the marker that lets the
// lifecycle hand a disk's allocation back unchanged. They name the marker's
// helpers, so they live apart from the rows that drive the same calls.

import "testing"

// TestDetachTailMarkerSkipsUnsettledRefusal is the answered refusal whose
// readback shows a note gone. The guard can't settle it, so the error carries
// no marker, and the lifecycle doesn't return the allocation.
func TestDetachTailMarkerSkipsUnsettledRefusal(t *testing.T) {
	err := answeredRefusalWithNoteGone(t)
	if isDetachTailNotSent(err) || isDiskReturnedUnchanged(err) {
		t.Fatalf("err = %v, want an unsettled refusal left unmarked", err)
	}
}

// TestDetachTailMarkerSkipsRefusalAfterMove is the detach whose tail is
// refused after the same call moved the disk. The call changed the disk, so
// the lifecycle doesn't mark the allocation as returned unchanged.
func TestDetachTailMarkerSkipsRefusalAfterMove(t *testing.T) {
	if err := refusedAfterMove(t); isDiskReturnedUnchanged(err) {
		t.Fatalf("err = %v, want it not marked as returning the disk unchanged", err)
	}
}
