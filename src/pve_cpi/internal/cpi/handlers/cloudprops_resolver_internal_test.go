package handlers

import (
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
)

// ---------------------------------------------------------------------------
// newLayeredResolver construction
// ---------------------------------------------------------------------------

func TestNewLayeredResolver_NilCallCP(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	r, err := newLayeredResolver(nil, cfg)
	if err != nil {
		t.Fatalf("nil callCP: unexpected error: %v", err)
	}
	if r == nil {
		t.Fatal("resolver must not be nil")
	}
	// No profiles resolved; call layer is empty map.
	if r.hasLayers() {
		t.Error("hasLayers() should be false when no vm_type or disk_type profiles exist")
	}
}

func TestNewLayeredResolver_EmptyCallCP(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	r, err := newLayeredResolver(map[string]any{}, cfg)
	if err != nil {
		t.Fatalf("empty callCP: unexpected error: %v", err)
	}
	if r == nil {
		t.Fatal("resolver must not be nil")
	}
}

func TestNewLayeredResolver_VMTypeAndDiskTypeBothResolve(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMTypes: map[string]config.TypeProfile{
			"medium": {CloudProperties: map[string]any{"cpus": 2, "ram": 2048}},
		},
		DiskTypes: map[string]config.TypeProfile{
			"fast": {CloudProperties: map[string]any{"storage_pool": "ceph-rbd"}},
		},
	}
	callCP := map[string]any{
		"vm_type":   "medium",
		"disk_type": "fast",
		"node":      "pve1",
	}
	r, err := newLayeredResolver(callCP, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.hasLayers() {
		t.Error("hasLayers() should be true when both profiles resolved")
	}
	// call layer wins for "node"
	node, ok := r.String("node")
	if !ok || node != "pve1" {
		t.Errorf("String(node) = %q,%v; want pve1,true", node, ok)
	}
	// disk_type layer provides storage_pool
	pool, ok := r.String("storage_pool")
	if !ok || pool != "ceph-rbd" {
		t.Errorf("String(storage_pool) = %q,%v; want ceph-rbd,true", pool, ok)
	}
	// vm_type layer provides cpus
	cpus, ok := r.Int("cpus")
	if !ok || cpus != 2 {
		t.Errorf("Int(cpus) = %d,%v; want 2,true", cpus, ok)
	}
}

func TestNewLayeredResolver_UnknownVMType_Error(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMTypes: map[string]config.TypeProfile{
			"medium": {CloudProperties: map[string]any{}},
		},
	}
	callCP := map[string]any{"vm_type": "xlarge"}
	_, err := newLayeredResolver(callCP, cfg)
	if err == nil {
		t.Fatal("expected error for unknown vm_type, got nil")
	}
}

func TestNewLayeredResolver_UnknownDiskType_Error(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		DiskTypes: map[string]config.TypeProfile{
			"fast": {CloudProperties: map[string]any{}},
		},
	}
	callCP := map[string]any{"disk_type": "ultra"}
	_, err := newLayeredResolver(callCP, cfg)
	if err == nil {
		t.Fatal("expected error for unknown disk_type, got nil")
	}
}

func TestNewLayeredResolver_NonStringSelectorVMType_Error(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMTypes: map[string]config.TypeProfile{
			"medium": {CloudProperties: map[string]any{}},
		},
	}
	callCP := map[string]any{"vm_type": 42}
	_, err := newLayeredResolver(callCP, cfg)
	if err == nil {
		t.Fatal("expected error for non-string vm_type selector, got nil")
	}
}

func TestNewLayeredResolver_NonStringSelectorDiskType_Error(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		DiskTypes: map[string]config.TypeProfile{
			"fast": {CloudProperties: map[string]any{}},
		},
	}
	callCP := map[string]any{"disk_type": true}
	_, err := newLayeredResolver(callCP, cfg)
	if err == nil {
		t.Fatal("expected error for non-string disk_type selector, got nil")
	}
}

func TestNewLayeredResolver_ProfileWithNilCloudProperties(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMTypes: map[string]config.TypeProfile{
			"bare": {CloudProperties: nil},
		},
	}
	callCP := map[string]any{"vm_type": "bare"}
	r, err := newLayeredResolver(callCP, cfg)
	if err != nil {
		t.Fatalf("nil CloudProperties profile: unexpected error: %v", err)
	}
	// Profile with nil CloudProperties still counts as "resolved" (profile exists),
	// but lookup into it returns not-found.
	_, ok := r.String("any_key")
	if ok {
		t.Error("nil CloudProperties profile should not supply any values")
	}
}

// ---------------------------------------------------------------------------
// Precedence: call > disk_type > vm_type > not-found
// ---------------------------------------------------------------------------

func TestLayeredResolver_PrecedenceCallBeatesDiskType(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		DiskTypes: map[string]config.TypeProfile{
			"fast": {CloudProperties: map[string]any{"storage_pool": "ceph-rbd"}},
		},
	}
	callCP := map[string]any{
		"disk_type":    "fast",
		"storage_pool": "nvme-local",
	}
	r, _ := newLayeredResolver(callCP, cfg)
	pool, ok := r.String("storage_pool")
	if !ok || pool != "nvme-local" {
		t.Errorf("String(storage_pool) = %q,%v; want nvme-local,true (call beats disk_type)", pool, ok)
	}
}

func TestLayeredResolver_PrecedenceDiskTypeBeatsVMType(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMTypes: map[string]config.TypeProfile{
			"med": {CloudProperties: map[string]any{"storage_pool": "vm-default"}},
		},
		DiskTypes: map[string]config.TypeProfile{
			"fast": {CloudProperties: map[string]any{"storage_pool": "ceph-rbd"}},
		},
	}
	callCP := map[string]any{
		"vm_type":   "med",
		"disk_type": "fast",
	}
	r, _ := newLayeredResolver(callCP, cfg)
	pool, ok := r.String("storage_pool")
	if !ok || pool != "ceph-rbd" {
		t.Errorf("String(storage_pool) = %q,%v; want ceph-rbd,true (disk_type beats vm_type)", pool, ok)
	}
}

func TestLayeredResolver_PrecedenceVMTypeFallback(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMTypes: map[string]config.TypeProfile{
			"med": {CloudProperties: map[string]any{"cpus": 4}},
		},
	}
	callCP := map[string]any{"vm_type": "med"}
	r, _ := newLayeredResolver(callCP, cfg)
	cpus, ok := r.Int("cpus")
	if !ok || cpus != 4 {
		t.Errorf("Int(cpus) = %d,%v; want 4,true (vm_type fallback)", cpus, ok)
	}
}

func TestLayeredResolver_PrecedenceNotFound(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	r, _ := newLayeredResolver(map[string]any{}, cfg)
	_, ok := r.String("nonexistent")
	if ok {
		t.Error("String(nonexistent) should return false when key absent from all layers")
	}
}

// ---------------------------------------------------------------------------
// Alias (multi-key) within-layer semantics
// ---------------------------------------------------------------------------

func TestLayeredResolver_AliasWithinLayerStoragePoolBeatsStorage(t *testing.T) {
	t.Parallel()

	// Both keys present in call layer; storage_pool must win within the layer.
	cfg := &config.CPIConfig{}
	callCP := map[string]any{
		"storage_pool": "ssd-pool",
		"storage":      "hdd-pool",
	}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.String("storage_pool", "storage")
	if !ok || got != "ssd-pool" {
		t.Errorf("String(storage_pool,storage) = %q,%v; want ssd-pool,true (first key wins in layer)", got, ok)
	}
}

func TestLayeredResolver_AliasInLowerLayerUsedWhenHigherLacksBoth(t *testing.T) {
	t.Parallel()

	// Call has neither key; vm_type profile has the second alias key only.
	cfg := &config.CPIConfig{
		VMTypes: map[string]config.TypeProfile{
			"med": {CloudProperties: map[string]any{"storage": "vm-pool"}},
		},
	}
	callCP := map[string]any{"vm_type": "med"}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.String("storage_pool", "storage")
	if !ok || got != "vm-pool" {
		t.Errorf("String(storage_pool,storage) = %q,%v; want vm-pool,true (alias in lower layer)", got, ok)
	}
}

func TestLayeredResolver_AliasCallLayerFirstKeyPresentWinsOverLowerLayerSecondKey(t *testing.T) {
	t.Parallel()

	// Call has second alias "storage"; vm_type has first alias "storage_pool".
	// Call layer has "storage" but not "storage_pool".
	// Rule: within call layer try "storage_pool" (absent) then "storage" (present) → "call-store".
	// vm_type layer has "storage_pool" = "vm-pool" — should NOT be returned.
	cfg := &config.CPIConfig{
		VMTypes: map[string]config.TypeProfile{
			"med": {CloudProperties: map[string]any{"storage_pool": "vm-pool"}},
		},
	}
	callCP := map[string]any{
		"vm_type": "med",
		"storage": "call-store",
	}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.String("storage_pool", "storage")
	if !ok || got != "call-store" {
		t.Errorf("String(storage_pool,storage) = %q,%v; want call-store,true", got, ok)
	}
}

// ---------------------------------------------------------------------------
// String
// ---------------------------------------------------------------------------

func TestLayeredResolver_String_WhitespaceOnlySkipped(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{
		VMTypes: map[string]config.TypeProfile{
			"med": {CloudProperties: map[string]any{"node": "pve2"}},
		},
	}
	callCP := map[string]any{
		"vm_type": "med",
		"node":    "   ",
	}
	r, _ := newLayeredResolver(callCP, cfg)
	// Call layer has whitespace-only "node" → skip → vm_type layer has "pve2".
	got, ok := r.String("node")
	if !ok || got != "pve2" {
		t.Errorf("String(node) = %q,%v; want pve2,true (whitespace skipped)", got, ok)
	}
}

func TestLayeredResolver_String_Trims(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	callCP := map[string]any{"node": "  pve3  "}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.String("node")
	if !ok || got != "pve3" {
		t.Errorf("String(node) = %q,%v; want pve3,true (trimmed)", got, ok)
	}
}

func TestLayeredResolver_String_NonStringSkipped(t *testing.T) {
	t.Parallel()

	// int value in call layer must not be returned by String; vm_type fallback used.
	cfg := &config.CPIConfig{
		VMTypes: map[string]config.TypeProfile{
			"med": {CloudProperties: map[string]any{"node": "pve4"}},
		},
	}
	callCP := map[string]any{
		"vm_type": "med",
		"node":    42,
	}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.String("node")
	if !ok || got != "pve4" {
		t.Errorf("String(node) = %q,%v; want pve4,true (non-string skipped)", got, ok)
	}
}

// ---------------------------------------------------------------------------
// Int
// ---------------------------------------------------------------------------

func TestLayeredResolver_Int_Float64JSONNumber(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	// JSON unmarshals numbers as float64 by default.
	callCP := map[string]any{"cpus": float64(8)}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.Int("cpus")
	if !ok || got != 8 {
		t.Errorf("Int(cpus) = %d,%v; want 8,true (float64 coercion)", got, ok)
	}
}

func TestLayeredResolver_Int_IntNative(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	callCP := map[string]any{"cpus": int(4)}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.Int("cpus")
	if !ok || got != 4 {
		t.Errorf("Int(cpus) = %d,%v; want 4,true", got, ok)
	}
}

func TestLayeredResolver_Int_Int64(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	callCP := map[string]any{"cpus": int64(16)}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.Int("cpus")
	if !ok || got != 16 {
		t.Errorf("Int(cpus) = %d,%v; want 16,true", got, ok)
	}
}

func TestLayeredResolver_Int_NumericString(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	callCP := map[string]any{"cpus": "2"}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.Int("cpus")
	if !ok || got != 2 {
		t.Errorf("Int(cpus) = %d,%v; want 2,true (numeric string)", got, ok)
	}
}

func TestLayeredResolver_Int_Absent(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	r, _ := newLayeredResolver(map[string]any{}, cfg)
	got, ok := r.Int("cpus")
	if ok || got != 0 {
		t.Errorf("Int(cpus absent) = %d,%v; want 0,false", got, ok)
	}
}

func TestLayeredResolver_Int_NonNumericString(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	callCP := map[string]any{"cpus": "not-a-number"}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.Int("cpus")
	if ok || got != 0 {
		t.Errorf("Int(cpus non-numeric) = %d,%v; want 0,false", got, ok)
	}
}

func TestLayeredResolver_Int_ZeroIsFound(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	callCP := map[string]any{"cpus": float64(0)}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.Int("cpus")
	if !ok || got != 0 {
		t.Errorf("Int(cpus=0) = %d,%v; want 0,true (zero is a valid value)", got, ok)
	}
}

// ---------------------------------------------------------------------------
// Bool
// ---------------------------------------------------------------------------

func TestLayeredResolver_Bool_ExplicitFalse(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	callCP := map[string]any{"firewall": false}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.Bool("firewall")
	if !ok || got != false {
		t.Errorf("Bool(firewall=false) = %v,%v; want false,true (explicit false is found)", got, ok)
	}
}

func TestLayeredResolver_Bool_ExplicitTrue(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	callCP := map[string]any{"firewall": true}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.Bool("firewall")
	if !ok || got != true {
		t.Errorf("Bool(firewall=true) = %v,%v; want true,true", got, ok)
	}
}

func TestLayeredResolver_Bool_Float64One(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	callCP := map[string]any{"firewall": float64(1)}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.Bool("firewall")
	if !ok || got != true {
		t.Errorf("Bool(firewall=1.0) = %v,%v; want true,true", got, ok)
	}
}

func TestLayeredResolver_Bool_Float64Zero(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	callCP := map[string]any{"firewall": float64(0)}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.Bool("firewall")
	if !ok || got != false {
		t.Errorf("Bool(firewall=0.0) = %v,%v; want false,true", got, ok)
	}
}

func TestLayeredResolver_Bool_IntOne(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	callCP := map[string]any{"firewall": int(1)}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.Bool("firewall")
	if !ok || got != true {
		t.Errorf("Bool(firewall=int(1)) = %v,%v; want true,true", got, ok)
	}
}

func TestLayeredResolver_Bool_IntZero(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	callCP := map[string]any{"firewall": int(0)}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.Bool("firewall")
	if !ok || got != false {
		t.Errorf("Bool(firewall=int(0)) = %v,%v; want false,true", got, ok)
	}
}

func TestLayeredResolver_Bool_StringTrue(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"true", "TRUE", "True", "1"} {
		s := s
		t.Run(s, func(t *testing.T) {
			t.Parallel()
			cfg := &config.CPIConfig{}
			callCP := map[string]any{"firewall": s}
			r, _ := newLayeredResolver(callCP, cfg)
			got, ok := r.Bool("firewall")
			if !ok || got != true {
				t.Errorf("Bool(firewall=%q) = %v,%v; want true,true", s, got, ok)
			}
		})
	}
}

func TestLayeredResolver_Bool_StringFalse(t *testing.T) {
	t.Parallel()

	for _, s := range []string{"false", "FALSE", "False", "0"} {
		s := s
		t.Run(s, func(t *testing.T) {
			t.Parallel()
			cfg := &config.CPIConfig{}
			callCP := map[string]any{"firewall": s}
			r, _ := newLayeredResolver(callCP, cfg)
			got, ok := r.Bool("firewall")
			if !ok || got != false {
				t.Errorf("Bool(firewall=%q) = %v,%v; want false,true", s, got, ok)
			}
		})
	}
}

func TestLayeredResolver_Bool_Absent(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	r, _ := newLayeredResolver(map[string]any{}, cfg)
	got, ok := r.Bool("firewall")
	if ok || got != false {
		t.Errorf("Bool(absent) = %v,%v; want false,false", got, ok)
	}
}

func TestLayeredResolver_Bool_UnparsableString(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	callCP := map[string]any{"firewall": "yes"}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.Bool("firewall")
	if ok || got != false {
		t.Errorf("Bool(firewall=yes) = %v,%v; want false,false (unparseable)", got, ok)
	}
}

// ---------------------------------------------------------------------------
// StringSlice
// ---------------------------------------------------------------------------

func TestLayeredResolver_StringSlice_SliceAny(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	callCP := map[string]any{"security_groups": []any{"group-a", "group-b"}}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.StringSlice("security_groups")
	if !ok {
		t.Fatal("StringSlice(security_groups): expected true, got false")
	}
	if len(got) != 2 || got[0] != "group-a" || got[1] != "group-b" {
		t.Errorf("StringSlice = %v; want [group-a group-b]", got)
	}
}

func TestLayeredResolver_StringSlice_StringSlice(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	callCP := map[string]any{"security_groups": []string{"group-x"}}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.StringSlice("security_groups")
	if !ok {
		t.Fatal("StringSlice: expected true, got false")
	}
	if len(got) != 1 || got[0] != "group-x" {
		t.Errorf("StringSlice = %v; want [group-x]", got)
	}
}

func TestLayeredResolver_StringSlice_SingleString(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	callCP := map[string]any{"security_groups": "only-group"}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.StringSlice("security_groups")
	if !ok {
		t.Fatal("StringSlice single string: expected true, got false")
	}
	if len(got) != 1 || got[0] != "only-group" {
		t.Errorf("StringSlice = %v; want [only-group]", got)
	}
}

func TestLayeredResolver_StringSlice_EmptiesFiltered(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	// Mix of valid, empty, and whitespace-only entries.
	callCP := map[string]any{"security_groups": []any{"grp-a", "", "  ", "grp-b"}}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.StringSlice("security_groups")
	if !ok {
		t.Fatal("StringSlice with empties: expected true, got false")
	}
	if len(got) != 2 || got[0] != "grp-a" || got[1] != "grp-b" {
		t.Errorf("StringSlice = %v; want [grp-a grp-b] (empties filtered)", got)
	}
}

func TestLayeredResolver_StringSlice_AllEmptiesAfterFilter_NotFound(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	callCP := map[string]any{"security_groups": []any{"", "  "}}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.StringSlice("security_groups")
	if ok || got != nil {
		t.Errorf("StringSlice all-empty: got %v,%v; want nil,false", got, ok)
	}
}

func TestLayeredResolver_StringSlice_NonStringInSliceSkipped(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	callCP := map[string]any{"security_groups": []any{"grp-a", 42, true, "grp-b"}}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.StringSlice("security_groups")
	if !ok {
		t.Fatal("StringSlice non-strings: expected true, got false")
	}
	if len(got) != 2 || got[0] != "grp-a" || got[1] != "grp-b" {
		t.Errorf("StringSlice = %v; want [grp-a grp-b]", got)
	}
}

func TestLayeredResolver_StringSlice_Absent(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	r, _ := newLayeredResolver(map[string]any{}, cfg)
	got, ok := r.StringSlice("security_groups")
	if ok || got != nil {
		t.Errorf("StringSlice absent: got %v,%v; want nil,false", got, ok)
	}
}

func TestLayeredResolver_StringSlice_WhitespaceSingleStringNotFound(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	callCP := map[string]any{"security_groups": "   "}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.StringSlice("security_groups")
	if ok || got != nil {
		t.Errorf("StringSlice whitespace single: got %v,%v; want nil,false", got, ok)
	}
}

// ---------------------------------------------------------------------------
// No-op / call-only layer behaves like single-map lookup
// ---------------------------------------------------------------------------

func TestLayeredResolver_NoSelectors_CallOnlyLikeSingleMapLookup(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	callCP := map[string]any{
		"node":         "pve5",
		"storage_pool": "local-zfs",
		"cpus":         float64(2),
		"firewall":     true,
	}
	r, err := newLayeredResolver(callCP, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if node, ok := r.String("node"); !ok || node != "pve5" {
		t.Errorf("String(node) = %q,%v; want pve5,true", node, ok)
	}
	if pool, ok := r.String("storage_pool"); !ok || pool != "local-zfs" {
		t.Errorf("String(storage_pool) = %q,%v; want local-zfs,true", pool, ok)
	}
	if cpus, ok := r.Int("cpus"); !ok || cpus != 2 {
		t.Errorf("Int(cpus) = %d,%v; want 2,true", cpus, ok)
	}
	if fw, ok := r.Bool("firewall"); !ok || fw != true {
		t.Errorf("Bool(firewall) = %v,%v; want true,true", fw, ok)
	}
	if _, ok := r.String("absent_key"); ok {
		t.Error("String(absent_key) should return false")
	}
	if r.hasLayers() {
		t.Error("hasLayers() should be false when no profile layers appended")
	}
}

// ---------------------------------------------------------------------------
// Float
// ---------------------------------------------------------------------------

func TestLayeredResolver_Float_Float64(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	callCP := map[string]any{"weight": float64(1.5)}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.Float("weight")
	if !ok || got != 1.5 {
		t.Errorf("Float(weight) = %v,%v; want 1.5,true", got, ok)
	}
}

func TestLayeredResolver_Float_IntToFloat(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	callCP := map[string]any{"weight": int(2)}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.Float("weight")
	if !ok || got != 2.0 {
		t.Errorf("Float(weight) = %v,%v; want 2.0,true (int coercion)", got, ok)
	}
}

func TestLayeredResolver_Float_Int64ToFloat(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	callCP := map[string]any{"weight": int64(3)}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.Float("weight")
	if !ok || got != 3.0 {
		t.Errorf("Float(weight) = %v,%v; want 3.0,true (int64 coercion)", got, ok)
	}
}

func TestLayeredResolver_Float_NumericString(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	callCP := map[string]any{"weight": "0.75"}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.Float("weight")
	if !ok || got != 0.75 {
		t.Errorf("Float(weight) = %v,%v; want 0.75,true (numeric string)", got, ok)
	}
}

func TestLayeredResolver_Float_JSONNumber(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	callCP := map[string]any{"weight": json.Number("2.5")}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.Float("weight")
	if !ok || got != 2.5 {
		t.Errorf("Float(weight) = %v,%v; want 2.5,true (json.Number)", got, ok)
	}
}

func TestLayeredResolver_Float_Absent(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	r, _ := newLayeredResolver(map[string]any{}, cfg)
	got, ok := r.Float("weight")
	if ok || got != 0 {
		t.Errorf("Float(absent) = %v,%v; want 0,false", got, ok)
	}
}

func TestLayeredResolver_Float_NonNumericString(t *testing.T) {
	t.Parallel()

	cfg := &config.CPIConfig{}
	callCP := map[string]any{"weight": "heavy"}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.Float("weight")
	if ok || got != 0 {
		t.Errorf("Float(non-numeric string) = %v,%v; want 0,false", got, ok)
	}
}

func TestLayeredResolver_Float_PrecedenceAcrossLayers(t *testing.T) {
	t.Parallel()

	// Call layer has the key; vm_type profile also has the key.
	// Call layer must win.
	cfg := &config.CPIConfig{
		VMTypes: map[string]config.TypeProfile{
			"med": {CloudProperties: map[string]any{"weight": float64(0.5)}},
		},
	}
	callCP := map[string]any{
		"vm_type": "med",
		"weight":  float64(1.0),
	}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.Float("weight")
	if !ok || got != 1.0 {
		t.Errorf("Float(weight) = %v,%v; want 1.0,true (call layer beats vm_type)", got, ok)
	}
}

func TestLayeredResolver_Float_VMTypeProfileFallback(t *testing.T) {
	t.Parallel()

	// Call layer lacks the key; vm_type profile provides it.
	cfg := &config.CPIConfig{
		VMTypes: map[string]config.TypeProfile{
			"med": {CloudProperties: map[string]any{"weight": float64(0.8)}},
		},
	}
	callCP := map[string]any{"vm_type": "med"}
	r, _ := newLayeredResolver(callCP, cfg)
	got, ok := r.Float("weight")
	if !ok || got != 0.8 {
		t.Errorf("Float(weight) = %v,%v; want 0.8,true (vm_type fallback)", got, ok)
	}
}
