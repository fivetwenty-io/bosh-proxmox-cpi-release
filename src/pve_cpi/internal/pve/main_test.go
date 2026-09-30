package pve

import (
	"os"
	"testing"
)

// TestMain turns off the grace pause that the parker and anti-affinity locks
// wait out after every create, so the tests that take those locks through
// fakes do not each sleep for it. The tests that exercise the pause set it
// themselves.
func TestMain(m *testing.M) {
	restoreGrace := SetClusterLockGraceForTest(0)
	code := m.Run()
	restoreGrace()
	os.Exit(code)
}
