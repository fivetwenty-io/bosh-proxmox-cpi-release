package allocationjournal

import (
	"errors"
	"io/fs"
	"strings"
	"syscall"
)

// errorClasses are the journal's own error classes, the sentinels its callers
// match with errors.Is.
var errorClasses = []error{
	ErrNotInitialized, ErrConflict, ErrReconciliationRequired,
	ErrAuthority, ErrCorrupt, ErrClosed,
}

// DescribeError renders err when it is an error the journal owns, and it
// reports false for anything else, which each caller then describes its own
// way. An unsafe path renders as its own text, which names only the path and
// the ownership or mode the journal checked. A durability failure says so and
// renders the error it wraps through cause, the caller's own renderer. A
// journal error class renders as its fixed text, followed by the fixed detail
// the journal put after it, so it never carries a wrapped cause. The package
// imports no other internal package, so it leaves every other error, and the
// scrubbing and length cap a printed description needs, to its callers.
func DescribeError(err error, cause func(error) string) (string, bool) {
	var unsafe *UnsafePathError
	if errors.As(err, &unsafe) {
		return unsafe.Error(), true
	}
	var durability *DurabilityError
	if errors.As(err, &durability) {
		return "journal durability failure; reconcile before mutation (" + cause(durability.Err) + ")", true
	}
	for _, class := range errorClasses {
		if errors.Is(err, class) {
			return ErrorLead(err, class.Error()), true
		}
	}
	return "", false
}

// DescribePathError renders the first filesystem path error in err's chain as
// its operation, its path, and its errno. A cause that is not an errno can
// carry file content, so such an error renders as its operation and path
// followed by "failed". It reports false when err holds no path error.
func DescribePathError(err error) (string, bool) {
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) {
		return "", false
	}
	var errno syscall.Errno
	if errors.As(pathErr.Err, &errno) {
		return pathErr.Op + " " + pathErr.Path + ": " + errno.Error(), true
	}
	return pathErr.Op + " " + pathErr.Path + " failed", true
}

// ErrorLead returns the fixed lead of a journal error: lead itself, followed
// by the next segment of the error's first line when the text starts with
// lead. The journal formats its errors as "<fixed>: <fixed detail>: <wrapped
// cause>", so the segment after lead is still text the journal wrote, while
// the wrapped cause may carry file content. When the text does not start with
// lead, a caller wrapped the error, and only lead is safe to show.
func ErrorLead(err error, lead string) string {
	text, _, _ := strings.Cut(err.Error(), "\n")
	rest, ok := strings.CutPrefix(text, lead)
	if !ok {
		return lead
	}
	detail, found := strings.CutPrefix(rest, ": ")
	if !found || detail == "" {
		return lead
	}
	detail, _, _ = strings.Cut(detail, ": ")
	return lead + ": " + detail
}
