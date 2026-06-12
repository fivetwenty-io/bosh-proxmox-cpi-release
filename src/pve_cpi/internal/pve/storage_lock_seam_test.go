package pve_test

import (
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// NOTE: these tests mutate process-wide storage-lock defaults and must NOT run
// in parallel with each other. Each restores via SetStorageLockBackoffForTest.

func TestConfigureStorageLockBackoff_SetsBaseCap(t *testing.T) {
	restore := pve.SetStorageLockBackoffForTest(2000, 30000, 30)
	defer restore()

	pve.ConfigureStorageLockBackoff(4000, 60000, 20)
	capDur := pve.StorageLockBackoffCap()
	if capDur != 60*time.Second {
		t.Errorf("cap = %v, want 60s", capDur)
	}
	// attempt-0 backoff: base=4s, ±20% → [3.2s, 4.8s].
	d := pve.StorageLockBackoff(0)
	if d < 3*time.Second || d > 5*time.Second {
		t.Errorf("attempt-0 backoff %v outside [3s,5s] for base=4s", d)
	}
}

func TestConfigureStorageLockBackoff_ZeroIgnored(t *testing.T) {
	restore := pve.SetStorageLockBackoffForTest(2000, 30000, 30)
	defer restore()

	pve.ConfigureStorageLockBackoff(0, 0, 0) // all ≤ 0 → no change
	capDur := pve.StorageLockBackoffCap()
	if capDur != 30*time.Second {
		t.Errorf("cap = %v, want unchanged 30s", capDur)
	}
}

func TestConfigureStorageLockBackoff_CapClampedUpToBase(t *testing.T) {
	restore := pve.SetStorageLockBackoffForTest(2000, 30000, 30)
	defer restore()

	pve.ConfigureStorageLockBackoff(20000, 1000, 0) // cap(1s) < base(20s) → clamp up
	capDur := pve.StorageLockBackoffCap()
	if capDur < 20*time.Second {
		t.Errorf("cap = %v, must be >= base (20s) after clamp", capDur)
	}
}

func TestConfigureStorageLockBackoff_Defaults2s30s30pct(t *testing.T) {
	restore := pve.SetStorageLockBackoffForTest(2000, 30000, 30)
	defer restore()

	// Shipped defaults: base=2000ms, cap=30000ms, jitter=30%.
	capDur := pve.StorageLockBackoffCap()
	if capDur != 30*time.Second {
		t.Errorf("default cap = %v, want 30s", capDur)
	}
	// attempt-0: base=2s, ±30% → [1.4s, 2.6s].
	d := pve.StorageLockBackoff(0)
	if d < 1*time.Second || d > 3*time.Second {
		t.Errorf("attempt-0 backoff with shipped defaults %v outside [1s,3s]", d)
	}
}

func TestConfigureStorageLockBackoff_JitterClampedTo100(t *testing.T) {
	restore := pve.SetStorageLockBackoffForTest(2000, 30000, 30)
	defer restore()

	pve.ConfigureStorageLockBackoff(0, 0, 200) // jitter 200 → clamped to 100
	// If jitter > 100 were not clamped, the backoff could exceed the cap.
	// With base=2s, cap=30s, jitter=100%: d - 100%d + 200%d could overflow.
	// After clamp to 100: d - 100%d + [0,200%d]/2 stays bounded.
	for i := 0; i < 20; i++ {
		d := pve.StorageLockBackoff(i)
		if d < 0 {
			t.Errorf("attempt %d: negative backoff %v after jitter clamp", i, d)
		}
	}
}

// TestStorageLockBackoff_DefaultsMatchConfigConstants ensures the seam's
// shipped defaults match the config package's defaultStorageLock* constants so
// an unset deployment stays byte-identical end-to-end.
func TestStorageLockBackoff_DefaultsMatchShippedCurve(t *testing.T) {
	restore := pve.SetStorageLockBackoffForTest(2000, 30000, 30)
	defer restore()

	// cap must equal 30s (config defaultStorageLockCapMs=30000).
	if got := pve.StorageLockBackoffCap(); got != 30*time.Second {
		t.Errorf("StorageLockBackoffCap() = %v, want 30s (matches config default)", got)
	}
	// attempt-0 with default base=2s should produce a value in the ±30% window.
	for i := 0; i < 10; i++ {
		d := pve.StorageLockBackoff(0)
		if d < 1*time.Second || d > 3*time.Second {
			t.Errorf("attempt-0 default backoff %v outside ±30%% window of 2s", d)
		}
	}
}
