package handlers_test

import (
	"os"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
)

// TestMain zeroes the template-cache recheck delay for the whole test binary.
// Nearly every create_vm test misses the stemcell template cache by design
// (fake PVE, import path), and each miss otherwise waits out the real
// 750ms × 2 recheck budget — enough summed across the package to blow the
// 120s per-package CI timeout on small runners. The recheck behavior itself
// is attempt-count based, so the tests that exercise it are unaffected.
//
// It also turns off the allocation journal's fsyncs, which dominate the
// journal-backed tests here. The journal's own tests keep them on.
func TestMain(m *testing.M) {
	restore := handlers.SetTemplateCacheRecheckDelay(0)
	restoreSync := aj.SetFileSyncForTest(false)
	code := m.Run()
	restoreSync()
	restore()
	os.Exit(code)
}
