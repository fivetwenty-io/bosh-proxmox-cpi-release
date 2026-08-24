package config_test

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// TestDiskMigrationValue covers the effective resolution of
// pve.disk_migration: empty resolves to "on_attach" (the default — a
// stranded disk moves instead of erroring), "off" opts out, and a nil
// receiver resolves to "off" so the migration path stays inert with no
// configuration loaded at all.
func TestDiskMigrationValue(t *testing.T) {
	t.Parallel()

	if got := (*config.CPIConfig)(nil).DiskMigrationValue(); got != config.DiskMigrationOff {
		t.Errorf("nil receiver = %q, want off", got)
	}
	if (*config.CPIConfig)(nil).DiskMigrationOnAttachEnabled() {
		t.Error("nil receiver must not enable migration")
	}

	cases := []struct {
		raw  string
		want string
	}{
		{"", config.DiskMigrationOnAttach},
		{"on_attach", config.DiskMigrationOnAttach},
		{" ON_ATTACH ", config.DiskMigrationOnAttach},
		{"off", config.DiskMigrationOff},
		{" OFF ", config.DiskMigrationOff},
	}
	for _, tc := range cases {
		c := &config.CPIConfig{DiskMigration: tc.raw}
		if got := c.DiskMigrationValue(); got != tc.want {
			t.Errorf("DiskMigrationValue(%q) = %q, want %q", tc.raw, got, tc.want)
		}
		if got := c.DiskMigrationOnAttachEnabled(); got != (tc.want == config.DiskMigrationOnAttach) {
			t.Errorf("DiskMigrationOnAttachEnabled(%q) = %v", tc.raw, got)
		}
	}
}

// TestValidate_DiskMigrationEnum: valid values load, anything else is
// rejected at validation time naming the field and the valid set.
func TestValidate_DiskMigrationEnum(t *testing.T) {
	t.Parallel()

	for _, v := range []string{"", "on_attach", "off", "ON_ATTACH"} {
		c := baseValidCfg()
		c.DiskMigration = v
		if err := c.Validate(); err != nil {
			t.Errorf("Validate with disk_migration=%q: %v", v, err)
		}
	}

	c := baseValidCfg()
	c.DiskMigration = "sometimes"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "disk_migration must be one of on_attach|off") {
		t.Fatalf("err = %v, want the enum rejection", err)
	}
}

// TestRetryDiskMigrate covers the resolved disk-migrate policy: unset keeps
// MaxAttempts 0 (callers fall back to pve.DefaultDiskMigrateMaxAttempts) and
// the 30-minute await budget; operator overrides win field-wise; negative
// values are rejected by validateRetry.
func TestRetryDiskMigrate(t *testing.T) {
	t.Parallel()

	const defaultCapMs = 1800000

	if got := (*config.CPIConfig)(nil).RetryDiskMigrate(); got.MaxAttempts != 0 || got.CapMs != defaultCapMs {
		t.Errorf("nil receiver = %+v, want MaxAttempts 0 and CapMs %d", got, defaultCapMs)
	}
	if got := baseValidCfg().RetryDiskMigrate(); got.MaxAttempts != 0 || got.CapMs != defaultCapMs {
		t.Errorf("unset block = %+v, want MaxAttempts 0 and CapMs %d", got, defaultCapMs)
	}

	c := baseValidCfg()
	c.Retry = &config.RetryConfig{DiskMigrate: &config.RetryPolicy{MaxAttempts: 6, CapMs: 60000}}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := c.RetryDiskMigrate(); got.MaxAttempts != 6 || got.CapMs != 60000 {
		t.Errorf("overridden = %+v, want MaxAttempts 6 and CapMs 60000", got)
	}

	c = baseValidCfg()
	c.Retry = &config.RetryConfig{DiskMigrate: &config.RetryPolicy{MaxAttempts: -1}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "retry.disk_migrate.max_attempts must be >= 0") {
		t.Fatalf("err = %v, want the negative-attempts rejection", err)
	}
}

// TestApplyContextOverrides_DiskMigration: the per-cpi-config key applies,
// and an invalid value fails the effective config's validation rather than
// silently running with the job-level setting.
func TestApplyContextOverrides_DiskMigration(t *testing.T) {
	t.Parallel()

	base := baseValidCfg()
	eff, applied, unknown, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_disk_migration": "off",
	})
	if err != nil {
		t.Fatalf("ApplyContextOverrides: %v", err)
	}
	if len(unknown) != 0 || len(applied) != 1 || applied[0] != "pve_disk_migration" {
		t.Fatalf("applied=%v unknown=%v, want exactly pve_disk_migration applied", applied, unknown)
	}
	if eff.DiskMigrationValue() != config.DiskMigrationOff {
		t.Errorf("effective value = %q, want off", eff.DiskMigrationValue())
	}
	if base.DiskMigrationValue() != config.DiskMigrationOnAttach {
		t.Errorf("base mutated: %q", base.DiskMigrationValue())
	}

	_, _, _, err = config.ApplyContextOverrides(base, map[string]any{
		"pve_disk_migration": "sometimes",
	})
	if err == nil || !strings.Contains(err.Error(), "disk_migration must be one of on_attach|off") {
		t.Fatalf("err = %v, want the enum rejection through effective validation", err)
	}
}
