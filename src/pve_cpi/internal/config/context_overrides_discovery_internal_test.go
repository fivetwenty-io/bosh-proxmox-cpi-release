package config

import (
	"encoding/json"
	"testing"
)

// discoveryOverrideBase returns a valid token-auth job config that verifies
// TLS, which is what the fixture cases run against.
func discoveryOverrideBase() *CPIConfig {
	verify := true
	return &CPIConfig{
		Host: "pve-a.example", Port: 8006, User: "u", Realm: "pve", APIToken: "u@pve!cpi=fake-token",
		VMStorage: "s", DiskStorage: "s", NetworkBridge: "br", VerifySSL: &verify,
		AgentMode: "cloudinit", VMDiskFormat: "qcow2", LogLevel: "info",
		VMIDRangeStart: 100, VMIDRangeEnd: 5999, RebootMode: "soft", RebootTimeout: 60,
		NetworkMode: "auto", SDNZoneType: "simple",
	}
}

// TestApplyContextOverrides_DiscoveryModeRefusalsMatchTheSharedFixture asserts
// that the override key refuses every mode value the shared refusal fixture
// lists for the job-level property, with the fixture's exact message after the
// override prefix. The fixture covers wrong case, padding, control and
// non-ASCII characters, the empty string, and values of the wrong JSON type
// with the bounded echo.
func TestApplyContextOverrides_DiscoveryModeRefusalsMatchTheSharedFixture(t *testing.T) {
	t.Parallel()
	var cases []templateRefusal
	readTemplateFixture(t, "discovery_refusals.json", &cases)
	const prefix = `config: context override "pve_api_endpoint_discovery": `
	checked := 0
	for _, tc := range cases {
		raw, ok := tc.Config["api_endpoint_discovery"]
		if !ok || len(tc.Config) != 1 {
			continue
		}
		checked++
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			var value any
			if err := json.Unmarshal(raw, &value); err != nil {
				t.Fatalf("decode fixture value %s: %v", raw, err)
			}
			_, _, _, err := ApplyContextOverrides(discoveryOverrideBase(), map[string]any{
				"pve_api_endpoint_discovery": value,
			})
			if err == nil {
				t.Fatalf("ApplyContextOverrides accepted %s, want the refusal %q", raw, tc.Message)
			}
			if want := prefix + tc.Message; err.Error() != want {
				t.Errorf("error\n  %q\nwant\n  %q", err.Error(), want)
			}
		})
	}
	if checked < 20 {
		t.Fatalf("only %d mode cases found in the fixture, want the whole mode block", checked)
	}
}

// TestCoerceOverrideDiscoveryMode_AgreesWithTheJobLevelDecoder asserts that
// the override coercion and DiscoveryMode.UnmarshalJSON map the same inputs
// to the same modes, so the two never drift.
func TestCoerceOverrideDiscoveryMode_AgreesWithTheJobLevelDecoder(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`"auto"`, `"on"`, `"off"`, `true`, `false`} {
		var decoded DiscoveryMode
		if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		var value any
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		got, err := coerceOverrideDiscoveryMode(value)
		if err != nil || got != decoded {
			t.Errorf("%s: coerce = %q, %v, want %q", raw, got, err, decoded)
		}
	}
}

// TestMarkInheritedCredsLooserTLS_KeepsAFlagTheBaseCarries asserts that the
// flag is only raised here, so an effective config derived from a flagged
// base stays flagged even when the entry changes nothing about TLS.
func TestMarkInheritedCredsLooserTLS_KeepsAFlagTheBaseCarries(t *testing.T) {
	t.Parallel()
	base := discoveryOverrideBase()
	base.inheritedCredsLooserTLS = true
	eff := *base
	markInheritedCredsLooserTLS(base, &eff, map[string]any{"pve_vm_storage": "other"})
	if !eff.inheritedCredsLooserTLS {
		t.Error("the flag was lost")
	}

	clean := discoveryOverrideBase()
	eff = *clean
	markInheritedCredsLooserTLS(clean, &eff, map[string]any{"pve_vm_storage": "other"})
	if eff.inheritedCredsLooserTLS {
		t.Error("the flag was raised for an entry that changed no TLS setting")
	}
}

// TestDropNullUnsetKeys covers the null rule for both keys and the guarantee
// that the caller's map is never modified.
func TestDropNullUnsetKeys(t *testing.T) {
	t.Parallel()
	in := map[string]any{
		"pve_endpoints": nil, "pve_api_endpoint_discovery": nil, "pve_vm_storage": nil, "pve_host": "h",
	}
	out := dropNullUnsetKeys(in)
	if len(in) != 4 {
		t.Errorf("the input map was modified: %v", in)
	}
	if len(out) != 2 {
		t.Fatalf("output = %v, want only pve_vm_storage and pve_host", out)
	}
	if _, kept := out["pve_vm_storage"]; !kept {
		t.Error("a null on a key outside the rule must stay, so its own coercion refuses it")
	}
	set := map[string]any{"pve_endpoints": []any{}, "pve_api_endpoint_discovery": "off"}
	if got := dropNullUnsetKeys(set); len(got) != 2 {
		t.Errorf("an empty list and a named mode must stay, got %v", got)
	}
}
