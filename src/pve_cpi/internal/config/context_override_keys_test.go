package config_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

func TestInspectContextKeys_Fails(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		extra map[string]any
		want  []string // substrings of the error
	}{
		{"host with user", map[string]any{"host": "h", "user": "u"},
			[]string{`"host"`, `"user"`, config.ContextMisnestHint}},
		{"host with password", map[string]any{"host": "h", "password": "secret-value"},
			[]string{`"host"`, `"password"`, config.ContextMisnestHint}},
		{"host with api_token and node", map[string]any{"host": "h", "api_token": "t", "node": "n"},
			[]string{`"api_token"`, `"host"`, config.ContextMisnestHint}},
		{"pve null does not disarm the rule", map[string]any{"pve": nil, "host": "h", "user": "u", "api_token": "t"},
			[]string{`"api_token"`, `"host"`, `"user"`, config.ContextMisnestHint}},
		{"pve typo", map[string]any{"pve": map[string]any{"host": "h", "vmid_rang_start": 1.0}},
			[]string{`"pve.vmid_rang_start"`}},
		{"agent typo", map[string]any{"agent": map[string]any{"mbsu": "https://x"}},
			[]string{`"agent.mbsu"`}},
		{"flat typo", map[string]any{"pve_host": "h", "pve_vmid_rang_start": 1.0},
			[]string{`"pve_vmid_rang_start"`}},
		{"every typo is named", map[string]any{"pve": map[string]any{"hots": "h", "usre": "u"}, "agent": map[string]any{"mbsu": "x"}},
			[]string{`"agent.mbsu"`, `"pve.hots"`, `"pve.usre"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := config.InspectContextKeys(tc.extra)
			if err == nil {
				t.Fatal("want an error")
			}
			for _, s := range tc.want {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q lacks %q", err, s)
				}
			}
			for _, secret := range []string{"secret-value", "https://x"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error %q leaks a value", err)
				}
			}
		})
	}
}

func TestInspectContextKeys_Passes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		extra map[string]any
		want  config.ContextKeyFindings
	}{
		{"empty", nil, config.ContextKeyFindings{}},
		{"lone host warns", map[string]any{"host": "h"},
			config.ContextKeyFindings{MisnestedSpecKeys: []string{"host"}}},
		{"credentials without host warn", map[string]any{"user": "u", "password": "p"},
			config.ContextKeyFindings{MisnestedSpecKeys: []string{"password", "user"}}},
		{"host with credentials but a pve map is not misnesting",
			map[string]any{"pve": map[string]any{"node": "n"}, "host": "h", "user": "u"},
			config.ContextKeyFindings{MisnestedSpecKeys: []string{"host", "user"}}},
		{"host with credentials next to a flat key is not misnesting",
			map[string]any{"pve_node": "n", "host": "h", "user": "u"},
			config.ContextKeyFindings{MisnestedSpecKeys: []string{"host", "user"}}},
		{"nested spec name prefix warns", map[string]any{"placement": map[string]any{}, "mbus": "m"},
			config.ContextKeyFindings{MisnestedSpecKeys: []string{"mbus", "placement"}}},
		{"unknown Director-style keys only log", map[string]any{"future_director_key": 1.0, "cpi_name": "x"},
			config.ContextKeyFindings{OtherTopLevelKeys: []string{"cpi_name", "future_director_key"}}},
		{"spec key without an override is not a typo",
			map[string]any{"pve": map[string]any{"host": "h", "hooks": map[string]any{}, "placement": map[string]any{}}},
			config.ContextKeyFindings{}},
		{"dotted spec name and flat spec name pass",
			map[string]any{"pve": map[string]any{"placement.enabled": true}, "pve_placement_enabled": true},
			config.ContextKeyFindings{}},
		{"agent blobstore warns", map[string]any{"agent": map[string]any{"mbus": "m", "blobstore": map[string]any{}}},
			config.ContextKeyFindings{UnsupportedAgentKeys: []string{"agent.blobstore"}}},
		{"a pve map without a host and an agent-only entry pass",
			map[string]any{"pve": map[string]any{"node": "n"}, "agent": map[string]any{"mbus": "m"}},
			config.ContextKeyFindings{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := config.InspectContextKeys(tc.extra)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("findings = %#v, want %#v", got, tc.want)
			}
		})
	}
}
