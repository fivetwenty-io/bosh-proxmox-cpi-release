// Package config internal tests for warnParkedDefaultBandCollision: the
// config-load Warn surfaced when the defaulted parked strategy stands down
// because its built-in band collides with another VMID band. The band itself
// stays in force read-only, so the message no longer needs to carry drain
// advice — previously parked disks keep unparking through the holder scans.
package config

import (
	"bytes"
	"strings"
	"testing"
)

func TestWarnStandDown_AnnouncesCollisionAndRemedy(t *testing.T) {
	var buf bytes.Buffer
	cfg := &CPIConfig{parkedDefaultBandCollision: "vmid_range [40000,200000]"}
	warnParkedDefaultBandCollision(cfg, &buf)
	out := buf.String()
	if out == "" {
		t.Fatal("a stand-down must be announced")
	}
	for _, want := range []string{"parked_disk_vmid_range", "vmid_range [40000,200000]", "read-only", "unparked"} {
		if !strings.Contains(out, want) {
			t.Errorf("stand-down warning must mention %q, got: %s", want, out)
		}
	}
}

func TestWarnStandDown_NoCollision_Silent(t *testing.T) {
	var buf bytes.Buffer
	cfg := &CPIConfig{}
	warnParkedDefaultBandCollision(cfg, &buf)
	if buf.Len() > 0 {
		t.Errorf("no collision must mean no warning, got: %s", buf.String())
	}
}

// TestFreeWithoutBand_BandStillFills pins the unified band semantics: an
// explicit opt-out with no band typed still resolves the built-in parker band
// after defaulting, so the holder scans keep recognizing and unparking disks
// parked while the "parked" default was in effect. This replaces the removed
// free-without-band load warning — the state it warned about (free with a
// [0,0] band) no longer exists on a loaded config.
func TestFreeWithoutBand_BandStillFills(t *testing.T) {
	cfg := &CPIConfig{DetachedDiskStrategy: "free"}
	cfg.ApplyDefaults()
	if cfg.ParkedDiskVMIDRangeStart != defaultParkerVMIDStart || cfg.ParkedDiskVMIDRangeEnd != defaultParkerVMIDEnd {
		t.Fatalf("strategy=free with no band must fill the built-in band, got [%d,%d]",
			cfg.ParkedDiskVMIDRangeStart, cfg.ParkedDiskVMIDRangeEnd)
	}
	if !cfg.ParkedStrategyActive() {
		t.Fatal("the filled band must keep the parker read paths active")
	}
	if cfg.DetachedDiskParkedEnabled() {
		t.Fatal("filling the band must not turn parking back on")
	}
}

// TestStandDown_BandStillFills pins the same rule for the collision stand-down:
// the strategy default stands down, the band does not.
func TestStandDown_BandStillFills(t *testing.T) {
	cfg := &CPIConfig{VMIDRangeStart: 40000, VMIDRangeEnd: 200000}
	cfg.ApplyDefaults()
	if cfg.ParkedDefaultStoodDown() == "" {
		t.Fatal("precondition: the widened vmid_range must stand the parked default down")
	}
	if cfg.DetachedDiskStrategyValue() != DetachedDiskStrategyFree {
		t.Fatal("precondition: a stood-down config must resolve to free")
	}
	if cfg.ParkedDiskVMIDRangeStart != defaultParkerVMIDStart || cfg.ParkedDiskVMIDRangeEnd != defaultParkerVMIDEnd {
		t.Fatalf("a stood-down load must still fill the built-in band, got [%d,%d]",
			cfg.ParkedDiskVMIDRangeStart, cfg.ParkedDiskVMIDRangeEnd)
	}
}
