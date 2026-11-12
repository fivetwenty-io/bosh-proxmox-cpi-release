package allocationjournal

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestOtherEnrollmentsRecordedCountsOnlyOtherEnrolledNamespaces counts the
// enrolled namespaces beside the one named. The named namespace, a namespace
// directory with no enrollment file, a file, and a directory whose name is not
// a namespace hash, even one that holds an enrollment file, are all left out
// of the count.
func TestOtherEnrollmentsRecordedCountsOnlyOtherEnrolledNamespaces(t *testing.T) {
	_, dir := fixture(t)
	if got, err := OtherEnrollmentsRecorded(dir, "namespace"); err != nil || got != 0 {
		t.Fatalf("own enrollment only: got %d, %v, want 0", got, err)
	}
	if got, err := OtherEnrollmentsRecorded(dir, "renamed"); err != nil || got != 1 {
		t.Fatalf("one other enrollment: got %d, %v, want 1", got, err)
	}
	second, err := Initialize(context.Background(), dir, "second", enrollment())
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, pathKey("never-enrolled")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "endpoint-cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "endpoint-cache", authorityFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "audit-x.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := OtherEnrollmentsRecorded(dir, "renamed"); err != nil || got != 2 {
		t.Fatalf("two other enrollments: got %d, %v, want 2", got, err)
	}
	if got, err := OtherEnrollmentsRecorded(dir, "second"); err != nil || got != 1 {
		t.Fatalf("named namespace excluded: got %d, %v, want 1", got, err)
	}
	if _, err := OtherEnrollmentsRecorded("relative", "namespace"); err == nil {
		t.Fatal("a relative directory was counted")
	}
	if _, err := OtherEnrollmentsRecorded(dir, "../escape"); err == nil {
		t.Fatal("an invalid namespace was counted")
	}
	if _, err := OtherEnrollmentsRecorded(filepath.Join(dir, "absent"), "namespace"); !os.IsNotExist(err) {
		t.Fatalf("a missing directory gave %v, want a not-exist error", err)
	}
}
