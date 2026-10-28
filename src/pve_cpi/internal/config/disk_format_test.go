package config_test

import (
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

func TestIsAutoDiskFormat(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"", "  ", "auto", "AUTO", " Auto "} {
		if !config.IsAutoDiskFormat(v) {
			t.Errorf("IsAutoDiskFormat(%q) = false; want true", v)
		}
	}
	for _, v := range []string{"qcow2", "raw", "vmdk", "autox", "qed"} {
		if config.IsAutoDiskFormat(v) {
			t.Errorf("IsAutoDiskFormat(%q) = true; want false", v)
		}
	}
}

// An unset vm_disk_format stays unset after defaulting, "auto" is stored as
// unset, and an explicit format is kept.
func TestApplyDefaults_VMDiskFormat(t *testing.T) {
	t.Parallel()
	cases := map[string]string{"": "", "auto": "", " AUTO ": "", "raw": "raw", "qcow2": "qcow2", " vmdk ": "vmdk"}
	for in, want := range cases {
		cfg := validBaseCfg()
		cfg.VMDiskFormat = in
		cfg.ApplyDefaults()
		if cfg.VMDiskFormat != want {
			t.Errorf("ApplyDefaults(vm_disk_format=%q) = %q; want %q", in, cfg.VMDiskFormat, want)
		}
	}
}

func TestValidate_VMDiskFormat(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"", "auto", "Auto", "qcow2", "raw", "vmdk"} {
		cfg := validBaseCfg()
		cfg.VMDiskFormat = v
		cfg.ApplyDefaults()
		if err := cfg.Validate(); err != nil && strings.Contains(err.Error(), "vm_disk_format") {
			t.Errorf("vm_disk_format=%q rejected: %v", v, err)
		}
	}
	cfg := validBaseCfg()
	cfg.VMDiskFormat = "qed"
	cfg.ApplyDefaults()
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "vm_disk_format must be one of qcow2|raw|vmdk|auto or empty") {
		t.Errorf("vm_disk_format=qed: err = %v; want the enum error", err)
	}
}

// An empty or "auto" pve_vm_disk_format context override counts as unset, so
// the job-level value still decides. An explicit override replaces it.
func TestApplyContextOverrides_VMDiskFormat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, base string
		override   any
		want       string
	}{
		{"empty override keeps job raw", "raw", "", "raw"},
		{"auto override keeps job raw", "raw", "auto", "raw"},
		{"AUTO override keeps job unset", "", " AUTO ", ""},
		{"empty override keeps job unset", "", "", ""},
		{"raw override replaces job unset", "", "raw", "raw"},
		{"qcow2 override replaces job raw", "raw", "qcow2", "qcow2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := validBaseCfg()
			base.VMDiskFormat = tc.base
			base.ApplyDefaults()
			eff, _, unknown, err := config.ApplyContextOverrides(base, map[string]any{"pve_vm_disk_format": tc.override})
			if err != nil {
				t.Fatalf("ApplyContextOverrides: %v", err)
			}
			if len(unknown) != 0 {
				t.Fatalf("unknown keys = %v", unknown)
			}
			if eff.VMDiskFormat != tc.want {
				t.Errorf("effective vm_disk_format = %q; want %q", eff.VMDiskFormat, tc.want)
			}
		})
	}
}
