// Package config internal tests for warnDetachedDiskStrategyFreeWithoutBand:
// the config-load Warn surfaced when an operator opts out of the parked default
// without keeping a parker band configured, which silently strands any disk
// parked while the default was in effect.
package config

import (
	"bytes"
	"strings"
	"testing"
)

func TestWarnFreeWithoutBand_FreeAndNoBand_Warns(t *testing.T) {
	var buf bytes.Buffer
	cfg := &CPIConfig{DetachedDiskStrategy: "free"}
	warnDetachedDiskStrategyFreeWithoutBand(cfg, &buf)
	out := buf.String()
	if out == "" {
		t.Fatal("strategy=free with no band must warn")
	}
	if !strings.Contains(out, "parked_disk_vmid_range") {
		t.Errorf("warning must name the property that fixes it, got: %s", out)
	}
	if !strings.Contains(out, "disk-audit") {
		t.Errorf("warning must point at the audit that proves whether disks are parked, got: %s", out)
	}
}

func TestWarnFreeWithoutBand_FreeWithBand_NoWarn(t *testing.T) {
	var buf bytes.Buffer
	cfg := &CPIConfig{
		DetachedDiskStrategy:     "free",
		ParkedDiskVMIDRangeStart: 90000,
		ParkedDiskVMIDRangeEnd:   90999,
	}
	warnDetachedDiskStrategyFreeWithoutBand(cfg, &buf)
	if buf.Len() > 0 {
		t.Errorf("an explicit band keeps the unpark probes running; no warning expected, got: %s", buf.String())
	}
}

func TestWarnFreeWithoutBand_DefaultStrategy_NoWarn(t *testing.T) {
	var buf bytes.Buffer
	cfg := &CPIConfig{}
	warnDetachedDiskStrategyFreeWithoutBand(cfg, &buf)
	if buf.Len() > 0 {
		t.Errorf("the parked default must not warn, got: %s", buf.String())
	}
}

// TestWarnStandDown_CarriesDrainAdvice pins the one message a stood-down load
// gets. The free-without-band warning is suppressed on that path to avoid
// saying the same thing twice, so this message has to carry the drain advice
// itself: a collision created AFTER parking has been running strands disks on
// parkers exactly the way an opt-out does, and the load cannot tell the two
// apart.
func TestWarnStandDown_CarriesDrainAdvice(t *testing.T) {
	var buf bytes.Buffer
	cfg := &CPIConfig{parkedDefaultBandCollision: "vmid_range [40000,200000]"}
	warnParkedDefaultBandCollision(cfg, &buf)
	out := buf.String()
	if out == "" {
		t.Fatal("a stand-down must be announced")
	}
	for _, want := range []string{"disk-audit", "parked_disk_vmid_range", "vmid_range [40000,200000]"} {
		if !strings.Contains(out, want) {
			t.Errorf("stand-down warning must mention %q, got: %s", want, out)
		}
	}
}

// TestWarnFreeWithoutBand_StandDown_Suppressed confirms the suppression, so the
// two messages never both fire for one load.
func TestWarnFreeWithoutBand_StandDown_Suppressed(t *testing.T) {
	var buf bytes.Buffer
	cfg := &CPIConfig{parkedDefaultBandCollision: "vmid_range [40000,200000]"}
	if cfg.DetachedDiskStrategyValue() != DetachedDiskStrategyFree {
		t.Fatal("precondition: a stood-down config must resolve to free")
	}
	warnDetachedDiskStrategyFreeWithoutBand(cfg, &buf)
	if buf.Len() > 0 {
		t.Errorf("the stand-down message covers this case; got a second warning: %s", buf.String())
	}
}
