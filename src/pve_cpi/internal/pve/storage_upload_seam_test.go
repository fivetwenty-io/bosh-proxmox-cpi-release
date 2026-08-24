package pve_test

import (
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
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
	restore := pve.SetStorageUploadRetryForTest(3)
	if got := pve.StorageUploadMaxAttempts(); got != 3 {
		t.Errorf("override: %d, want 3", got)
	}
	restore()
	if got := pve.StorageUploadMaxAttempts(); got == 3 {
		t.Error("restore did not clear the test override")
	}
}
