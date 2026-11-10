package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// TestParseCPIConfig_DiscoveryModeRefusalEchoesTheRawText asserts that an entry
// whose discovery mode has the wrong JSON type is refused with exactly the
// text the job-level decoder gives for the same bytes, whether the mode sits
// in the flat key or in the pve member.
func TestParseCPIConfig_DiscoveryModeRefusalEchoesTheRawText(t *testing.T) {
	t.Parallel()
	const prefix = `config: context override "pve_api_endpoint_discovery": `
	for _, raw := range []string{`5.0`, `1e3`, `12345678901234567890`, `{ "b" : 2 , "a" : [ 1 , 2 ] }`, `[ "on" ]`} {
		for name, properties := range map[string]string{
			"flat":   `{"pve_api_endpoint_discovery":` + raw + `}`,
			"nested": `{"pve":{"api_endpoint_discovery":` + raw + `}}`,
		} {
			t.Run(name+" "+raw, func(t *testing.T) {
				t.Parallel()
				var job config.DiscoveryMode
				jobErr := json.Unmarshal([]byte(raw), &job)
				if jobErr == nil {
					t.Fatalf("the job-level decoder accepted %s", raw)
				}
				entries, err := parseCPIConfig([]byte(`{"cpis":[{"name":"a","type":"pve","properties":` + properties + `}]}`))
				if err != nil {
					t.Fatalf("parseCPIConfig: %v", err)
				}
				_, _, _, err = config.ApplyContextOverrides(haCfg("auto", "none"), entries[0].Properties)
				if err == nil {
					t.Fatalf("%s: accepted", properties)
				}
				if want := prefix + jobErr.Error(); err.Error() != want {
					t.Errorf("error\n  %q\nwant\n  %q", err.Error(), want)
				}
			})
		}
	}
}

// TestParseCPIConfig_OtherPropertiesDecodeAsBefore asserts that only the
// discovery mode keeps raw text. A usable mode and every other property,
// numbers included, decode to the types plain decoding gives.
func TestParseCPIConfig_OtherPropertiesDecodeAsBefore(t *testing.T) {
	t.Parallel()
	const properties = `{"pve_port":8006.0,"pve_vm_storage":"s","pve_api_endpoint_discovery":"on",` +
		`"pve":{"port":1.0,"api_endpoint_discovery":true,"vm_storage":"s","sdn_zone_mtu":1400,"ha":{"mode":"auto"}},` +
		`"agent":{"mbus":"nats://x"}}`
	entries, err := parseCPIConfig([]byte(`[{"name":"a","properties":` + properties + `}]`))
	if err != nil {
		t.Fatalf("parseCPIConfig: %v", err)
	}
	var plain map[string]any
	if err := json.Unmarshal([]byte(properties), &plain); err != nil {
		t.Fatal(err)
	}
	if got := entries[0].Properties; !reflect.DeepEqual(got, plain) {
		t.Errorf("properties = %#v, want what plain decoding gives, %#v", got, plain)
	}
	if port, ok := entries[0].Properties["pve_port"].(float64); !ok || port != 8006 {
		t.Errorf("pve_port = %#v, want float64(8006)", entries[0].Properties["pve_port"])
	}

	// A number in the mode is the one value that differs from plain decoding.
	entries, err = parseCPIConfig([]byte(`[{"name":"a","properties":{"pve_api_endpoint_discovery":5.0}}]`))
	if err != nil {
		t.Fatalf("parseCPIConfig: %v", err)
	}
	if got, ok := entries[0].Properties["pve_api_endpoint_discovery"].(json.RawMessage); !ok || strings.TrimSpace(string(got)) != "5.0" {
		t.Errorf("mode = %#v, want the raw text 5.0", entries[0].Properties["pve_api_endpoint_discovery"])
	}

	// An entry with no properties, and one that is not an object, behave as before.
	none, err := parseCPIConfig([]byte(`[{"name":"a"}]`))
	if err != nil || none[0].Properties != nil {
		t.Errorf("no properties: %#v, %v, want nil and no error", none, err)
	}
	if _, err := parseCPIConfig([]byte(`[{"name":"a","properties":[1]}]`)); err == nil {
		t.Error("an entry whose properties are a list was accepted")
	}
}
