package pve_test

import (
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func TestStorageUploadMaxAttempts_Default(t *testing.T) {
	restore := pve.SetStorageUploadRetryForTest(0)
	defer restore()
	if got := pve.StorageUploadMaxAttempts(); got != pve.DefaultStorageUploadMaxAttempts {
		t.Errorf("StorageUploadMaxAttempts = %d, want default %d", got, pve.DefaultStorageUploadMaxAttempts)
	}
}

func TestConfigureStorageUploadRetry_Override(t *testing.T) {
	restore := pve.SetStorageUploadRetryForTest(0)
	defer restore()

	pve.ConfigureStorageUploadRetry(0) // no-op: zero keeps the default
	if got := pve.StorageUploadMaxAttempts(); got != pve.DefaultStorageUploadMaxAttempts {
		t.Errorf("after Configure(0): %d, want default %d", got, pve.DefaultStorageUploadMaxAttempts)
	}

	pve.ConfigureStorageUploadRetry(42)
	if got := pve.StorageUploadMaxAttempts(); got != 42 {
		t.Errorf("after Configure(42): %d, want 42", got)
	}
}

func TestSetStorageUploadRetryForTest_Restores(t *testing.T) {
	// Pin a known prior value so the restore assertion does not depend on
	// what earlier tests left in the process-global seam.
	defer pve.SetStorageUploadRetryForTest(0)()

	before := pve.StorageUploadMaxAttempts()
	restore := pve.SetStorageUploadRetryForTest(before + 1)
	if got := pve.StorageUploadMaxAttempts(); got != before+1 {
		t.Errorf("override: %d, want %d", got, before+1)
	}
	restore()
	if got := pve.StorageUploadMaxAttempts(); got != before {
		t.Errorf("after restore: %d, want the prior value %d", got, before)
	}
}
