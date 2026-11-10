package pve

import (
	"fmt"
	"os"
	"testing"
)

// TestMain turns off the grace pause that the parker and anti-affinity locks
// wait out after every create, so the tests that take those locks through
// fakes do not each sleep for it. The tests that exercise the pause set it
// themselves.
//
// It also gives the test binary its own temporary directory. A client built
// from a token and one host chooses its endpoint cache under the temporary
// directory, so without this the tests would leave cache directories in the
// machine's.
func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "pve-tests-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "pve tests: make a temporary directory: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("TMPDIR", tmp); err != nil {
		fmt.Fprintf(os.Stderr, "pve tests: set TMPDIR: %v\n", err)
		os.Exit(1)
	}
	restoreGrace := SetClusterLockGraceForTest(0)
	code := m.Run()
	restoreGrace()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}
