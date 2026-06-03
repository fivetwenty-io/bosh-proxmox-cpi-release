package handlers

import (
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
)

// boolPtr returns a pointer to b. Helper for building DiskPerformanceDefaults literals.
func boolPtr(b bool) *bool { return &b }

// buildResolver builds a layeredResolver from a plain call cloud_properties map
// (no vm_type / disk_type selectors) and an empty CPIConfig (no profiles).
func buildResolver(t *testing.T, cp map[string]any) *layeredResolver {
	t.Helper()
	r, err := newLayeredResolver(cp, &config.CPIConfig{})
	if err != nil {
		t.Fatalf("newLayeredResolver: %v", err)
	}
	return r
}

// ----------------------------------------------------------------
// resolveDiskPerfOptions
// ----------------------------------------------------------------

func TestResolveDiskPerfOptions_EmptyResolverNilConfig(t *testing.T) {
	r := buildResolver(t, nil)
	opts, err := resolveDiskPerfOptions(r, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(opts) != 0 {
		t.Errorf("expected empty map, got %v", opts)
	}
}

func TestResolveDiskPerfOptions_EmptyResolverEmptyConfig(t *testing.T) {
	r := buildResolver(t, map[string]any{})
	opts, err := resolveDiskPerfOptions(r, &config.CPIConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(opts) != 0 {
		t.Errorf("expected empty map, got %v", opts)
	}
}

func TestResolveDiskPerfOptions_AllCallCloudProps(t *testing.T) {
	cp := map[string]any{
		"iothread": true,
		"cache":    "writeback",
		"discard":  true,
		"ssd":      true,
		"mbps_rd":  125.5,
		"iops_wr":  1000,
	}
	r := buildResolver(t, cp)
	opts, err := resolveDiskPerfOptions(r, &config.CPIConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := map[string]string{
		"iothread": "1",
		"cache":    "writeback",
		"discard":  "on",
		"ssd":      "1",
		"mbps_rd":  "125.5",
		"iops_wr":  "1000",
	}
	for k, wv := range want {
		if gv := opts[k]; gv != wv {
			t.Errorf("opts[%q]: want %q, got %q", k, wv, gv)
		}
	}
	if len(opts) != len(want) {
		t.Errorf("map length: want %d, got %d: %v", len(want), len(opts), opts)
	}
}

func TestResolveDiskPerfOptions_IothreadFalseOmitted(t *testing.T) {
	cp := map[string]any{"iothread": false}
	r := buildResolver(t, cp)
	opts, err := resolveDiskPerfOptions(r, &config.CPIConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, present := opts["iothread"]; present {
		t.Errorf("iothread=false should be omitted, got opts=%v", opts)
	}
}

func TestResolveDiskPerfOptions_GlobalConfigDefault(t *testing.T) {
	// Empty call cloud_properties; config Iothread=true should propagate.
	r := buildResolver(t, map[string]any{})
	cfg := &config.CPIConfig{
		DiskPerformance: &config.DiskPerformanceDefaults{
			Iothread: boolPtr(true),
		},
	}
	opts, err := resolveDiskPerfOptions(r, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v, ok := opts["iothread"]; !ok || v != "1" {
		t.Errorf("expected opts[iothread]=1 from global default, got opts=%v", opts)
	}
}

func TestResolveDiskPerfOptions_CallOverridesGlobal(t *testing.T) {
	// call iothread:false must beat config Iothread:true → key omitted.
	cp := map[string]any{"iothread": false}
	r := buildResolver(t, cp)
	cfg := &config.CPIConfig{
		DiskPerformance: &config.DiskPerformanceDefaults{
			Iothread: boolPtr(true),
		},
	}
	opts, err := resolveDiskPerfOptions(r, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, present := opts["iothread"]; present {
		t.Errorf("call iothread:false should beat config Iothread:true; got opts=%v", opts)
	}
}

func TestResolveDiskPerfOptions_CacheBogusError(t *testing.T) {
	cp := map[string]any{"cache": "bogus"}
	r := buildResolver(t, cp)
	_, err := resolveDiskPerfOptions(r, &config.CPIConfig{})
	if err == nil {
		t.Fatal("expected error for bogus cache mode, got nil")
	}
}

func TestResolveDiskPerfOptions_MbpsRdNegativeError(t *testing.T) {
	cp := map[string]any{"mbps_rd": -1.0}
	r := buildResolver(t, cp)
	_, err := resolveDiskPerfOptions(r, &config.CPIConfig{})
	if err == nil {
		t.Fatal("expected error for negative mbps_rd, got nil")
	}
}

func TestResolveDiskPerfOptions_IopsRdNegativeError(t *testing.T) {
	cp := map[string]any{"iops_rd": -1}
	r := buildResolver(t, cp)
	_, err := resolveDiskPerfOptions(r, &config.CPIConfig{})
	if err == nil {
		t.Fatal("expected error for negative iops_rd, got nil")
	}
}

func TestResolveDiskPerfOptions_MbpsWrZeroOmitted(t *testing.T) {
	cp := map[string]any{"mbps_wr": 0.0}
	r := buildResolver(t, cp)
	opts, err := resolveDiskPerfOptions(r, &config.CPIConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, present := opts["mbps_wr"]; present {
		t.Errorf("mbps_wr=0 should be omitted, got opts=%v", opts)
	}
}

func TestResolveDiskPerfOptions_IopsWrZeroOmitted(t *testing.T) {
	cp := map[string]any{"iops_wr": 0}
	r := buildResolver(t, cp)
	opts, err := resolveDiskPerfOptions(r, &config.CPIConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, present := opts["iops_wr"]; present {
		t.Errorf("iops_wr=0 should be omitted, got opts=%v", opts)
	}
}

// ----------------------------------------------------------------
// filterDiskPerfForBus
// ----------------------------------------------------------------

func TestFilterDiskPerfForBus_VirtioDropsSSd(t *testing.T) {
	input := map[string]string{"ssd": "1", "iothread": "1"}
	got := filterDiskPerfForBus(input, "virtio")
	if _, present := got["ssd"]; present {
		t.Error("virtio bus should drop ssd key")
	}
	if v := got["iothread"]; v != "1" {
		t.Errorf("virtio bus should keep iothread; got %v", got)
	}
}

func TestFilterDiskPerfForBus_ScsiKeepsAll(t *testing.T) {
	input := map[string]string{"ssd": "1", "iothread": "1"}
	got := filterDiskPerfForBus(input, "scsi")
	if len(got) != 2 {
		t.Errorf("scsi should keep all keys; got %v", got)
	}
}

func TestFilterDiskPerfForBus_SataKeepsAll(t *testing.T) {
	input := map[string]string{"ssd": "1", "cache": "writeback"}
	got := filterDiskPerfForBus(input, "sata")
	if len(got) != 2 {
		t.Errorf("sata should keep all keys; got %v", got)
	}
}

func TestFilterDiskPerfForBus_IdeKeepsAll(t *testing.T) {
	input := map[string]string{"ssd": "1", "cache": "writeback"}
	got := filterDiskPerfForBus(input, "ide")
	if len(got) != 2 {
		t.Errorf("ide should keep all keys; got %v", got)
	}
}

func TestFilterDiskPerfForBus_UnknownBusKeepsAll(t *testing.T) {
	input := map[string]string{"ssd": "1", "iothread": "1"}
	got := filterDiskPerfForBus(input, "nvme")
	if len(got) != 2 {
		t.Errorf("unknown bus should keep all keys; got %v", got)
	}
}

func TestFilterDiskPerfForBus_InputNotMutated(t *testing.T) {
	input := map[string]string{"ssd": "1", "iothread": "1"}
	_ = filterDiskPerfForBus(input, "virtio")
	if _, present := input["ssd"]; !present {
		t.Error("input map was mutated; ssd should still be present")
	}
}

func TestFilterDiskPerfForBus_NilInput(t *testing.T) {
	got := filterDiskPerfForBus(nil, "virtio")
	if got == nil || len(got) != 0 {
		t.Errorf("nil input should return empty map; got %v", got)
	}
}

func TestFilterDiskPerfForBus_EmptyInput(t *testing.T) {
	got := filterDiskPerfForBus(map[string]string{}, "scsi")
	if len(got) != 0 {
		t.Errorf("empty input should return empty map; got %v", got)
	}
}

// ----------------------------------------------------------------
// resolveVirtioSCSISingle
// ----------------------------------------------------------------

func TestResolveVirtioSCSISingle_CallTrue(t *testing.T) {
	cp := map[string]any{"virtio_scsi_single": true}
	r := buildResolver(t, cp)
	if !resolveVirtioSCSISingle(r, &config.CPIConfig{}) {
		t.Error("expected true from call cloud_properties")
	}
}

func TestResolveVirtioSCSISingle_ConfigTrueEmptyCall(t *testing.T) {
	r := buildResolver(t, map[string]any{})
	cfg := &config.CPIConfig{
		DiskPerformance: &config.DiskPerformanceDefaults{
			VirtioSCSISingle: boolPtr(true),
		},
	}
	if !resolveVirtioSCSISingle(r, cfg) {
		t.Error("expected true from config default")
	}
}

func TestResolveVirtioSCSISingle_NeitherSetFalse(t *testing.T) {
	r := buildResolver(t, map[string]any{})
	if resolveVirtioSCSISingle(r, &config.CPIConfig{}) {
		t.Error("expected false when neither call nor config sets virtio_scsi_single")
	}
}

func TestResolveVirtioSCSISingle_CallFalseBeatsConfigTrue(t *testing.T) {
	// call false must override config true.
	cp := map[string]any{"virtio_scsi_single": false}
	r := buildResolver(t, cp)
	cfg := &config.CPIConfig{
		DiskPerformance: &config.DiskPerformanceDefaults{
			VirtioSCSISingle: boolPtr(true),
		},
	}
	if resolveVirtioSCSISingle(r, cfg) {
		t.Error("call false should beat config true → expected false")
	}
}

// ----------------------------------------------------------------
// validateDiskPerfCache
// ----------------------------------------------------------------

func TestValidateDiskPerfCache_EmptyOK(t *testing.T) {
	if err := validateDiskPerfCache(""); err != nil {
		t.Errorf("empty mode should be valid, got %v", err)
	}
}

func TestValidateDiskPerfCache_ValidModes(t *testing.T) {
	for _, mode := range []string{"none", "writethrough", "writeback", "unsafe", "directsync"} {
		if err := validateDiskPerfCache(mode); err != nil {
			t.Errorf("mode %q should be valid, got %v", mode, err)
		}
	}
}

func TestValidateDiskPerfCache_BogusError(t *testing.T) {
	if err := validateDiskPerfCache("bogus"); err == nil {
		t.Error("bogus mode should return error, got nil")
	}
}
