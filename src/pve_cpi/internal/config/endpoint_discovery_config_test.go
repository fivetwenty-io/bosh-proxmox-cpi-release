package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// discoveryBaseCfg returns a valid, defaulted config that authenticates with
// an API token and verifies TLS, the combination every discovery mode accepts.
func discoveryBaseCfg() *CPIConfig {
	c := &CPIConfig{
		Host:          "pve1.example.com",
		User:          "root@pam",
		APIToken:      "root@pam!cpi=00000000-0000-0000-0000-000000000000",
		VMStorage:     "local-lvm",
		DiskStorage:   "local-lvm",
		NetworkBridge: "vmbr0",
	}
	c.ApplyDefaults()
	return c
}

// discoveryPasswordCfg is discoveryBaseCfg with password auth in place of the
// token.
func discoveryPasswordCfg() *CPIConfig {
	c := discoveryBaseCfg()
	c.APIToken = ""
	c.Password = "secret"
	return c
}

func discoveryBoolPtr(b bool) *bool { return &b }

// discoveryLoadJSON renders a minimal valid cpi.json with a password and the
// given extra object members.
func discoveryLoadJSON(extra string) string {
	return `{
		"host": "pve1.example.com",
		"user": "root@pam",
		"password": "secret",
		"vm_storage": "local-lvm",
		"disk_storage": "local-lvm",
		"network_bridge": "vmbr0"` + extra + `
	}`
}

func discoveryErrorOf(c *CPIConfig) string {
	if err := c.Validate(); err != nil {
		return err.Error()
	}
	return ""
}

func TestDiscoveryMode_DecodesStringsAndBooleans(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		json string
		want DiscoveryMode
	}{
		{"auto string", `"auto"`, DiscoveryModeAuto},
		{"on string", `"on"`, DiscoveryModeOn},
		{"off string", `"off"`, DiscoveryModeOff},
		{"true boolean is on", `true`, DiscoveryModeOn},
		{"false boolean is off", `false`, DiscoveryModeOff},
		{"empty string is unset", `""`, ""},
		{"unknown string kept for validation", `"sometimes"`, "sometimes"},
		{"wrong case kept for validation", `"ON"`, "ON"},
		{"string true is not a boolean", `"true"`, "true"},
		{"padded string kept for validation", `" on"`, " on"},
		{"padded boolean", ` true `, DiscoveryModeOn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var m DiscoveryMode
			if err := json.Unmarshal([]byte(tc.json), &m); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.json, err)
			}
			if m != tc.want {
				t.Errorf("decoded %s as %q, want %q", tc.json, m, tc.want)
			}
		})
	}
}

func TestDiscoveryMode_NullLeavesTheModeUnchanged(t *testing.T) {
	t.Parallel()
	m := DiscoveryModeOn
	if err := json.Unmarshal([]byte(`null`), &m); err != nil {
		t.Fatalf("unmarshal null: %v", err)
	}
	if m != DiscoveryModeOn {
		t.Errorf("null changed the mode to %q", m)
	}
	cfg, err := mustLoadDiscovery(t, discoveryLoadJSON(`, "api_endpoint_discovery": null`))
	if err != nil {
		t.Fatalf("load with a null mode: %v", err)
	}
	if cfg.APIEndpointDiscovery != "" {
		t.Errorf("null mode loaded as %q, want unset", cfg.APIEndpointDiscovery)
	}
}

func TestDiscoveryMode_RejectsOtherJSONTypes(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`5`, `1.5`, `[]`, `["on"]`, `{}`, `{"mode":"on"}`} {
		var m DiscoveryMode
		err := json.Unmarshal([]byte(raw), &m)
		if err == nil {
			t.Errorf("%s decoded as %q, want an error", raw, m)
			continue
		}
		if !strings.Contains(err.Error(), "api_endpoint_discovery: expected a string or a boolean") {
			t.Errorf("%s: error %q does not name the property and the accepted types", raw, err)
		}
	}
	if _, err := mustLoadDiscovery(t, discoveryLoadJSON(`, "api_endpoint_discovery": 5`)); err == nil {
		t.Error("Load accepted a numeric api_endpoint_discovery")
	}
}

func mustLoadDiscovery(t *testing.T, jsonStr string) (*CPIConfig, error) {
	t.Helper()
	return Load(strings.NewReader(jsonStr))
}

func TestLoad_ReadsDiscoveryPropertiesAndRoundTrips(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoadDiscovery(t, discoveryLoadJSON(
		`, "api_endpoint_discovery": "on", "endpoint_cache_dir": "/var/vcap/store/pve_cpi/cache"`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.APIEndpointDiscovery != DiscoveryModeOn {
		t.Errorf("APIEndpointDiscovery = %q, want on", cfg.APIEndpointDiscovery)
	}
	if cfg.EndpointCacheDir != "/var/vcap/store/pve_cpi/cache" {
		t.Errorf("EndpointCacheDir = %q", cfg.EndpointCacheDir)
	}
	out, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		`"api_endpoint_discovery":"on"`,
		`"endpoint_cache_dir":"/var/vcap/store/pve_cpi/cache"`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("marshaled config lacks %s: %s", want, out)
		}
	}
}

func TestLoad_BooleanModeBecomesOnAndOff(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]DiscoveryMode{"true": DiscoveryModeOn, "false": DiscoveryModeOff} {
		cfg, err := mustLoadDiscovery(t, discoveryLoadJSON(`, "verify_ssl": true, "api_endpoint_discovery": `+raw))
		if err != nil {
			t.Fatalf("load with %s: %v", raw, err)
		}
		if cfg.APIEndpointDiscovery != want {
			t.Errorf("%s loaded as %q, want %q", raw, cfg.APIEndpointDiscovery, want)
		}
	}
}

func TestDiscoveryProperties_UnsetAreOmittedAndValid(t *testing.T) {
	t.Parallel()
	c := discoveryBaseCfg()
	if err := c.Validate(); err != nil {
		t.Fatalf("base config invalid: %v", err)
	}
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"api_endpoint_discovery"`, `"endpoint_cache_dir"`} {
		if strings.Contains(string(out), key) {
			t.Errorf("a config without the property marshals %s: %s", key, out)
		}
	}
	if got := c.DiscoveryModeValue(); got != DiscoveryModeAuto {
		t.Errorf("DiscoveryModeValue() on an unset mode = %q, want auto", got)
	}
}

func TestLoad_InvalidModeNamesTheValue(t *testing.T) {
	t.Parallel()
	_, err := mustLoadDiscovery(t, discoveryLoadJSON(`, "api_endpoint_discovery": "sometimes"`))
	if err == nil {
		t.Fatal("Load accepted an invalid api_endpoint_discovery")
	}
	if !strings.Contains(err.Error(), `api_endpoint_discovery must be auto, on, or off, got "sometimes"`) {
		t.Errorf("error %q lacks the mode message", err)
	}
}

func TestDiscoveryModeValue(t *testing.T) {
	t.Parallel()
	var nilCfg *CPIConfig
	if got := nilCfg.DiscoveryModeValue(); got != DiscoveryModeAuto {
		t.Errorf("nil config = %q, want auto", got)
	}
	for _, m := range []DiscoveryMode{DiscoveryModeAuto, DiscoveryModeOn, DiscoveryModeOff, "bogus"} {
		c := &CPIConfig{APIEndpointDiscovery: m}
		if got := c.DiscoveryModeValue(); got != m {
			t.Errorf("DiscoveryModeValue() = %q, want %q", got, m)
		}
	}
}

// TestEffectiveDiscoveryMode drives every row of the mode table that needs
// only the config, in table order, and checks the reason text of each.
func TestEffectiveDiscoveryMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		build      func() *CPIConfig
		want       DiscoveryMode
		wantReason string // substring; empty means the row logs nothing
	}{
		{
			name: "row 1: auto with credentials inherited under looser TLS is off",
			build: func() *CPIConfig {
				c := discoveryBaseCfg()
				c.inheritedCredsLooserTLS = true
				return c
			},
			want:       DiscoveryModeOff,
			wantReason: "inherits the job's credentials and sets looser TLS than the job",
		},
		{
			name: "row 1: unset mode with credentials inherited under looser TLS is off",
			build: func() *CPIConfig {
				c := discoveryBaseCfg()
				c.APIEndpointDiscovery = ""
				c.inheritedCredsLooserTLS = true
				return c
			},
			want:       DiscoveryModeOff,
			wantReason: "looser TLS",
		},
		{
			name: "row 1: on with credentials inherited under looser TLS is off for a caller that skipped validation",
			build: func() *CPIConfig {
				c := discoveryBaseCfg()
				c.APIEndpointDiscovery = DiscoveryModeOn
				c.inheritedCredsLooserTLS = true
				return c
			},
			want:       DiscoveryModeOff,
			wantReason: "looser TLS",
		},
		{
			name: "row 1: off with credentials inherited under looser TLS keeps the looser TLS reason",
			build: func() *CPIConfig {
				c := discoveryBaseCfg()
				c.APIEndpointDiscovery = DiscoveryModeOff
				c.inheritedCredsLooserTLS = true
				return c
			},
			want:       DiscoveryModeOff,
			wantReason: "looser TLS",
		},
		{
			name: "row 2: off is off and silent",
			build: func() *CPIConfig {
				c := discoveryBaseCfg()
				c.APIEndpointDiscovery = DiscoveryModeOff
				return c
			},
			want: DiscoveryModeOff,
		},
		{
			name: "row 2: off with a password and several endpoints is off and silent",
			build: func() *CPIConfig {
				c := discoveryPasswordCfg()
				c.APIEndpointDiscovery = DiscoveryModeOff
				c.Endpoints = []string{"a:8006", "b:8006"}
				return c
			},
			want: DiscoveryModeOff,
		},
		{
			name: "row 3: on with a password and verify_ssl false is off, with the validation message",
			build: func() *CPIConfig {
				c := discoveryPasswordCfg()
				c.APIEndpointDiscovery = DiscoveryModeOn
				c.VerifySSL = discoveryBoolPtr(false)
				return c
			},
			want:       DiscoveryModeOff,
			wantReason: "cannot be used with password auth while verify_ssl is false",
		},
		{
			name: "row 4: auto with several endpoints is off and silent",
			build: func() *CPIConfig {
				c := discoveryBaseCfg()
				c.APIEndpointDiscovery = DiscoveryModeAuto
				c.Endpoints = []string{"a:8006", "b:8006"}
				return c
			},
			want: DiscoveryModeOff,
		},
		{
			name: "row 4: unset mode with several endpoints is off and silent",
			build: func() *CPIConfig {
				c := discoveryBaseCfg()
				c.Endpoints = []string{"a:8006", "b:8006", "c:8006"}
				return c
			},
			want: DiscoveryModeOff,
		},
		{
			name: "row 4 comes before row 5: auto with a password and several endpoints stays silent",
			build: func() *CPIConfig {
				c := discoveryPasswordCfg()
				c.Endpoints = []string{"a:8006", "b:8006"}
				return c
			},
			want: DiscoveryModeOff,
		},
		{
			name: "row 5: auto with a password is off, with a reason",
			build: func() *CPIConfig {
				c := discoveryPasswordCfg()
				c.APIEndpointDiscovery = DiscoveryModeAuto
				return c
			},
			want:       DiscoveryModeOff,
			wantReason: "api_endpoint_discovery is auto and the CPI logs in with a password",
		},
		{
			name:       "row 5: unset mode with a password is off, with a reason",
			build:      discoveryPasswordCfg,
			want:       DiscoveryModeOff,
			wantReason: "set pve.api_endpoint_discovery to on to discover with a password",
		},
		{
			name: "row 5: auto with a password and verify_ssl false is off, with the password reason",
			build: func() *CPIConfig {
				c := discoveryPasswordCfg()
				c.VerifySSL = discoveryBoolPtr(false)
				return c
			},
			want:       DiscoveryModeOff,
			wantReason: "logs in with a password",
		},
		{
			name: "row 7: auto with one endpoint and a token is on",
			build: func() *CPIConfig {
				c := discoveryBaseCfg()
				c.APIEndpointDiscovery = DiscoveryModeAuto
				return c
			},
			want: DiscoveryModeOn,
		},
		{
			name:  "row 7: unset mode with one endpoint and a token is on",
			build: discoveryBaseCfg,
			want:  DiscoveryModeOn,
		},
		{
			name: "row 7: auto with a one-entry list and a token is on",
			build: func() *CPIConfig {
				c := discoveryBaseCfg()
				c.Endpoints = []string{"pve1.example.com:8006"}
				return c
			},
			want: DiscoveryModeOn,
		},
		{
			name: "row 7: auto with one endpoint and a token under verify_ssl false is on",
			build: func() *CPIConfig {
				c := discoveryBaseCfg()
				c.VerifySSL = discoveryBoolPtr(false)
				return c
			},
			want: DiscoveryModeOn,
		},
		{
			name: "row 7: on with several endpoints is on",
			build: func() *CPIConfig {
				c := discoveryBaseCfg()
				c.APIEndpointDiscovery = DiscoveryModeOn
				c.Endpoints = []string{"a:8006", "b:8006"}
				return c
			},
			want: DiscoveryModeOn,
		},
		{
			name: "row 7: on with a password and verify_ssl true is on",
			build: func() *CPIConfig {
				c := discoveryPasswordCfg()
				c.APIEndpointDiscovery = DiscoveryModeOn
				return c
			},
			want: DiscoveryModeOn,
		},
		{
			name: "row 7: on with a token and verify_ssl false is on",
			build: func() *CPIConfig {
				c := discoveryBaseCfg()
				c.APIEndpointDiscovery = DiscoveryModeOn
				c.VerifySSL = discoveryBoolPtr(false)
				return c
			},
			want: DiscoveryModeOn,
		},
		{
			name: "token wins when a hand-built config holds both credentials",
			build: func() *CPIConfig {
				c := discoveryBaseCfg()
				c.Password = "secret"
				c.APIEndpointDiscovery = DiscoveryModeOn
				c.VerifySSL = discoveryBoolPtr(false)
				return c
			},
			want: DiscoveryModeOn,
		},
		{
			name: "an invalid mode fails closed",
			build: func() *CPIConfig {
				c := discoveryBaseCfg()
				c.APIEndpointDiscovery = "sometimes"
				return c
			},
			want:       DiscoveryModeOff,
			wantReason: `api_endpoint_discovery "sometimes" is not auto, on, or off`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, reason := EffectiveDiscoveryMode(tc.build())
			if got != tc.want {
				t.Errorf("mode = %q, want %q (reason %q)", got, tc.want, reason)
			}
			if tc.wantReason == "" {
				if reason != "" {
					t.Errorf("reason = %q, want none", reason)
				}
				return
			}
			if !strings.Contains(reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", reason, tc.wantReason)
			}
		})
	}
}

func TestEffectiveDiscoveryMode_NilConfigIsOff(t *testing.T) {
	t.Parallel()
	got, reason := EffectiveDiscoveryMode(nil)
	if got != DiscoveryModeOff || reason != "" {
		t.Errorf("EffectiveDiscoveryMode(nil) = %q, %q; want off and no reason", got, reason)
	}
}

// discoverySweepCfg builds the config for one point of the sweep below.
func discoverySweepCfg(mode DiscoveryMode, password, verify bool, count int, looser bool) *CPIConfig {
	c := discoveryBaseCfg()
	if password {
		c.APIToken, c.Password = "", "secret"
	}
	c.VerifySSL = discoveryBoolPtr(verify)
	c.APIEndpointDiscovery = mode
	c.Endpoints = nil
	for i := 0; i < count; i++ {
		c.Endpoints = append(c.Endpoints, "h"+string(rune('a'+i))+":8006")
	}
	if count > 0 {
		c.Host, c.Port = "ha", 8006
	}
	c.inheritedCredsLooserTLS = looser
	return c
}

// TestEffectiveDiscoveryMode_NeverAnswersAuto sweeps every combination the
// table keys on. The answer is always on or off, and a combination that
// Validate rejects never resolves to on.
func TestEffectiveDiscoveryMode_NeverAnswersAuto(t *testing.T) {
	t.Parallel()
	bools := []bool{false, true}
	for _, mode := range []DiscoveryMode{"", DiscoveryModeAuto, DiscoveryModeOn, DiscoveryModeOff, "bogus"} {
		for _, password := range bools {
			for _, verify := range bools {
				for _, count := range []int{0, 1, 2} {
					for _, looser := range bools {
						c := discoverySweepCfg(mode, password, verify, count, looser)
						got, _ := EffectiveDiscoveryMode(c)
						if got != DiscoveryModeOn && got != DiscoveryModeOff {
							t.Errorf("mode %q password %v verify %v endpoints %d looser %v resolved to %q",
								mode, password, verify, count, looser, got)
						}
						if got == DiscoveryModeOn && discoveryErrorOf(c) != "" {
							t.Errorf("mode %q password %v verify %v endpoints %d looser %v is invalid yet resolves to on",
								mode, password, verify, count, looser)
						}
					}
				}
			}
		}
	}
}

func TestValidateDiscoveryMode_Accepts(t *testing.T) {
	t.Parallel()
	for _, mode := range []DiscoveryMode{"", DiscoveryModeAuto, DiscoveryModeOn, DiscoveryModeOff} {
		c := discoveryPasswordCfg()
		c.APIEndpointDiscovery = mode
		if msg := discoveryErrorOf(c); msg != "" {
			t.Errorf("mode %q rejected: %s", mode, msg)
		}
	}
	// On with a password is allowed while verify_ssl is true, and on with a
	// token is allowed whatever verify_ssl says.
	c := discoveryBaseCfg()
	c.APIEndpointDiscovery = DiscoveryModeOn
	c.VerifySSL = discoveryBoolPtr(false)
	if msg := discoveryErrorOf(c); msg != "" {
		t.Errorf("on with a token and verify_ssl false rejected: %s", msg)
	}
	// auto and off with a password and verify_ssl false stay valid.
	for _, mode := range []DiscoveryMode{DiscoveryModeAuto, DiscoveryModeOff} {
		c := discoveryPasswordCfg()
		c.APIEndpointDiscovery = mode
		c.VerifySSL = discoveryBoolPtr(false)
		if msg := discoveryErrorOf(c); msg != "" {
			t.Errorf("%s with a password and verify_ssl false rejected: %s", mode, msg)
		}
	}
}

func TestValidateDiscoveryMode_RejectsBadValues(t *testing.T) {
	t.Parallel()
	for _, bad := range []DiscoveryMode{"ON", "Auto", "true", "yes", " on", "on ", "sometimes", "enabled"} {
		c := discoveryBaseCfg()
		c.APIEndpointDiscovery = bad
		want := `api_endpoint_discovery must be auto, on, or off, got "` + string(bad) + `"`
		if msg := discoveryErrorOf(c); !strings.Contains(msg, want) {
			t.Errorf("mode %q: error %q does not contain %q", bad, msg, want)
		}
	}
}

func TestValidateDiscoveryMode_OnWithPasswordAndNoVerificationIsRejected(t *testing.T) {
	t.Parallel()
	c := discoveryPasswordCfg()
	c.APIEndpointDiscovery = DiscoveryModeOn
	c.VerifySSL = discoveryBoolPtr(false)
	msg := discoveryErrorOf(c)
	for _, want := range []string{
		`api_endpoint_discovery "on" cannot be used with password auth while verify_ssl is false`,
		"use an API token (pve.api_token)",
		"turn on verification (pve.verify_ssl: true)",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "secret") {
		t.Errorf("error %q echoes the password", msg)
	}

	// The same rule through Load, with the boolean spelling of on.
	_, err := mustLoadDiscovery(t, discoveryLoadJSON(`, "verify_ssl": false, "api_endpoint_discovery": true`))
	if err == nil || !strings.Contains(err.Error(), "cannot be used with password auth while verify_ssl is false") {
		t.Errorf("Load error = %v, want the password and verify_ssl message", err)
	}
}

func TestValidateDiscoveryMode_OnWithInheritedCredentialsUnderLooserTLSIsRejected(t *testing.T) {
	t.Parallel()
	c := discoveryBaseCfg()
	c.APIEndpointDiscovery = DiscoveryModeOn
	c.inheritedCredsLooserTLS = true
	msg := discoveryErrorOf(c)
	for _, want := range []string{
		`api_endpoint_discovery "on" cannot be used because this entry inherits the job's credentials`,
		"give the entry its own credentials",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
	// auto and off are not errors; auto simply resolves to off.
	for _, mode := range []DiscoveryMode{"", DiscoveryModeAuto, DiscoveryModeOff} {
		c := discoveryBaseCfg()
		c.APIEndpointDiscovery = mode
		c.inheritedCredsLooserTLS = true
		if msg := discoveryErrorOf(c); msg != "" {
			t.Errorf("mode %q with the looser TLS flag rejected: %s", mode, msg)
		}
	}
}

func TestInheritedCredsLooserTLS_IsPrivateAndSurvivesACopy(t *testing.T) {
	t.Parallel()
	var nilCfg *CPIConfig
	if nilCfg.InheritedCredsLooserTLS() {
		t.Error("nil config reports the flag")
	}
	c := discoveryBaseCfg()
	if c.InheritedCredsLooserTLS() {
		t.Error("a fresh config reports the flag")
	}
	c.inheritedCredsLooserTLS = true
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(strings.ToLower(string(out)), "looser") {
		t.Errorf("the flag leaks into cpi.json: %s", out)
	}
	cp := *c
	if !cp.InheritedCredsLooserTLS() {
		t.Error("the flag does not survive a shallow copy")
	}
	// A cpi.json cannot set it: the key is not a known field.
	if _, known := knownConfigFields["inherited_creds_looser_tls"]; known {
		t.Error("the flag is a known config field")
	}
	loaded, err := mustLoadDiscovery(t, discoveryLoadJSON(`, "inheritedCredsLooserTLS": true, "inherited_creds_looser_tls": true`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.InheritedCredsLooserTLS() {
		t.Error("cpi.json set the flag")
	}
}

func TestValidateEndpointCacheDir_Accepts(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, dir, journal string }{
		{"no cache directory", "", ""},
		{"absolute and clean with no journal", "/var/vcap/store/pve_cpi/endpoint-cache", ""},
		{"beside the journal", "/var/vcap/store/pve_cpi/endpoint-cache", "/var/vcap/store/pve_cpi/journal"},
		{"sibling sharing a name prefix", "/var/vcap/store/journal-cache", "/var/vcap/store/journal"},
		{"journal sharing a name prefix", "/var/vcap/store/journal/x", "/var/vcap/store/jour"},
		{"parent of the journal", "/var/vcap/store", "/var/vcap/store/journal"},
		{"unrelated tree", "/tmp/pve-cache", "/var/vcap/store/journal"},
		{"journal that is not absolute is left to its own check", "/tmp/pve-cache", "journal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := discoveryBaseCfg()
			c.EndpointCacheDir = tc.dir
			c.StorageAllocationJournalDir = tc.journal
			msg := discoveryErrorOf(c)
			if strings.Contains(msg, "endpoint_cache_dir") {
				t.Errorf("rejected %q with journal %q: %s", tc.dir, tc.journal, msg)
			}
		})
	}
}

func TestValidateEndpointCacheDir_RejectsBadPaths(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		dir     string
		journal string
		want    string
	}{
		{"relative", "cache", "", `endpoint_cache_dir must be an absolute path, got "cache"`},
		{"dot relative", "./cache", "", `endpoint_cache_dir must be an absolute path, got "./cache"`},
		{"blank", "   ", "", `endpoint_cache_dir must be an absolute path, got "   "`},
		{"tilde", "~/cache", "", `endpoint_cache_dir must be an absolute path, got "~/cache"`},
		{"trailing slash", "/var/cache/pve/", "", `must be a clean path, write "/var/cache/pve" instead of "/var/cache/pve/"`},
		{"doubled slash", "/var//cache", "", `must be a clean path, write "/var/cache" instead of "/var//cache"`},
		{"dot segment", "/var/./cache", "", `must be a clean path, write "/var/cache" instead of "/var/./cache"`},
		{"parent segment", "/var/vcap/../cache", "", `must be a clean path, write "/var/cache" instead of "/var/vcap/../cache"`},
		{"newline", "/var/cache\n/x", "", "endpoint_cache_dir must not contain control characters"},
		{"NUL byte", "/var/cache\x00", "", "endpoint_cache_dir must not contain control characters"},
		{"equal to the journal", "/var/vcap/store/journal", "/var/vcap/store/journal",
			`endpoint_cache_dir "/var/vcap/store/journal" must not equal or sit inside storage_allocation_journal_dir`},
		{"inside the journal", "/var/vcap/store/journal/endpoint-cache", "/var/vcap/store/journal",
			"must not equal or sit inside storage_allocation_journal_dir"},
		{"deep inside the journal", "/var/vcap/store/journal/a/b/c", "/var/vcap/store/journal",
			"must not equal or sit inside storage_allocation_journal_dir"},
		{"equal to an unclean journal path", "/var/vcap/store/journal", "/var/vcap/store/journal/",
			"must not equal or sit inside storage_allocation_journal_dir"},
		{"inside an unclean journal path", "/var/vcap/store/journal/c", "/var/vcap/store//journal/../journal",
			"must not equal or sit inside storage_allocation_journal_dir"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := discoveryBaseCfg()
			c.EndpointCacheDir = tc.dir
			c.StorageAllocationJournalDir = tc.journal
			msg := discoveryErrorOf(c)
			if !strings.Contains(msg, tc.want) {
				t.Errorf("error %q does not contain %q", msg, tc.want)
			}
		})
	}
}

func TestValidateEndpointCacheDir_RootIsAllowedAndFlagsOnlyTheJournal(t *testing.T) {
	t.Parallel()
	c := discoveryBaseCfg()
	c.EndpointCacheDir = "/"
	if msg := discoveryErrorOf(c); strings.Contains(msg, "endpoint_cache_dir") {
		t.Errorf("the root directory is absolute and clean, yet was rejected: %s", msg)
	}
	c.StorageAllocationJournalDir = "/"
	if msg := discoveryErrorOf(c); !strings.Contains(msg, "must not equal or sit inside") {
		t.Errorf("a journal at the root holds every path, yet %q was accepted: %s", c.EndpointCacheDir, msg)
	}
}

func TestPathIsWithin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		root, child string
		want        bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b", "/a/b/c", true},
		{"/a/b", "/a/b/c/d", true},
		{"/a/b", "/a/bc", false},
		{"/a/b", "/a", false},
		{"/a/b", "/", false},
		{"/a/b", "/x/y", false},
		{"/", "/a", true},
		{"/", "/", true},
	}
	for _, tc := range cases {
		if got := pathIsWithin(tc.root, tc.child); got != tc.want {
			t.Errorf("pathIsWithin(%q, %q) = %v, want %v", tc.root, tc.child, got, tc.want)
		}
	}
}

func TestLoad_RejectsABadCacheDirectory(t *testing.T) {
	t.Parallel()
	_, err := mustLoadDiscovery(t, discoveryLoadJSON(`, "endpoint_cache_dir": "relative/cache"`))
	if err == nil || !strings.Contains(err.Error(), "endpoint_cache_dir must be an absolute path") {
		t.Errorf("Load error = %v, want the absolute path message", err)
	}
	_, err = mustLoadDiscovery(t, discoveryLoadJSON(`,
		"storage_allocation_journal_dir": "/var/vcap/store/journal",
		"endpoint_cache_dir": "/var/vcap/store/journal/cache"`))
	if err == nil || !strings.Contains(err.Error(), "must not equal or sit inside storage_allocation_journal_dir") {
		t.Errorf("Load error = %v, want the journal message", err)
	}
}

func TestValidateDiscovery_ReportsEveryProblemInOnePass(t *testing.T) {
	t.Parallel()
	c := discoveryPasswordCfg()
	c.APIEndpointDiscovery = DiscoveryModeOn
	c.VerifySSL = discoveryBoolPtr(false)
	c.EndpointCacheDir = "relative"
	msg := discoveryErrorOf(c)
	for _, want := range []string{"cannot be used with password auth", "endpoint_cache_dir must be an absolute path"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
}

// TestDiscoveryNames_UseTheSpecName checks the property names the config
// reads and writes, and that none of them is a retired key.
func TestDiscoveryNames_UseTheSpecName(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"api_endpoint_discovery", "endpoint_cache_dir"} {
		if _, known := knownConfigFields[name]; !known {
			t.Errorf("%s is not a known config field", name)
		}
		for _, k := range removedKeys {
			if strings.HasSuffix(k.specName(), name) {
				t.Errorf("%s collides with the retired key %s", name, k.specName())
			}
		}
	}
	// The name the design replaced must not be accepted by the struct.
	if _, known := knownConfigFields["endpoint_discovery"]; known {
		t.Error("endpoint_discovery is a known config field; the property is api_endpoint_discovery")
	}
	for _, c := range []string{discoveryErrLooserTLS, discoveryErrPasswordNoVerify, discoveryReasonPassword} {
		if strings.Contains(c, "pve.endpoint_discovery") || strings.Contains(c, " endpoint_discovery ") {
			t.Errorf("message %q uses a name other than api_endpoint_discovery", c)
		}
	}
}
