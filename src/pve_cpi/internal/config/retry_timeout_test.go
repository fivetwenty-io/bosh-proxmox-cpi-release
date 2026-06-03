package config_test

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
)

// boolPtrT is a local helper so these tests do not depend on test helpers
// elsewhere in the package.
func boolPtrT(b bool) *bool { return &b }

// --------------------------------------------------------------------------
// RetryConfig accessors: defaults preserve current behavior
// --------------------------------------------------------------------------

func TestRetryAccessors_NilBlock_ReturnsClassDefaults(t *testing.T) {
	c := &config.CPIConfig{} // no Retry block

	si := c.RetryStorageImport()
	if si.BaseMs != 2000 || si.CapMs != 30000 || si.JitterPct != 30 {
		t.Errorf("storage_import defaults = %+v, want base=2000 cap=30000 jitter=30", si)
	}
	if si.MaxAttempts != 0 {
		t.Errorf("storage_import MaxAttempts = %d, want 0 (caller default)", si.MaxAttempts)
	}

	vm := c.RetryVMIDAlloc()
	if vm.BaseMs != 50 || vm.CapMs != 250 {
		t.Errorf("vmid_alloc defaults = %+v, want base=50 cap=250", vm)
	}
	if vm.MaxAttempts != 0 {
		t.Errorf("vmid_alloc MaxAttempts = %d, want 0 (caller default)", vm.MaxAttempts)
	}

	tp := c.RetryTaskPoll()
	if tp.BaseMs != 2000 || tp.CapMs != 10000 || tp.JitterPct != 10 {
		t.Errorf("task_poll defaults = %+v, want base=2000 cap=10000 jitter=10", tp)
	}
}

func TestRetryAccessors_NilReceiver_DoesNotPanic(t *testing.T) {
	var c *config.CPIConfig
	_ = c.RetryStorageImport()
	_ = c.RetryVMIDAlloc()
	_ = c.RetryTaskPoll()
}

func TestRetryAccessors_PartialOverride_FillsOnlySetFields(t *testing.T) {
	c := &config.CPIConfig{
		Retry: &config.RetryConfig{
			StorageImport: &config.RetryPolicy{CapMs: 60000}, // only cap set
		},
	}
	si := c.RetryStorageImport()
	if si.BaseMs != 2000 {
		t.Errorf("BaseMs = %d, want default 2000 when unset", si.BaseMs)
	}
	if si.CapMs != 60000 {
		t.Errorf("CapMs = %d, want override 60000", si.CapMs)
	}
	if si.JitterPct != 30 {
		t.Errorf("JitterPct = %d, want default 30 when unset", si.JitterPct)
	}
}

func TestRetryAccessors_FullOverride(t *testing.T) {
	c := &config.CPIConfig{
		Retry: &config.RetryConfig{
			VMIDAlloc: &config.RetryPolicy{MaxAttempts: 7, BaseMs: 10, CapMs: 99},
		},
	}
	vm := c.RetryVMIDAlloc()
	if vm.MaxAttempts != 7 || vm.BaseMs != 10 || vm.CapMs != 99 {
		t.Errorf("vmid_alloc override = %+v, want attempts=7 base=10 cap=99", vm)
	}
}

func TestRetryTaskPoll_MaxAttemptsAlwaysZero(t *testing.T) {
	c := &config.CPIConfig{
		Retry: &config.RetryConfig{
			TaskPoll: &config.RetryPolicy{MaxAttempts: 99},
		},
	}
	if got := c.RetryTaskPoll().MaxAttempts; got != 0 {
		t.Errorf("task_poll MaxAttempts = %d, want 0 (not applicable to polling)", got)
	}
}

// --------------------------------------------------------------------------
// OperationTimeout accessors
// --------------------------------------------------------------------------

func TestOperationTimeoutEnabled(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.CPIConfig
		want bool
	}{
		{"nil receiver", nil, false},
		{"nil block", &config.CPIConfig{}, false},
		{"nil enabled", &config.CPIConfig{OperationTimeout: &config.OperationTimeoutConfig{}}, false},
		{"explicit false", &config.CPIConfig{OperationTimeout: &config.OperationTimeoutConfig{Enabled: boolPtrT(false)}}, false},
		{"explicit true", &config.CPIConfig{OperationTimeout: &config.OperationTimeoutConfig{Enabled: boolPtrT(true)}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.OperationTimeoutEnabled(); got != tc.want {
				t.Errorf("OperationTimeoutEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOperationTimeoutClassDefaults(t *testing.T) {
	c := &config.CPIConfig{OperationTimeout: &config.OperationTimeoutConfig{Enabled: boolPtrT(true)}}
	if got := c.OperationTimeoutCreateSec(); got != 1800 {
		t.Errorf("create default = %d, want 1800", got)
	}
	if got := c.OperationTimeoutDeleteSec(); got != 900 {
		t.Errorf("delete default = %d, want 900", got)
	}
	if got := c.OperationTimeoutQuerySec(); got != 120 {
		t.Errorf("query default = %d, want 120", got)
	}
	if got := c.OperationTimeoutDefaultSec(); got != 600 {
		t.Errorf("default-class default = %d, want 600", got)
	}
}

func TestOperationTimeoutOverrides(t *testing.T) {
	c := &config.CPIConfig{OperationTimeout: &config.OperationTimeoutConfig{
		Enabled:    boolPtrT(true),
		CreateSec:  60,
		DeleteSec:  30,
		QuerySec:   10,
		DefaultSec: 45,
	}}
	if c.OperationTimeoutCreateSec() != 60 || c.OperationTimeoutDeleteSec() != 30 ||
		c.OperationTimeoutQuerySec() != 10 || c.OperationTimeoutDefaultSec() != 45 {
		t.Errorf("overrides not honored: %d/%d/%d/%d",
			c.OperationTimeoutCreateSec(), c.OperationTimeoutDeleteSec(),
			c.OperationTimeoutQuerySec(), c.OperationTimeoutDefaultSec())
	}
}

// --------------------------------------------------------------------------
// Validation
// --------------------------------------------------------------------------

func baseValidCfg() *config.CPIConfig {
	c := &config.CPIConfig{
		Host:          "pve.example.com",
		User:          "root@pam",
		Password:      "secret",
		VMStorage:     "local-lvm",
		DiskStorage:   "local-lvm",
		NetworkBridge: "vmbr0",
	}
	c.ApplyDefaults() // fill enum/range defaults so the base config is valid
	return c
}

func TestValidateRetry_RejectsBadValues(t *testing.T) {
	cases := []struct {
		name    string
		retry   *config.RetryConfig
		wantSub string
	}{
		{"negative attempts", &config.RetryConfig{VMIDAlloc: &config.RetryPolicy{MaxAttempts: -1}}, "max_attempts must be >= 0"},
		{"negative base", &config.RetryConfig{StorageImport: &config.RetryPolicy{BaseMs: -5}}, "base_ms must be >= 0"},
		{"jitter over 100", &config.RetryConfig{TaskPoll: &config.RetryPolicy{JitterPct: 150}}, "jitter_pct must be 0-100"},
		{"cap below base", &config.RetryConfig{StorageImport: &config.RetryPolicy{BaseMs: 5000, CapMs: 1000}}, "effective cap_ms (1000) must be >= effective base_ms (5000)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := baseValidCfg()
			c.Retry = tc.retry
			err := c.Validate()
			if err == nil {
				t.Fatalf("expected validation error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestValidateRetry_AcceptsZeroAndValid(t *testing.T) {
	c := baseValidCfg()
	c.Retry = &config.RetryConfig{
		StorageImport: &config.RetryPolicy{}, // all zero = use defaults
		VMIDAlloc:     &config.RetryPolicy{MaxAttempts: 5, BaseMs: 10, CapMs: 200, JitterPct: 20},
		TaskPoll:      &config.RetryPolicy{BaseMs: 1000, CapMs: 1000}, // cap == base ok
	}
	if err := c.Validate(); err != nil {
		t.Errorf("expected valid config, got %v", err)
	}
}

func TestValidateRetry_EffectiveCapBelowDefaultBase(t *testing.T) {
	// Operator sets only cap_ms, leaving base_ms unset → base resolves to the
	// class default (2000 for storage_import). cap_ms 500 < effective base 2000
	// must be rejected even though the raw base field is 0.
	c := baseValidCfg()
	c.Retry = &config.RetryConfig{
		StorageImport: &config.RetryPolicy{CapMs: 500},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "effective cap_ms") {
		t.Fatalf("expected effective cap<base rejection, got %v", err)
	}
}

func TestValidateRetry_EffectiveCapOK_WhenBaseUnsetAndCapAboveDefault(t *testing.T) {
	// cap_ms 5000 >= effective base 2000 → valid.
	c := baseValidCfg()
	c.Retry = &config.RetryConfig{
		StorageImport: &config.RetryPolicy{CapMs: 5000},
	}
	if err := c.Validate(); err != nil {
		t.Errorf("expected valid, got %v", err)
	}
}

func TestValidateOperationTimeout_OnlyWhenEnabled(t *testing.T) {
	// Disabled block with a wild value must NOT fail validation.
	c := baseValidCfg()
	c.OperationTimeout = &config.OperationTimeoutConfig{CreateSec: -99}
	if err := c.Validate(); err != nil {
		t.Errorf("disabled timeout block should be ignored, got %v", err)
	}

	// Enabled with a negative value must fail.
	c2 := baseValidCfg()
	c2.OperationTimeout = &config.OperationTimeoutConfig{Enabled: boolPtrT(true), CreateSec: -1}
	err := c2.Validate()
	if err == nil || !strings.Contains(err.Error(), "operation_timeout.create_sec") {
		t.Errorf("expected create_sec range error, got %v", err)
	}
}
