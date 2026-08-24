package handlers

import (
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
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

// TestResolveDiskPerfOptions_EmptyResolverNilConfig_IothreadDefaultsOn
// verifies the default flip: with everything else absent, iothread
// resolves to "1" because its built-in Level-5 default is now true. This
// replaces the pre-Phase-2 "expected empty map" assertion.
func TestResolveDiskPerfOptions_EmptyResolverNilConfig_IothreadDefaultsOn(t *testing.T) {
	r := buildResolver(t, nil)
	opts, err := resolveDiskPerfOptions(r, nil, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(opts) != 1 || opts["iothread"] != "1" {
		t.Errorf("expected map[iothread:1] (the current default), got %v", opts)
	}
}

func TestResolveDiskPerfOptions_EmptyResolverEmptyConfig_IothreadDefaultsOn(t *testing.T) {
	r := buildResolver(t, map[string]any{})
	opts, err := resolveDiskPerfOptions(r, &config.CPIConfig{}, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(opts) != 1 || opts["iothread"] != "1" {
		t.Errorf("expected map[iothread:1] (the current default), got %v", opts)
	}
}

// TestResolveDiskPerfOptions_ExplicitIothreadFalse_TrulyEmpty verifies that an
// explicit call-level iothread:false still yields a fully empty map — the
// default flip does not remove the ability to opt all the way out.
func TestResolveDiskPerfOptions_ExplicitIothreadFalse_TrulyEmpty(t *testing.T) {
	r := buildResolver(t, map[string]any{"iothread": false})
	opts, err := resolveDiskPerfOptions(r, nil, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(opts) != 0 {
		t.Errorf("expected empty map with explicit iothread:false, got %v", opts)
	}
}

// TestResolveDiskPerfOptions_ExplicitIothreadFalse_GlobalConfig verifies a
// global config Iothread=false also fully disables the default.
func TestResolveDiskPerfOptions_ExplicitIothreadFalse_GlobalConfig(t *testing.T) {
	r := buildResolver(t, map[string]any{})
	cfg := &config.CPIConfig{DiskPerformance: &config.DiskPerformanceDefaults{Iothread: boolPtr(false)}}
	opts, err := resolveDiskPerfOptions(r, cfg, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(opts) != 0 {
		t.Errorf("expected empty map with global Iothread=false, got %v", opts)
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
	opts, err := resolveDiskPerfOptions(r, &config.CPIConfig{}, "", "")
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
	opts, err := resolveDiskPerfOptions(r, &config.CPIConfig{}, "", "")
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
	opts, err := resolveDiskPerfOptions(r, cfg, "", "")
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
	opts, err := resolveDiskPerfOptions(r, cfg, "", "")
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
	_, err := resolveDiskPerfOptions(r, &config.CPIConfig{}, "", "")
	if err == nil {
		t.Fatal("expected error for bogus cache mode, got nil")
	}
}

func TestResolveDiskPerfOptions_MbpsRdNegativeError(t *testing.T) {
	cp := map[string]any{"mbps_rd": -1.0}
	r := buildResolver(t, cp)
	_, err := resolveDiskPerfOptions(r, &config.CPIConfig{}, "", "")
	if err == nil {
		t.Fatal("expected error for negative mbps_rd, got nil")
	}
}

func TestResolveDiskPerfOptions_IopsRdNegativeError(t *testing.T) {
	cp := map[string]any{"iops_rd": -1}
	r := buildResolver(t, cp)
	_, err := resolveDiskPerfOptions(r, &config.CPIConfig{}, "", "")
	if err == nil {
		t.Fatal("expected error for negative iops_rd, got nil")
	}
}

func TestResolveDiskPerfOptions_MbpsWrZeroOmitted(t *testing.T) {
	cp := map[string]any{"mbps_wr": 0.0}
	r := buildResolver(t, cp)
	opts, err := resolveDiskPerfOptions(r, &config.CPIConfig{}, "", "")
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
	opts, err := resolveDiskPerfOptions(r, &config.CPIConfig{}, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, present := opts["iops_wr"]; present {
		t.Errorf("iops_wr=0 should be omitted, got opts=%v", opts)
	}
}

// ----------------------------------------------------------------
// resolveDiskPerfOptions: discard/ssd auto-resolution (TRIM capability)
// ----------------------------------------------------------------

// TestResolveDiskPerfOptions_Auto_TrimCapableStorage_BothOn verifies that
// with discard/ssd left unset everywhere, a TRIM-capable storage type (e.g.
// lvmthin) auto-resolves both to on.
func TestResolveDiskPerfOptions_Auto_TrimCapableStorage_BothOn(t *testing.T) {
	t.Parallel()
	r := buildResolver(t, map[string]any{})
	opts, err := resolveDiskPerfOptions(r, &config.CPIConfig{}, "lvmthin", diskFormatRaw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts["discard"] != "on" {
		t.Errorf("discard = %q; want on (lvmthin is TRIM-capable)", opts["discard"])
	}
	if opts["ssd"] != "1" {
		t.Errorf("ssd = %q; want 1 (lvmthin is TRIM-capable)", opts["ssd"])
	}
}

// TestResolveDiskPerfOptions_Auto_QCOW2OnFileBacked_BothOn verifies the
// second TRIM-capable shape: a file-backed pool (dir/nfs/cifs) with a qcow2
// disk image.
func TestResolveDiskPerfOptions_Auto_QCOW2OnFileBacked_BothOn(t *testing.T) {
	t.Parallel()
	r := buildResolver(t, map[string]any{})
	opts, err := resolveDiskPerfOptions(r, &config.CPIConfig{}, "nfs", "qcow2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts["discard"] != "on" {
		t.Errorf("discard = %q; want on (qcow2 on nfs is TRIM-capable)", opts["discard"])
	}
	if opts["ssd"] != "1" {
		t.Errorf("ssd = %q; want 1 (qcow2 on nfs is TRIM-capable)", opts["ssd"])
	}
}

// TestResolveDiskPerfOptions_Auto_NonTrimStorage_NothingBaked verifies that
// discard/ssd stay omitted on backends where TRIM does not reclaim space:
// thick lvm, and raw format on a file-backed pool.
func TestResolveDiskPerfOptions_Auto_NonTrimStorage_NothingBaked(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		storageType string
		format      string
	}{
		{"thick_lvm", "lvm", diskFormatRaw},
		{"raw_on_file_backed", "dir", diskFormatRaw},
		{"cephfs", "cephfs", "qcow2"},
		{"unknown_backend", "made-up", "qcow2"},
		{"unresolved_type", "", "qcow2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := buildResolver(t, map[string]any{})
			opts, err := resolveDiskPerfOptions(r, &config.CPIConfig{}, tc.storageType, tc.format)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, present := opts["discard"]; present {
				t.Errorf("discard must be omitted for storageType=%q format=%q, got opts=%v", tc.storageType, tc.format, opts)
			}
			if _, present := opts["ssd"]; present {
				t.Errorf("ssd must be omitted for storageType=%q format=%q, got opts=%v", tc.storageType, tc.format, opts)
			}
		})
	}
}

// TestResolveDiskPerfOptions_Auto_CallExplicitTrue_OverridesNonTrim verifies
// that an explicit call-level discard:true/ssd:true forces the options on
// even on a non-TRIM-capable backend (PVE itself is left to accept or reject
// the value — the CPI does not second-guess an explicit operator choice).
func TestResolveDiskPerfOptions_Auto_CallExplicitTrue_OverridesNonTrim(t *testing.T) {
	t.Parallel()
	r := buildResolver(t, map[string]any{"discard": true, "ssd": true})
	opts, err := resolveDiskPerfOptions(r, &config.CPIConfig{}, "lvm", diskFormatRaw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts["discard"] != "on" {
		t.Errorf("discard = %q; want on (explicit true forces it on thick lvm)", opts["discard"])
	}
	if opts["ssd"] != "1" {
		t.Errorf("ssd = %q; want 1 (explicit true forces it on thick lvm)", opts["ssd"])
	}
}

// TestResolveDiskPerfOptions_Auto_CallExplicitFalse_OverridesTrimCapable
// verifies that an explicit call-level discard:false/ssd:false suppresses
// both even on a TRIM-capable backend.
func TestResolveDiskPerfOptions_Auto_CallExplicitFalse_OverridesTrimCapable(t *testing.T) {
	t.Parallel()
	r := buildResolver(t, map[string]any{"discard": false, "ssd": false})
	opts, err := resolveDiskPerfOptions(r, &config.CPIConfig{}, "lvmthin", diskFormatRaw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, present := opts["discard"]; present {
		t.Errorf("discard must be omitted with explicit false, got opts=%v", opts)
	}
	if _, present := opts["ssd"]; present {
		t.Errorf("ssd must be omitted with explicit false, got opts=%v", opts)
	}
}

// TestResolveDiskPerfOptions_Auto_GlobalConfigExplicit_OverridesAuto verifies
// that the global config layer's explicit discard/ssd values also win over
// the TRIM-capability auto default, both directions.
func TestResolveDiskPerfOptions_Auto_GlobalConfigExplicit_OverridesAuto(t *testing.T) {
	t.Parallel()

	t.Run("config_false_beats_trim_capable", func(t *testing.T) {
		t.Parallel()
		r := buildResolver(t, map[string]any{})
		cfg := &config.CPIConfig{DiskPerformance: &config.DiskPerformanceDefaults{
			Discard: boolPtr(false), SSD: boolPtr(false),
		}}
		opts, err := resolveDiskPerfOptions(r, cfg, "zfspool", diskFormatRaw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// iothread is not under test here and defaults on independently;
		// only discard/ssd must be absent.
		if _, present := opts["discard"]; present {
			t.Errorf("discard must be absent with explicit global false, got opts=%v", opts)
		}
		if _, present := opts["ssd"]; present {
			t.Errorf("ssd must be absent with explicit global false, got opts=%v", opts)
		}
	})

	t.Run("config_true_beats_non_trim", func(t *testing.T) {
		t.Parallel()
		r := buildResolver(t, map[string]any{})
		cfg := &config.CPIConfig{DiskPerformance: &config.DiskPerformanceDefaults{
			Discard: boolPtr(true), SSD: boolPtr(true),
		}}
		opts, err := resolveDiskPerfOptions(r, cfg, "lvm", diskFormatRaw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if opts["discard"] != "on" || opts["ssd"] != "1" {
			t.Errorf("expected discard=on, ssd=1 with explicit global true, got opts=%v", opts)
		}
	})

	t.Run("call_beats_config", func(t *testing.T) {
		t.Parallel()
		// call discard:true must win over config Discard:false, even on a
		// non-TRIM backend — call layer is always highest precedence.
		r := buildResolver(t, map[string]any{"discard": true})
		cfg := &config.CPIConfig{DiskPerformance: &config.DiskPerformanceDefaults{Discard: boolPtr(false)}}
		opts, err := resolveDiskPerfOptions(r, cfg, "lvm", diskFormatRaw)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if opts["discard"] != "on" {
			t.Errorf("discard = %q; want on (call layer beats config layer)", opts["discard"])
		}
	})
}

// ----------------------------------------------------------------
// needsDiskPerfStorageTypeLookup
// ----------------------------------------------------------------

func TestNeedsDiskPerfStorageTypeLookup_NeitherSet_True(t *testing.T) {
	t.Parallel()
	r := buildResolver(t, map[string]any{})
	if !needsDiskPerfStorageTypeLookup(r, &config.CPIConfig{}) {
		t.Error("expected true: discard and ssd both unset, auto-resolution needs the lookup")
	}
}

func TestNeedsDiskPerfStorageTypeLookup_BothExplicitAtCall_False(t *testing.T) {
	t.Parallel()
	r := buildResolver(t, map[string]any{"discard": false, "ssd": true})
	if needsDiskPerfStorageTypeLookup(r, &config.CPIConfig{}) {
		t.Error("expected false: both discard and ssd explicit at the call layer, no lookup needed")
	}
}

func TestNeedsDiskPerfStorageTypeLookup_BothExplicitAtConfig_False(t *testing.T) {
	t.Parallel()
	r := buildResolver(t, map[string]any{})
	cfg := &config.CPIConfig{DiskPerformance: &config.DiskPerformanceDefaults{
		Discard: boolPtr(true), SSD: boolPtr(false),
	}}
	if needsDiskPerfStorageTypeLookup(r, cfg) {
		t.Error("expected false: both discard and ssd explicit at the config layer, no lookup needed")
	}
}

func TestNeedsDiskPerfStorageTypeLookup_OnlyOneExplicit_True(t *testing.T) {
	t.Parallel()
	// discard explicit, ssd left unset — the lookup is still needed for ssd's
	// own auto-resolution.
	r := buildResolver(t, map[string]any{"discard": true})
	if !needsDiskPerfStorageTypeLookup(r, &config.CPIConfig{}) {
		t.Error("expected true: ssd is still unset and needs auto-resolution")
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

// TestResolveVirtioSCSISingle_NeitherSetDefaultsTrue verifies the current
// default flip: with neither the layered resolver nor global config setting
// virtio_scsi_single, the built-in Level-5 default is now true.
func TestResolveVirtioSCSISingle_NeitherSetDefaultsTrue(t *testing.T) {
	r := buildResolver(t, map[string]any{})
	if !resolveVirtioSCSISingle(r, &config.CPIConfig{}) {
		t.Error("expected true (the current default) when neither call nor config sets virtio_scsi_single")
	}
}

// TestResolveVirtioSCSISingle_CallFalse_OptsOut verifies an explicit
// call-level false still fully disables the default.
func TestResolveVirtioSCSISingle_CallFalse_OptsOut(t *testing.T) {
	cp := map[string]any{"virtio_scsi_single": false}
	r := buildResolver(t, cp)
	if resolveVirtioSCSISingle(r, &config.CPIConfig{}) {
		t.Error("expected false with explicit call-level virtio_scsi_single:false")
	}
}

// TestResolveVirtioSCSISingle_ConfigFalse_OptsOut verifies an explicit
// global config false still fully disables the default.
func TestResolveVirtioSCSISingle_ConfigFalse_OptsOut(t *testing.T) {
	r := buildResolver(t, map[string]any{})
	cfg := &config.CPIConfig{DiskPerformance: &config.DiskPerformanceDefaults{VirtioSCSISingle: boolPtr(false)}}
	if resolveVirtioSCSISingle(r, cfg) {
		t.Error("expected false with explicit global config virtio_scsi_single:false")
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

// ----------------------------------------------------------------
// validateDiskPerfAio
// ----------------------------------------------------------------

func TestValidateDiskPerfAio_EmptyOK(t *testing.T) {
	if err := validateDiskPerfAio(""); err != nil {
		t.Errorf("empty mode should be valid, got %v", err)
	}
}

func TestValidateDiskPerfAio_ValidModes(t *testing.T) {
	for _, mode := range []string{"native", "io_uring", "threads"} {
		if err := validateDiskPerfAio(mode); err != nil {
			t.Errorf("mode %q should be valid, got %v", mode, err)
		}
	}
}

func TestValidateDiskPerfAio_BogusError(t *testing.T) {
	if err := validateDiskPerfAio("bogus"); err == nil {
		t.Error("bogus mode should return error, got nil")
	}
}

// ----------------------------------------------------------------
// resolveDiskPerfOptions: aio
// ----------------------------------------------------------------

// TestResolveDiskPerfOptions_Aio_UnsetOmitted verifies that with aio unset at
// every layer, opts carries no "aio" key — byte-identical to before this
// option existed (only the pre-existing iothread default-true entry remains).
func TestResolveDiskPerfOptions_Aio_UnsetOmitted(t *testing.T) {
	r := buildResolver(t, nil)
	opts, err := resolveDiskPerfOptions(r, nil, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, present := opts["aio"]; present {
		t.Errorf("aio should be omitted when unset at every layer, got opts=%v", opts)
	}
	if len(opts) != 1 || opts["iothread"] != "1" {
		t.Errorf("expected map[iothread:1] only, got %v", opts)
	}
}

// TestResolveDiskPerfOptions_Aio_GlobalConfig_SetsKey verifies the global
// disk_performance.aio default is baked into opts when nothing overrides it
// at the call layer.
func TestResolveDiskPerfOptions_Aio_GlobalConfig_SetsKey(t *testing.T) {
	r := buildResolver(t, nil)
	cfg := &config.CPIConfig{
		DiskPerformance: &config.DiskPerformanceDefaults{AIO: "native"},
	}
	opts, err := resolveDiskPerfOptions(r, cfg, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts["aio"] != "native" {
		t.Errorf("aio = %q; want %q", opts["aio"], "native")
	}
}

// TestResolveDiskPerfOptions_Aio_CallOverridesGlobal verifies call-level
// cloud_properties.aio wins over the global disk_performance.aio default,
// matching the layered resolver's precedence for every other disk_performance
// option.
func TestResolveDiskPerfOptions_Aio_CallOverridesGlobal(t *testing.T) {
	cp := map[string]any{"aio": "threads"}
	r := buildResolver(t, cp)
	cfg := &config.CPIConfig{
		DiskPerformance: &config.DiskPerformanceDefaults{AIO: "native"},
	}
	opts, err := resolveDiskPerfOptions(r, cfg, "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts["aio"] != "threads" {
		t.Errorf("call aio=threads should beat config aio=native; got opts[aio]=%q", opts["aio"])
	}
}

// TestResolveDiskPerfOptions_Aio_BogusError verifies an invalid aio value at
// the call layer produces a non-retriable error rather than being silently
// baked into the volid.
func TestResolveDiskPerfOptions_Aio_BogusError(t *testing.T) {
	cp := map[string]any{"aio": "bogus"}
	r := buildResolver(t, cp)
	_, err := resolveDiskPerfOptions(r, &config.CPIConfig{}, "", "")
	if err == nil {
		t.Fatal("expected error for bogus aio mode, got nil")
	}
}

// TestResolveDiskPerfOptions_Aio_BogusGlobalConfigError verifies an invalid
// aio value at the global config layer also produces an error (not just a
// call-level bogus value).
func TestResolveDiskPerfOptions_Aio_BogusGlobalConfigError(t *testing.T) {
	r := buildResolver(t, nil)
	cfg := &config.CPIConfig{
		DiskPerformance: &config.DiskPerformanceDefaults{AIO: "bogus"},
	}
	_, err := resolveDiskPerfOptions(r, cfg, "", "")
	if err == nil {
		t.Fatal("expected error for bogus global aio mode, got nil")
	}
}
