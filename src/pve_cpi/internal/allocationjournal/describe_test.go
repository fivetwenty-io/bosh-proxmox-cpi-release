package allocationjournal

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"syscall"
	"testing"
)

// TestDescribeError renders the errors the journal owns the way every caller
// that shows them must. An unsafe path names itself. A durability failure
// says so and hands its wrapped error to the caller's renderer. A class keeps
// its fixed text and the fixed detail the journal wrote after it, and nothing
// from a wrapped cause. Anything else is left to the caller.
func TestDescribeError(t *testing.T) {
	cause := func(err error) string {
		if text, ok := DescribePathError(err); ok {
			return text
		}
		return "caller's description"
	}
	for _, tc := range []struct {
		name    string
		err     error
		want    string
		ok      bool
		exclude string
	}{
		{"an unsafe path", &UnsafePathError{Path: "/j", UID: 1000, EUID: 0, Cause: ErrUnsafeOwnership}, "journal: unsafe ownership: /j is owned by uid 1000, not effective uid 0", true, ""},
		{"a durability failure", &DurabilityError{Err: &fs.PathError{Op: "rename", Path: "/j/.tmp-1", Err: syscall.ENOSPC}}, "journal durability failure; reconcile before mutation (rename /j/.tmp-1: no space left on device)", true, ""},
		{"a durability failure the caller describes", &DurabilityError{Err: errors.New("secret")}, "journal durability failure; reconcile before mutation (caller's description)", true, "secret"},
		{"a class alone", ErrClosed, "journal allocation handle is closed", true, ""},
		{"a class with the journal's detail", fmt.Errorf("%w: malformed envelope: %w", ErrCorrupt, errors.New("near corrupt-secret")), "invalid journal evidence: malformed envelope", true, "secret"},
		{"a class a caller wrapped", fmt.Errorf("record secret-record: %w", ErrAuthority), "journal authority mismatch or recovery required", true, "secret"},
		{"a class inside a path error", &fs.PathError{Op: "open", Path: "/j/a.json", Err: ErrNotInitialized}, "journal authority is not initialized; historical provenance audit required", true, ""},
		{"a path error alone", &fs.PathError{Op: "open", Path: "/j/a.json", Err: syscall.EACCES}, "", false, ""},
		{"nothing the journal owns", errors.New("transport secret"), "", false, ""},
		{"nil", nil, "", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := DescribeError(tc.err, cause)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("DescribeError = %q, %t, want %q, %t", got, ok, tc.want, tc.ok)
			}
			if tc.exclude != "" && strings.Contains(got, tc.exclude) {
				t.Fatalf("description %q leaked %q", got, tc.exclude)
			}
		})
	}
}

// TestDescribePathError renders a filesystem path error as its operation, its
// path, and its errno. A cause that is not an errno can carry file content, so
// it renders only as a failed operation.
func TestDescribePathError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
		ok   bool
	}{
		{"an errno", &fs.PathError{Op: "rename", Path: "/j/allocation-a.json", Err: syscall.ENOSPC}, "rename /j/allocation-a.json: no space left on device", true},
		{"a wrapped path error", fmt.Errorf("settling: %w", &DurabilityError{Err: &fs.PathError{Op: "fsync", Path: "/j", Err: syscall.EIO}}), "fsync /j: input/output error", true},
		{"a cause that is not an errno", &fs.PathError{Op: "open", Path: "/j/x", Err: errors.New("secret transport text")}, "open /j/x failed", true},
		{"no path error", ErrClosed, "", false},
		{"nil", nil, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := DescribePathError(tc.err)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("DescribePathError = %q, %t, want %q, %t", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestErrorLead keeps the segment the journal wrote right after a lead, and
// only the lead once a caller has wrapped the error or the text goes on to a
// wrapped cause.
func TestErrorLead(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		lead string
		want string
	}{
		{"the journal's own detail", errors.New("journal: record exceeds size limit"), "journal", "journal: record exceeds size limit"},
		{"a cause after the detail", fmt.Errorf("journal: fingerprint: %w", errors.New("secret")), "journal", "journal: fingerprint"},
		{"a caller's wrap", fmt.Errorf("secret: %w", ErrConflict), ErrConflict.Error(), ErrConflict.Error()},
		{"a lead with nothing after it", ErrClosed, ErrClosed.Error(), ErrClosed.Error()},
		{"only the first line", errors.Join(errors.New("journal: directory changed while opening"), errors.New("secret")), "journal", "journal: directory changed while opening"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ErrorLead(tc.err, tc.lead); got != tc.want {
				t.Fatalf("ErrorLead = %q, want %q", got, tc.want)
			}
		})
	}
}
