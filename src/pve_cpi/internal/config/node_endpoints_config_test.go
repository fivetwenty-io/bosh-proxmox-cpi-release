package config_test

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
)

func TestNodeEndpoints_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, `{
		"host": "pve1.example.com",
		"user": "root@pam",
		"password": "secret",
		"vm_storage": "local-lvm",
		"disk_storage": "local-lvm",
		"network_bridge": "vmbr0",
		"node_endpoints": {"pve2": "pve2.example.com", "pve3": "10.0.0.13:8006"}
	}`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.NodeEndpoints["pve2"] != "pve2.example.com" || cfg.NodeEndpoints["pve3"] != "10.0.0.13:8006" {
		t.Errorf("NodeEndpoints = %v; want both entries decoded", cfg.NodeEndpoints)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid node_endpoints rejected: %v", err)
	}
}

func TestNodeEndpoints_UnsetIsValid(t *testing.T) {
	t.Parallel()
	c := baseValidCfg()
	if err := c.Validate(); err != nil {
		t.Fatalf("base config invalid: %v", err)
	}
}

func TestValidateNodeEndpoints_RejectsBadValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		entries map[string]string
		wantSub string
	}{
		{"empty key", map[string]string{" ": "pve2.example.com"}, "node_endpoints keys must be non-empty"},
		{"empty value", map[string]string{"pve2": ""}, `node_endpoints["pve2"] must not be empty`},
		{"scheme", map[string]string{"pve2": "https://pve2.example.com"}, "without a scheme"},
		{"path", map[string]string{"pve2": "pve2.example.com/api2"}, "without a path"},
		{"bad port", map[string]string{"pve2": "pve2.example.com:99999"}, "port must be 1-65535"},
		{"non-numeric port", map[string]string{"pve2": "pve2.example.com:abc"}, "port must be 1-65535"},
		{"empty port", map[string]string{"pve2": "pve2.example.com:"}, "port must be 1-65535"},
		{"missing host", map[string]string{"pve2": ":8006"}, "must carry a host before the port"},
		{"padded key", map[string]string{" pve2": "pve2.example.com"}, "must not carry surrounding whitespace"},
		{"bracketed without port", map[string]string{"pve2": "[fd00::1]"}, "bracketed address must be [host]:port"},
		{"double port typo", map[string]string{"pve2": "pve2:8006:8006"}, "bare IPv6 literal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := baseValidCfg()
			c.NodeEndpoints = tc.entries
			err := c.Validate()
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestValidateNodeEndpoints_AcceptsGoodValues(t *testing.T) {
	t.Parallel()
	c := baseValidCfg()
	c.NodeEndpoints = map[string]string{
		"pve2": "pve2.example.com",
		"pve3": "10.0.0.13:8006",
		"pve4": "10.0.0.14",
		"pve6": "2001:db8::6",
		"pve7": "[2001:db8::7]:8006",
	}
	if err := c.Validate(); err != nil {
		t.Errorf("valid entries rejected: %v", err)
	}
}

func TestRetryStorageUploadMaxAttempts(t *testing.T) {
	t.Parallel()
	if got := (*config.CPIConfig)(nil).RetryStorageUploadMaxAttempts(); got != 0 {
		t.Errorf("nil receiver = %d; want 0 (caller default)", got)
	}
	c := baseValidCfg()
	if got := c.RetryStorageUploadMaxAttempts(); got != 0 {
		t.Errorf("unset block = %d; want 0 (caller default)", got)
	}
	c.Retry = &config.RetryConfig{StorageUpload: &config.RetryPolicy{MaxAttempts: 55}}
	if got := c.RetryStorageUploadMaxAttempts(); got != 55 {
		t.Errorf("override = %d; want 55", got)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("valid storage_upload block rejected: %v", err)
	}
	c.Retry = &config.RetryConfig{StorageUpload: &config.RetryPolicy{MaxAttempts: -1}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "retry.storage_upload.max_attempts must be >= 0") {
		t.Errorf("negative max_attempts: got %v; want a retry.storage_upload.max_attempts bound error", err)
	}
}
