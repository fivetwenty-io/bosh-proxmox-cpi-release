// parker_internal_test.go — white-box tests for unexported parker helpers.
// Package pve gives access to chooseParkSlot and other unexported symbols.
package pve

import (
	"errors"
	"fmt"
	"testing"
)

// ---------------------------------------------------------------------------
// chooseParkSlot
// ---------------------------------------------------------------------------

func TestChooseParkSlot_EmptyParker(t *testing.T) {
	t.Parallel()
	slot, err := chooseParkSlot(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if slot != "scsi0" {
		t.Errorf("want scsi0, got %q", slot)
	}
}

func TestChooseParkSlot_EmptyMap(t *testing.T) {
	t.Parallel()
	slot, err := chooseParkSlot(map[string]string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if slot != "scsi0" {
		t.Errorf("want scsi0, got %q", slot)
	}
}

func TestChooseParkSlot_Scsi0Taken(t *testing.T) {
	t.Parallel()
	disks := map[string]string{
		"scsi0": "local-lvm:vm-9000-disk-0",
	}
	slot, err := chooseParkSlot(disks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if slot != "scsi1" {
		t.Errorf("want scsi1, got %q", slot)
	}
}

func TestChooseParkSlot_HolesInMiddle(t *testing.T) {
	t.Parallel()
	// scsi0 and scsi2 taken; scsi1 is the hole → want scsi1.
	disks := map[string]string{
		"scsi0": "local-lvm:vm-9000-disk-0",
		"scsi2": "local-lvm:vm-9000-disk-1",
	}
	slot, err := chooseParkSlot(disks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if slot != "scsi1" {
		t.Errorf("want scsi1 (first hole), got %q", slot)
	}
}

func TestChooseParkSlot_AllSlotsOccupied_ErrNoSlots(t *testing.T) {
	t.Parallel()
	disks := make(map[string]string, parkerMaxSlots)
	for i := 0; i < parkerMaxSlots; i++ {
		disks[fmt.Sprintf("scsi%d", i)] = fmt.Sprintf("local-lvm:vm-9000-disk-%d", i)
	}
	_, err := chooseParkSlot(disks)
	if err == nil {
		t.Fatal("expected ErrNoSlots for fully occupied parker")
	}
	if !errors.Is(err, ErrNoSlots) {
		t.Errorf("expected errors.Is(err, ErrNoSlots); got: %v", err)
	}
}

func TestChooseParkSlot_Last30Taken_Scsi30Free(t *testing.T) {
	t.Parallel()
	// scsi0..scsi29 taken; scsi30 is the last free slot.
	disks := make(map[string]string, 30)
	for i := 0; i < 30; i++ {
		disks[fmt.Sprintf("scsi%d", i)] = fmt.Sprintf("local-lvm:vm-9000-disk-%d", i)
	}
	slot, err := chooseParkSlot(disks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if slot != "scsi30" {
		t.Errorf("want scsi30, got %q", slot)
	}
}

// ---------------------------------------------------------------------------
// tagContainsParker
// ---------------------------------------------------------------------------

func TestTagContainsParker_Single(t *testing.T) {
	t.Parallel()
	if !tagContainsParker("bosh-parker") {
		t.Error("expected true for single-tag string")
	}
}

func TestTagContainsParker_MultipleWithParker(t *testing.T) {
	t.Parallel()
	if !tagContainsParker("bosh-stemcell;bosh-parker;director--prod") {
		t.Error("expected true when bosh-parker is present among other tags")
	}
}

func TestTagContainsParker_Absent(t *testing.T) {
	t.Parallel()
	if tagContainsParker("bosh-stemcell;director--prod") {
		t.Error("expected false when bosh-parker is absent")
	}
}

func TestTagContainsParker_Empty(t *testing.T) {
	t.Parallel()
	if tagContainsParker("") {
		t.Error("expected false for empty tag string")
	}
}

func TestTagContainsParker_CaseInsensitive(t *testing.T) {
	t.Parallel()
	if !tagContainsParker("Bosh-Parker") {
		t.Error("expected true: tag comparison is case-insensitive")
	}
}

// ---------------------------------------------------------------------------
// sanitizeParkerTagValue
// ---------------------------------------------------------------------------

func TestSanitizeParkerTagValue_AlphanumericUnchanged(t *testing.T) {
	t.Parallel()
	got := sanitizeParkerTagValue("prod-director123")
	if got != "prod-director123" {
		t.Errorf("want %q, got %q", "prod-director123", got)
	}
}

func TestSanitizeParkerTagValue_StripsSpaces(t *testing.T) {
	t.Parallel()
	got := sanitizeParkerTagValue("hello world")
	if got != "helloworld" {
		t.Errorf("want %q, got %q", "helloworld", got)
	}
}

func TestSanitizeParkerTagValue_EmptyInput(t *testing.T) {
	t.Parallel()
	got := sanitizeParkerTagValue("")
	if got != "" {
		t.Errorf("want empty, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// parkerVMName
// ---------------------------------------------------------------------------

func TestParkerVMName(t *testing.T) {
	t.Parallel()
	got := parkerVMName(90000)
	if got != "bosh-parker-90000" {
		t.Errorf("want %q, got %q", "bosh-parker-90000", got)
	}
}

// ---------------------------------------------------------------------------
// buildParkerTags
// ---------------------------------------------------------------------------

func TestBuildParkerTags_NoDirectorID(t *testing.T) {
	t.Parallel()
	cfg := ParkerConfig{VMIDRangeStart: 90000, VMIDRangeEnd: 90999}
	got := buildParkerTags(cfg)
	if got != "bosh-parker" {
		t.Errorf("want %q, got %q", "bosh-parker", got)
	}
}

func TestBuildParkerTags_WithDirectorID(t *testing.T) {
	t.Parallel()
	cfg := ParkerConfig{VMIDRangeStart: 90000, VMIDRangeEnd: 90999, DirectorID: "my-director"}
	got := buildParkerTags(cfg)
	want := "bosh-parker;director--my-director"
	if got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestBuildParkerTags_DirectorIDWithSpecialChars(t *testing.T) {
	t.Parallel()
	// Special chars stripped by sanitizeParkerTagValue.
	cfg := ParkerConfig{VMIDRangeStart: 90000, VMIDRangeEnd: 90999, DirectorID: "my director!"}
	got := buildParkerTags(cfg)
	want := "bosh-parker;director--mydirector"
	if got != want {
		t.Errorf("want %q, got %q", want, got)
	}
}

func TestBuildParkerTags_DirectorIDBecomesEmpty(t *testing.T) {
	t.Parallel()
	// DirectorID of only special chars sanitizes to ""; director tag omitted.
	cfg := ParkerConfig{VMIDRangeStart: 90000, VMIDRangeEnd: 90999, DirectorID: "!!!"}
	got := buildParkerTags(cfg)
	if got != "bosh-parker" {
		t.Errorf("want %q (director tag omitted), got %q", "bosh-parker", got)
	}
}
