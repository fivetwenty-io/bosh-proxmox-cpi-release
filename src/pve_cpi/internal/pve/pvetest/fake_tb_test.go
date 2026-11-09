package pvetest_test

import (
	"fmt"
	"sync/atomic"
	"testing"
)

// fakeTB records a failure and unwinds with a panic, so a test can assert that
// a helper fails the test it belongs to without failing the real test.
type fakeTB struct {
	testing.TB
	failed atomic.Bool
}

type fakeFatal struct{ msg string }

func (f *fakeTB) Fatalf(format string, args ...any) {
	f.failed.Store(true)
	panic(fakeFatal{msg: fmt.Sprintf(format, args...)})
}

func (f *fakeTB) Fatal(args ...any) {
	f.failed.Store(true)
	panic(fakeFatal{msg: fmt.Sprint(args...)})
}
