package config_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// -----------------------------------------------------------------------
// ApplyContextOverrides — no-op / byte-identical fast path
// -----------------------------------------------------------------------

func TestApplyContextOverrides_NilBase(t *testing.T) {
	t.Parallel()
	_, _, _, err := config.ApplyContextOverrides(nil, map[string]any{"pve_host": "x"})
	if err == nil {
		t.Fatal("expected error for nil base, got nil")
	}
}

func TestApplyContextOverrides_EmptyExtra_ReturnsBaseUnchanged(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()

	eff, applied, unknown, err := config.ApplyContextOverrides(base, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff != base {
		t.Error("expected the SAME pointer back (byte-identical fast path) for nil extra")
	}
	if applied != nil {
		t.Errorf("applied = %#v, want nil", applied)
	}
	if unknown != nil {
		t.Errorf("unknown = %#v, want nil", unknown)
	}

	// Same contract for an explicitly-empty (non-nil) map.
	eff2, applied2, unknown2, err2 := config.ApplyContextOverrides(base, map[string]any{})
	if err2 != nil {
		t.Fatalf("unexpected error: %v", err2)
	}
	if eff2 != base {
		t.Error("expected the SAME pointer back for an empty (non-nil) extra map")
	}
	if applied2 != nil || unknown2 != nil {
		t.Errorf("applied2=%#v unknown2=%#v, want both nil", applied2, unknown2)
	}
}

func TestApplyContextOverrides_OnlyUnknownKeys_ReturnsBaseUnchanged(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()

	// pve_placement_enabled is a real job property, but is deliberately not
	// in the per-request override set (it is process-wide placement policy,
	// not connection/routing identity) — see contextOverrideFieldOrder's doc.
	eff, applied, unknown, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_placement_enabled": true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff != base {
		t.Error("expected the SAME pointer back when no SUPPORTED override key is present")
	}
	if applied != nil {
		t.Errorf("applied = %#v, want nil", applied)
	}
	if len(unknown) != 1 || unknown[0] != "pve_placement_enabled" {
		t.Errorf("unknown = %#v, want [pve_placement_enabled]", unknown)
	}
}

func TestApplyContextOverrides_NonPVEPrefixedKeys_Ignored(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	eff, applied, unknown, err := config.ApplyContextOverrides(base, map[string]any{
		"some_future_context_key": "value",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff != base || applied != nil || unknown != nil {
		t.Errorf("non-pve_-prefixed extra keys must be silently ignored, got eff==base:%v applied:%#v unknown:%#v",
			eff == base, applied, unknown)
	}
}

// -----------------------------------------------------------------------
// ApplyContextOverrides — override application, base immutability, and
// absent-key inheritance
// -----------------------------------------------------------------------

func TestApplyContextOverrides_AppliesOverrides_BaseUnmutated(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	base.Node = "pve01"
	base.NetworkBridge = "vmbr0"

	eff, applied, unknown, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_host":       "10.255.0.10",
		"pve_vm_storage": "az2-vms",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(unknown) != 0 {
		t.Errorf("unknown = %#v, want empty", unknown)
	}
	wantApplied := []string{"pve_host", "pve_vm_storage"} // already alpha-sorted
	if !equalStringSlices(applied, wantApplied) {
		t.Errorf("applied = %#v, want %#v", applied, wantApplied)
	}

	if eff == base {
		t.Fatal("expected a NEW *CPIConfig when overrides applied, got the same pointer")
	}
	if eff.Host != "10.255.0.10" {
		t.Errorf("eff.Host = %q, want 10.255.0.10", eff.Host)
	}
	if eff.VMStorage != "az2-vms" {
		t.Errorf("eff.VMStorage = %q, want az2-vms", eff.VMStorage)
	}

	// Absent keys inherit job config unchanged.
	if eff.Node != "pve01" {
		t.Errorf("eff.Node = %q, want inherited pve01", eff.Node)
	}
	if eff.NetworkBridge != "vmbr0" {
		t.Errorf("eff.NetworkBridge = %q, want inherited vmbr0", eff.NetworkBridge)
	}

	// base itself must never be mutated by ApplyContextOverrides.
	if base.Host != "h" {
		t.Errorf("base.Host mutated to %q, want unchanged \"h\"", base.Host)
	}
	if base.VMStorage != "s" {
		t.Errorf("base.VMStorage mutated to %q, want unchanged \"s\"", base.VMStorage)
	}
}

func TestApplyContextOverrides_VerifySSL_NewPointer_BaseUnmutated(t *testing.T) {
	t.Parallel()
	base := validBaseCfg() // VerifySSL = true
	if !base.VerifySSLValue() {
		t.Fatal("test fixture assumption broken: base.VerifySSLValue() should start true")
	}

	eff, applied, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_verify_ssl": false,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(applied) != 1 || applied[0] != "pve_verify_ssl" {
		t.Errorf("applied = %#v, want [pve_verify_ssl]", applied)
	}
	if eff.VerifySSLValue() != false {
		t.Errorf("eff.VerifySSLValue() = true, want false")
	}
	if base.VerifySSLValue() != true {
		t.Errorf("base.VerifySSLValue() mutated to false, want unchanged true")
	}
	// eff.VerifySSL must be a DIFFERENT pointer than base.VerifySSL, proving
	// the override allocated a fresh *bool rather than writing through the
	// shared shallow-copy pointer.
	if eff.VerifySSL == base.VerifySSL {
		t.Error("eff.VerifySSL and base.VerifySSL point at the same *bool; override must allocate a new one")
	}
}

// -----------------------------------------------------------------------
// Empty-string vs unset semantics
// -----------------------------------------------------------------------

func TestApplyContextOverrides_ExplicitEmptyString_Applies(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	base.AgentMBus = "https://mbus:pw@job-level-mbus:6868"

	// An explicit empty-string override for pve_agent_mbus must APPLY (clear
	// the inherited value), not be treated as "absent". pve_agent_mbus is
	// used here specifically because it carries no eff.Validate() constraint
	// (unlike, say, pve_password — see
	// TestApplyContextOverrides_EmptyString_ClearingBothCredentials_Rejected
	// below), so this test isolates the empty-string-applies SEMANTIC from
	// the separate question of whether the resulting effective config is
	// still valid.
	eff, applied, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_agent_mbus": "",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(applied) != 1 || applied[0] != "pve_agent_mbus" {
		t.Errorf("applied = %#v, want [pve_agent_mbus] (explicit empty string must count as applied)", applied)
	}
	if eff.AgentMBus != "" {
		t.Errorf("eff.AgentMBus = %q, want empty string", eff.AgentMBus)
	}
	if base.AgentMBus != "https://mbus:pw@job-level-mbus:6868" {
		t.Errorf("base.AgentMBus mutated to %q, want unchanged", base.AgentMBus)
	}
}

func TestApplyContextOverrides_AbsentKey_Inherits(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	base.Password = "job-level-password"

	// pve_password absent entirely (only pve_host present) — Password must
	// be INHERITED from base, not cleared.
	eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_host": "10.255.0.10",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff.Password != "job-level-password" {
		t.Errorf("eff.Password = %q, want inherited \"job-level-password\" (key was absent, not empty)", eff.Password)
	}
}

// -----------------------------------------------------------------------
// Type coercion — numbers and booleans arriving as either JSON type or string
// -----------------------------------------------------------------------

func TestApplyContextOverrides_PortCoercion(t *testing.T) {
	t.Parallel()

	t.Run("as JSON number", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_port": float64(8007)})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.Port != 8007 {
			t.Errorf("eff.Port = %d, want 8007", eff.Port)
		}
	})

	t.Run("as numeric string", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_port": "8007"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.Port != 8007 {
			t.Errorf("eff.Port = %d, want 8007", eff.Port)
		}
	})

	t.Run("non-integer number rejected", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_port": 8006.5})
		if err == nil {
			t.Fatal("expected error for non-integer pve_port, got nil")
		}
	})

	t.Run("non-numeric string rejected", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_port": "not-a-number"})
		if err == nil {
			t.Fatal("expected error for non-numeric pve_port string, got nil")
		}
		if !strings.Contains(err.Error(), "pve_port") {
			t.Errorf("error %q should mention pve_port", err.Error())
		}
	})

	t.Run("out of range rejected", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_port": 99999})
		if err == nil {
			t.Fatal("expected error for out-of-range pve_port, got nil")
		}
	})

	t.Run("wrong JSON type rejected", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_port": true})
		if err == nil {
			t.Fatal("expected error for boolean pve_port, got nil")
		}
	})
}

func TestApplyContextOverrides_VerifySSLCoercion(t *testing.T) {
	t.Parallel()

	t.Run("as JSON bool", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_verify_ssl": false})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.VerifySSLValue() != false {
			t.Error("eff.VerifySSLValue() = true, want false")
		}
	})

	t.Run("as string", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_verify_ssl": "false"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.VerifySSLValue() != false {
			t.Error("eff.VerifySSLValue() = true, want false")
		}
	})

	t.Run("as \"1\"/\"0\" string", func(t *testing.T) {
		t.Parallel()
		eff, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_verify_ssl": "1"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if eff.VerifySSLValue() != true {
			t.Error("eff.VerifySSLValue() = false, want true (\"1\" parses as true)")
		}
	})

	t.Run("invalid string rejected", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_verify_ssl": "not-a-bool"})
		if err == nil {
			t.Fatal("expected error for invalid pve_verify_ssl string, got nil")
		}
	})

	t.Run("wrong JSON type rejected", func(t *testing.T) {
		t.Parallel()
		_, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_verify_ssl": 1.0})
		if err == nil {
			t.Fatal("expected error for numeric pve_verify_ssl, got nil")
		}
	})
}

// -----------------------------------------------------------------------
// pve_vmid_range_start/end structural validation
// -----------------------------------------------------------------------

func TestApplyContextOverrides_VMIDRange_BothOverridden_OrderEnforced(t *testing.T) {
	t.Parallel()
	_, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{
		"pve_vmid_range_start": 500,
		"pve_vmid_range_end":   400,
	})
	if err == nil {
		t.Fatal("expected error for inverted vmid range when both bounds are overridden together")
	}
	// The error now comes from eff.Validate() (config.go's own field-name
	// convention "vmid_range_end", not the context key "pve_vmid_range_end").
	if !strings.Contains(err.Error(), "vmid_range_end") {
		t.Errorf("error %q should name vmid_range_end", err.Error())
	}
}

func TestApplyContextOverrides_VMIDRange_BothOverridden_ValidOrder(t *testing.T) {
	t.Parallel()
	eff, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{
		"pve_vmid_range_start": 400,
		"pve_vmid_range_end":   500,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff.VMIDRangeStart != 400 || eff.VMIDRangeEnd != 500 {
		t.Errorf("eff range = [%d,%d], want [400,500]", eff.VMIDRangeStart, eff.VMIDRangeEnd)
	}
}

func TestApplyContextOverrides_VMIDRange_OnlyStartOverridden_InheritsEnd(t *testing.T) {
	t.Parallel()
	base := validBaseCfg() // VMIDRangeEnd = 5999 per validBaseCfg
	eff, _, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_vmid_range_start": 100,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff.VMIDRangeStart != 100 {
		t.Errorf("eff.VMIDRangeStart = %d, want 100", eff.VMIDRangeStart)
	}
	if eff.VMIDRangeEnd != base.VMIDRangeEnd {
		t.Errorf("eff.VMIDRangeEnd = %d, want inherited %d", eff.VMIDRangeEnd, base.VMIDRangeEnd)
	}
}

func TestApplyContextOverrides_VMIDRangeStart_NonPositive_Rejected(t *testing.T) {
	t.Parallel()
	_, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_vmid_range_start": 0})
	if err == nil {
		t.Fatal("expected error for pve_vmid_range_start=0, got nil")
	}
}

// -----------------------------------------------------------------------
// H1 (A13 review) reproducers — the effective override config must be
// re-validated against the SAME invariants config.Validate checks for the
// job-level config at CPI startup. Each test below is a confirmed
// reproducer from the review that the (now-removed) ad-hoc "only check vmid
// ordering when both bounds are overridden together" logic missed.
// -----------------------------------------------------------------------

// TestApplyContextOverrides_VMIDRange_SingleBoundInversion_Rejected is the
// primary H1 reproducer: overriding ONLY pve_vmid_range_start above the
// INHERITED pve_vmid_range_end (never itself touched by this request)
// previously passed silently, because the old ad-hoc check only fired when
// both bounds were overridden in the same request. The inherited end (5999,
// from validBaseCfg) is itself perfectly valid on its own — the violation
// only exists in combination with the override — which is exactly why
// re-validating the full EFFECTIVE config (not just the overridden fields in
// isolation) is required.
func TestApplyContextOverrides_VMIDRange_SingleBoundInversion_Rejected(t *testing.T) {
	t.Parallel()
	base := validBaseCfg() // VMIDRangeEnd = 5999
	_, _, _, err := config.ApplyContextOverrides(base, map[string]any{
		"pve_vmid_range_start": 9000, // > inherited end 5999; pve_vmid_range_end NOT overridden
	})
	if err == nil {
		t.Fatal("expected error: overriding only vmid_range_start above the inherited vmid_range_end must be rejected")
	}
	if !strings.Contains(err.Error(), "vmid_range_end") {
		t.Errorf("error %q should name vmid_range_end (the effective, inherited bound the override now conflicts with)", err.Error())
	}
}

// TestApplyContextOverrides_VMIDRange_OverlapsDiskBand_Rejected overrides
// both VM VMID bounds (order is internally valid, 9000 < 10000) into the
// DEFAULT persistent-disk VMID band (9000-29999), producing colliding VMIDs
// between VMs and persistent-disk containers — a data-integrity violation
// config.Validate's validateVMIDBands rejects for the job-level config, and
// which the override path must reject identically.
func TestApplyContextOverrides_VMIDRange_OverlapsDiskBand_Rejected(t *testing.T) {
	t.Parallel()
	_, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{
		"pve_vmid_range_start": 9000,
		"pve_vmid_range_end":   10000,
	})
	if err == nil {
		t.Fatal("expected error: VM VMID range overlapping the persistent-disk band must be rejected")
	}
	if !strings.Contains(err.Error(), "overlap") {
		t.Errorf("error %q should mention the band overlap", err.Error())
	}
}

// TestApplyContextOverrides_VMIDRangeStart_BelowPVEReservedFloor_Rejected
// covers vmid_range_start values in (0, 100) — the old field-local check
// only rejected n<=0, silently accepting anything from 1 to 99 despite PVE
// reserving VMIDs below 100.
func TestApplyContextOverrides_VMIDRangeStart_BelowPVEReservedFloor_Rejected(t *testing.T) {
	t.Parallel()
	_, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{
		"pve_vmid_range_start": 50,
		"pve_vmid_range_end":   5999,
	})
	if err == nil {
		t.Fatal("expected error for pve_vmid_range_start=50 (below PVE's reserved floor of 100)")
	}
	if !strings.Contains(err.Error(), "100") {
		t.Errorf("error %q should mention the reserved floor 100", err.Error())
	}
}

// TestApplyContextOverrides_Port_Zero_Rejected covers pve_port=0 — the old
// field-local check accepted [0,65535], silently letting an operator set
// port=0 (config.Validate requires [1,65535] for the job-level config; there
// is no "0 means use the SDK default 8006" semantic for an explicit
// per-request override, unlike ApplyDefaults' job-level JSON-unset case).
func TestApplyContextOverrides_Port_Zero_Rejected(t *testing.T) {
	t.Parallel()
	_, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_port": 0})
	if err == nil {
		t.Fatal("expected error for pve_port=0, got nil")
	}
}

// TestApplyContextOverrides_EmptyString_ClearingRequiredField_Rejected
// covers clearing a startup-required field (host) via an explicit
// empty-string override. Previously this silently produced an effective
// config with host="" that only surfaced as a confusing downstream RETRIABLE
// failure (pve.NewClient's "host is required" CloudError arrives through the
// resolve() path, wrapped as retriable — see H1) instead of an immediate,
// clearly-attributed, non-retriable rejection at the override boundary.
func TestApplyContextOverrides_EmptyString_ClearingRequiredField_Rejected(t *testing.T) {
	t.Parallel()
	_, _, _, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{"pve_host": ""})
	if err == nil {
		t.Fatal("expected error when clearing the required pve_host field, got nil")
	}
	if !strings.Contains(err.Error(), "host") {
		t.Errorf("error %q should mention host", err.Error())
	}
}

// TestApplyContextOverrides_EmptyString_ClearingBothCredentials_Rejected
// covers clearing pve_password while the effective config has no api_token
// (validBaseCfg sets Password only), leaving the effective config with NO
// auth credential at all — config.Validate's validateAuth rejects this for
// the job-level config, and the override path must reject it identically
// rather than producing an auth-less client that fails downstream.
func TestApplyContextOverrides_EmptyString_ClearingBothCredentials_Rejected(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	base.Password = "job-level-password"
	base.APIToken = ""

	_, _, _, err := config.ApplyContextOverrides(base, map[string]any{"pve_password": ""})
	if err == nil {
		t.Fatal("expected error when pve_password is cleared and no api_token is present")
	}
	if !strings.Contains(err.Error(), "password or api_token") {
		t.Errorf("error %q should mention the password/api_token requirement", err.Error())
	}
	// base itself must never be mutated, including on the rejected path.
	if base.Password != "job-level-password" {
		t.Errorf("base.Password mutated to %q, want unchanged", base.Password)
	}
}

// TestApplyContextOverrides_EmptyString_ClearingPassword_WithAPIToken_Applies
// is the companion positive case: clearing pve_password is VALID when
// api_token is (or is concurrently overridden to be) non-empty — the whole
// point of the empty-string-applies semantic is to let a cpi-config entry
// switch from password auth to token auth. This must keep working after the
// H1 re-validation fix.
func TestApplyContextOverrides_EmptyString_ClearingPassword_WithAPIToken_Applies(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	base.Password = "job-level-password"
	base.APIToken = "root@pam!cpi=deadbeef-token"

	eff, applied, _, err := config.ApplyContextOverrides(base, map[string]any{"pve_password": ""})
	if err != nil {
		t.Fatalf("unexpected error clearing password while api_token remains set: %v", err)
	}
	if eff.Password != "" {
		t.Errorf("eff.Password = %q, want empty string", eff.Password)
	}
	if eff.APIToken != "root@pam!cpi=deadbeef-token" {
		t.Errorf("eff.APIToken = %q, want inherited unchanged", eff.APIToken)
	}
	if len(applied) != 1 || applied[0] != "pve_password" {
		t.Errorf("applied = %#v, want [pve_password]", applied)
	}
}

// -----------------------------------------------------------------------
// Unknown pve_* keys never fail the merge
// -----------------------------------------------------------------------

func TestApplyContextOverrides_UnknownKeysAlongsideKnown_NoError(t *testing.T) {
	t.Parallel()
	eff, applied, unknown, err := config.ApplyContextOverrides(validBaseCfg(), map[string]any{
		"pve_host":              "10.255.0.10",
		"pve_placement_enabled": true,
		"pve_hooks":             []any{"audit_log"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff.Host != "10.255.0.10" {
		t.Errorf("eff.Host = %q, want 10.255.0.10", eff.Host)
	}
	if len(applied) != 1 || applied[0] != "pve_host" {
		t.Errorf("applied = %#v, want [pve_host]", applied)
	}
	wantUnknown := []string{"pve_hooks", "pve_placement_enabled"}
	if !equalStringSlices(unknown, wantUnknown) {
		t.Errorf("unknown = %#v, want %#v", unknown, wantUnknown)
	}
}

// -----------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	aSorted := append([]string(nil), a...)
	bSorted := append([]string(nil), b...)
	sort.Strings(aSorted)
	sort.Strings(bSorted)
	for i := range aSorted {
		if aSorted[i] != bSorted[i] {
			return false
		}
	}
	return true
}

// TestContextOverrideFieldOrder is the sync assertion the source comments at
// contextOverrideFieldOrder / contextOverrideFields have cited since the
// feature landed: every entry in the order slice must have a matching apply
// function, every apply function must be listed in the order slice, and the
// slice must be duplicate-free. A 19th overridable field added to one side
// but not the other would either be silently skipped by
// ApplyContextOverrides or invisible to the audit ordering — this converts
// that mistake into a failing build.
func TestContextOverrideFieldOrder(t *testing.T) {
	t.Parallel()
	order := config.ContextOverrideFieldOrderForTest()
	keys := config.ContextOverrideFieldKeysForTest()

	seen := make(map[string]bool, len(order))
	for _, f := range order {
		if seen[f] {
			t.Errorf("contextOverrideFieldOrder contains duplicate entry %q", f)
		}
		seen[f] = true
	}
	keySet := make(map[string]bool, len(keys))
	for _, k := range keys {
		keySet[k] = true
	}
	for _, f := range order {
		if !keySet[f] {
			t.Errorf("field %q is in contextOverrideFieldOrder but has no apply function in contextOverrideFields", f)
		}
	}
	for _, k := range keys {
		if !seen[k] {
			t.Errorf("field %q has an apply function but is missing from contextOverrideFieldOrder", k)
		}
	}
}

// TestApplyContextOverrides_NestedCPIConfigShape verifies the shape BOSH's
// cpi-config feature actually delivers (live-verified against a 282.x
// director): the entry's whole properties hash arrives NESTED in the request
// context as context.pve = {...} and context.agent = {mbus: ...} — not as
// flat pve_* keys. Every supported nested property must apply, agent.mbus
// must land on AgentMBus, and unsupported nested properties must surface in
// unknown under their flat name.
func TestApplyContextOverrides_NestedCPIConfigShape(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()

	extra := map[string]any{
		"director_uuid": "d-1",
		"request_id":    "cpi-1",
		"agent":         map[string]any{"mbus": "nats://10.254.48.10:4222"},
		"pve": map[string]any{
			"host":                               "10.255.0.10",
			"node":                               "az2-node",
			"vm_storage":                         "az2-lvm",
			"disk_storage":                       "az2-lvm",
			"stemcell_storage":                   "nfs-shared",
			"iso_storage":                        "local",
			"vmid_range_start":                   float64(5000),
			"vmid_range_end":                     float64(8999),
			"disk_vmid_range_start":              float64(20000),
			"disk_vmid_range_end":                float64(29999),
			"stemcell_template_vmid_range_start": float64(30500),
			"stemcell_template_vmid_range_end":   float64(30999),
			"stemcell_replicate_local":           true,
			"vm_prefix":                          "az2",
			"log_level":                          "info", // deliberately unsupported per-request
		},
	}

	eff, applied, unknown, err := config.ApplyContextOverrides(base, extra)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff.Host != "10.255.0.10" || eff.Node != "az2-node" {
		t.Errorf("host/node = %q/%q, want the nested entry's cluster", eff.Host, eff.Node)
	}
	if eff.StemcellStorage != "nfs-shared" || eff.VMStorage != "az2-lvm" {
		t.Errorf("storages = %q/%q, want nested values", eff.StemcellStorage, eff.VMStorage)
	}
	if eff.VMIDRangeStart != 5000 || eff.VMIDRangeEnd != 8999 {
		t.Errorf("vm band = %d-%d, want 5000-8999", eff.VMIDRangeStart, eff.VMIDRangeEnd)
	}
	if eff.DiskVMIDRangeStart != 20000 || eff.DiskVMIDRangeEnd != 29999 {
		t.Errorf("disk band = %d-%d, want 20000-29999", eff.DiskVMIDRangeStart, eff.DiskVMIDRangeEnd)
	}
	if eff.StemcellTemplateVMIDRangeStart != 30500 || eff.StemcellTemplateVMIDRangeEnd != 30999 {
		t.Errorf("template band = %d-%d, want 30500-30999",
			eff.StemcellTemplateVMIDRangeStart, eff.StemcellTemplateVMIDRangeEnd)
	}
	if !eff.StemcellReplicateLocal {
		t.Error("stemcell_replicate_local must apply from the nested entry")
	}
	if eff.VMPrefix != "az2" {
		t.Errorf("vm_prefix = %q, want az2", eff.VMPrefix)
	}
	if eff.AgentMBus != "nats://10.254.48.10:4222" {
		t.Errorf("agent mbus = %q, want the nested agent.mbus", eff.AgentMBus)
	}
	if len(applied) == 0 {
		t.Fatal("applied is empty — the nested shape did not fold into overrides")
	}
	foundUnknown := false
	for _, k := range unknown {
		if k == "pve_log_level" {
			foundUnknown = true
		}
	}
	if !foundUnknown {
		t.Errorf("unknown = %v, want it to carry pve_log_level (unsupported nested property)", unknown)
	}
	// Input map must not be mutated by the fold.
	if _, mutated := extra["pve_host"]; mutated {
		t.Error("input extra map was mutated with flattened keys")
	}
	// Base must stay untouched.
	if base.Host != "h" || base.StemcellReplicateLocal {
		t.Error("base config was mutated")
	}
}

// TestApplyContextOverrides_NestedFlatPrecedence verifies explicit flat keys
// win over the nested entry hash.
func TestApplyContextOverrides_NestedFlatPrecedence(t *testing.T) {
	t.Parallel()
	base := validBaseCfg()
	extra := map[string]any{
		"pve_host": "flat-wins",
		"pve":      map[string]any{"host": "nested-loses"},
	}
	eff, _, _, err := config.ApplyContextOverrides(base, extra)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if eff.Host != "flat-wins" {
		t.Errorf("host = %q, want the explicit flat key to win", eff.Host)
	}
}
