package handlers_test

import (
	"os"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/cpi/handlers"
)

// TestMain zeroes the template-cache recheck delay for the whole test binary.
// Nearly every create_vm test misses the stemcell template cache by design
// (fake PVE, import path), and each miss otherwise waits out the real
// 750ms × 2 recheck budget — enough summed across the package to blow the
// 120s per-package CI timeout on small runners. The recheck behavior itself
// is attempt-count based, so the tests that exercise it are unaffected.
func TestMain(m *testing.M) {
	restore := handlers.SetTemplateCacheRecheckDelay(0)
	code := m.Run()
	restore()
	os.Exit(code)
}
