package handlers_test

import (
	"os"
	"testing"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// TestMain zeroes the template-cache recheck delay for the whole test binary.
// Nearly every create_vm test misses the stemcell template cache by design
// (fake PVE, import path), and each miss otherwise waits out the real
// 750ms × 2 recheck budget, which adds minutes to the package's run and eats
// into the 900s per-package timeout that make test sets. The recheck
// behavior itself is attempt-count based, so the tests that exercise it are
// unaffected.
//
// It also turns off the allocation journal's fsyncs, which dominate the
// journal-backed tests here. The journal's own tests keep them on.
func TestMain(m *testing.M) {
	restore := handlers.SetTemplateCacheRecheckDelay(0)
	restoreSync := aj.SetFileSyncForTest(false)
	// The parker and anti-affinity locks pause after every create. The tests
	// that exercise the pause set it themselves.
	restoreGrace := pve.SetClusterLockGraceForTest(0)
	// A managed volume's presence read waits between the reads of a listing
	// that failed. The tests that exercise the wait set it themselves.
	restoreListing := handlers.SetManagedVolumePresenceRetryDelay(0)
	code := m.Run()
	restoreListing()
	restoreGrace()
	restoreSync()
	restore()
	os.Exit(code)
}
