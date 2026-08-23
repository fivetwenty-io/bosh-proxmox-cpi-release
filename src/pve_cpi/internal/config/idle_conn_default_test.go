package config_test

import (
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
)

// TestApplyDefaults_PVEIdleConnTimeout verifies the shipped idle keep-alive
// window: an unset pve_api_idle_conn_timeout_sec defaults to 15 seconds
// instead of falling through to the SDK's 90-second default, which sits far
// past pveproxy's keep-alive window and makes the client routinely reuse
// connections the server has already closed. An explicit operator value
// passes through untouched.
func TestApplyDefaults_PVEIdleConnTimeout(t *testing.T) {
	t.Parallel()

	var cfg config.CPIConfig
	cfg.VMStorage = "vm-store"
	cfg.ApplyDefaults()

	if cfg.PVEIdleConnTimeoutSec != 15 {
		t.Errorf("PVEIdleConnTimeoutSec = %d, want 15 when unset", cfg.PVEIdleConnTimeoutSec)
	}

	var explicit config.CPIConfig
	explicit.VMStorage = "vm-store"
	explicit.PVEIdleConnTimeoutSec = 45
	explicit.ApplyDefaults()

	if explicit.PVEIdleConnTimeoutSec != 45 {
		t.Errorf("PVEIdleConnTimeoutSec = %d, want the explicit 45 preserved", explicit.PVEIdleConnTimeoutSec)
	}
}
