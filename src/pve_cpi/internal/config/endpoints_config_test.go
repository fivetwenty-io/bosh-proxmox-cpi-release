package config_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
)

// endpointsLoadJSON renders a minimal valid cpi.json whose pve fields are
// replaced by extra, a JSON object fragment such as `"host": "a", "port": 9000`.
func endpointsLoadJSON(extra string) string {
	return `{
		"user": "root@pam",
		"password": "secret",
		"vm_storage": "local-lvm",
		"disk_storage": "local-lvm",
		"network_bridge": "vmbr0",
		` + extra + `
	}`
}

func TestEndpoints_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	cfg, err := mustLoad(t, endpointsLoadJSON(`"endpoints": ["pve1.example.com:8006", "pve2.example.com:8006"]`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := []string{"pve1.example.com:8006", "pve2.example.com:8006"}
	if !reflect.DeepEqual(cfg.Endpoints, want) {
		t.Errorf("Endpoints = %v, want %v", cfg.Endpoints, want)
	}
	out, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"endpoints":["pve1.example.com:8006","pve2.example.com:8006"]`) {
		t.Errorf("marshaled config does not carry the list: %s", out)
	}
}

func TestEndpoints_UnsetIsOmittedAndValid(t *testing.T) {
	t.Parallel()
	c := baseValidCfg()
	if err := c.Validate(); err != nil {
		t.Fatalf("base config invalid: %v", err)
	}
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), `"endpoints"`) {
		t.Errorf("a config without a list marshals an endpoints key: %s", out)
	}
}

func TestValidateEndpoints_AcceptsGoodValues(t *testing.T) {
	t.Parallel()
	c := baseValidCfg()
	c.Host = ""
	c.Endpoints = []string{
		"pve1.example.com",
		"10.0.0.12:8007",
		"10.0.0.13",
		"2001:db8::6",
		"[2001:db8::7]:8006",
	}
	if err := c.Validate(); err != nil {
		t.Errorf("valid entries rejected: %v", err)
	}
}

func TestValidateEndpoints_RejectsBadValues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		entry   string
		wantSub string
	}{
		{"empty", "", "endpoints[1] must not be empty"},
		{"blank", "   ", "endpoints[1] must not be empty"},
		{"scheme", "https://pve2.example.com", "endpoints[1] must be host or host:port without a scheme"},
		{"path", "pve2.example.com/api2", "endpoints[1] must be host or host:port without a path"},
		{"port too large", "pve2.example.com:99999", `endpoints[1] port must be 1-65535, got "99999"`},
		{"port zero", "pve2.example.com:0", `endpoints[1] port must be 1-65535, got "0"`},
		{"non-numeric port", "pve2.example.com:abc", "endpoints[1] port must be 1-65535"},
		{"empty port", "pve2.example.com:", "endpoints[1] port must be 1-65535"},
		{"missing host", ":8006", "endpoints[1] must carry a host before the port"},
		{"padded leading", " pve2.example.com", "endpoints[1] value must not carry surrounding whitespace"},
		{"padded trailing", "pve2.example.com ", "endpoints[1] value must not carry surrounding whitespace"},
		{"inner whitespace", "pve2 .example.com", "no user info, query, or whitespace"},
		{"user info", "root@pve2.example.com", "no user info, query, or whitespace"},
		{"query", "pve2.example.com?x=1", "no user info, query, or whitespace"},
		{"fragment", "pve2.example.com#frag", "no user info, query, or whitespace"},
		{"backslash", `pve2.example.com\x`, "no user info, query, or whitespace"},
		{"signed port", "pve2.example.com:+80", `endpoints[1] port must be digits only, got "+80"`},
		{"comma joined", "a,b", "endpoints[1] host must hold only letters, digits"},
		{"leading hyphen", "-a", "endpoints[1] host must hold only letters, digits"},
		{"bracketed non-address", "[abc]:8006", "endpoints[1] host must hold only letters, digits"},
		{"bracketed without port", "[fd00::1]", "endpoints[1] bracketed address must be [host]:port"},
		{"double port typo", "pve2:8006:8006", "endpoints[1] must be host, host:port, or a bare IPv6 literal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := baseValidCfg()
			c.Host = ""
			c.Endpoints = []string{"pve1.example.com", tc.entry}
			err := c.Validate()
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestValidateEndpoints_EntryCap(t *testing.T) {
	t.Parallel()
	build := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("pve%d.example.com", i+1)
		}
		return out
	}

	atCap := baseValidCfg()
	atCap.Host = ""
	atCap.Endpoints = build(16)
	if err := atCap.Validate(); err != nil {
		t.Errorf("16 entries rejected: %v", err)
	}

	over := baseValidCfg()
	over.Host = ""
	over.Endpoints = build(17)
	err := over.Validate()
	if err == nil {
		t.Fatal("17 entries accepted")
	}
	if !strings.Contains(err.Error(), "endpoints holds 17 entries, at most 16 are allowed") {
		t.Errorf("error %q does not name the cap", err.Error())
	}
}

func TestValidateEndpoints_RejectsNormalizedDuplicates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		port    int
		entries []string
	}{
		{"exact", 0, []string{"a.example.com:8006", "a.example.com:8006"}},
		{"case", 0, []string{"a.example.com", "A.Example.COM"}},
		{"trailing dot", 0, []string{"a.example.com", "a.example.com."}},
		{"bare and default port", 0, []string{"a.example.com", "a.example.com:8006"}},
		{"bare and configured port", 9000, []string{"a.example.com:9000", "a.example.com"}},
		{"ipv6 spellings", 0, []string{"2001:db8::1", "[2001:0DB8:0:0:0:0:0:1]:8006"}},
		{"later duplicate", 0, []string{"a.example.com", "b.example.com", "B.example.com"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := baseValidCfg()
			c.Host = ""
			c.Port = tc.port
			c.Endpoints = tc.entries
			err := c.Validate()
			if err == nil {
				t.Fatal("duplicate accepted")
			}
			if !strings.Contains(err.Error(), "repeats endpoints[") {
				t.Errorf("error %q does not report the duplicate", err.Error())
			}
		})
	}
}

func TestValidateEndpoints_DistinctPortsAreNotDuplicates(t *testing.T) {
	t.Parallel()
	c := baseValidCfg()
	c.Host = ""
	c.Endpoints = []string{"a.example.com:8006", "a.example.com:8007"}
	if err := c.Validate(); err != nil {
		t.Errorf("same host on two ports rejected: %v", err)
	}
}

func TestLoad_EndpointPortFillIn(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		extra    string
		wantList []string
		wantHost string
		wantPort int
	}{
		{
			"pve.port unset uses 8006",
			`"endpoints": ["a.example.com", "b.example.com:9100"]`,
			[]string{"a.example.com:8006", "b.example.com:9100"},
			"a.example.com", 8006,
		},
		{
			"pve.port fills bare entries only",
			`"port": 8443, "endpoints": ["a.example.com", "b.example.com:9100", "10.0.0.3"]`,
			[]string{"a.example.com:8443", "b.example.com:9100", "10.0.0.3:8443"},
			"a.example.com", 8443,
		},
		{
			"first entry port wins over pve.port",
			`"port": 8006, "endpoints": ["a.example.com:9000", "b.example.com"]`,
			[]string{"a.example.com:9000", "b.example.com:8006"},
			"a.example.com", 9000,
		},
		{
			"bare ipv6 literal is bracketed",
			`"port": 8007, "endpoints": ["2001:db8::1", "[2001:db8::2]:8010"]`,
			[]string{"[2001:db8::1]:8007", "[2001:db8::2]:8010"},
			"2001:db8::1", 8007,
		},
		{
			"bracketed first entry gives a bare host",
			`"endpoints": ["[2001:db8::2]:8010", "b.example.com"]`,
			[]string{"[2001:db8::2]:8010", "b.example.com:8006"},
			"2001:db8::2", 8010,
		},
		{
			"host and matching list agree",
			`"host": "a.example.com", "port": 9000, "endpoints": ["a.example.com", "b.example.com"]`,
			[]string{"a.example.com:9000", "b.example.com:9000"},
			"a.example.com", 9000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := mustLoad(t, endpointsLoadJSON(tc.extra))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if !reflect.DeepEqual(cfg.Endpoints, tc.wantList) {
				t.Errorf("Endpoints = %v, want %v", cfg.Endpoints, tc.wantList)
			}
			if cfg.Host != tc.wantHost || cfg.Port != tc.wantPort {
				t.Errorf("Host, Port = %q, %d; want %q, %d", cfg.Host, cfg.Port, tc.wantHost, tc.wantPort)
			}
		})
	}
}

func TestLoad_EndpointPortFillInLeavesBadEntriesForValidation(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, endpointsLoadJSON(`"endpoints": ["a.example.com", "https://b.example.com"]`))
	assertCloudError(t, err, `endpoints[1] must be host or host:port without a scheme, got "https://b.example.com"`)

	_, err = mustLoad(t, endpointsLoadJSON(`"endpoints": ["https://a.example.com", "b.example.com"]`))
	assertCloudError(t, err, "endpoints[0] must be host or host:port without a scheme")
}

func TestLoad_EndpointsWithOutOfRangePortReportPortOnce(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, endpointsLoadJSON(`"port": 70000, "endpoints": ["a.example.com"]`))
	if err == nil {
		t.Fatal("out-of-range pve.port accepted")
	}
	if strings.Contains(err.Error(), "endpoints[0]") {
		t.Errorf("error %q blames the endpoint for the port", err.Error())
	}
}

func TestApplyDefaults_AdoptsFirstEndpoint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		host     string
		port     int
		entries  []string
		wantHost string
		wantPort int
	}{
		{"empty host takes first entry", "", 0, []string{"a.example.com:9000", "b.example.com"}, "a.example.com", 9000},
		{"first entry port beats configured port", "", 8443, []string{"a.example.com:9000"}, "a.example.com", 9000},
		{"bare first entry keeps configured port", "", 8443, []string{"a.example.com"}, "a.example.com", 8443},
		{"bare first entry and no port defaults", "", 0, []string{"a.example.com"}, "a.example.com", 8006},
		{"host already set is kept", "keep.example.com", 0, []string{"a.example.com:9000"}, "keep.example.com", 8006},
		{"malformed first entry leaves host empty", "", 0, []string{"https://a.example.com"}, "", 8006},
		{"no list leaves host empty", "", 0, nil, "", 8006},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &config.CPIConfig{Host: tc.host, Port: tc.port, Endpoints: tc.entries}
			c.ApplyDefaults()
			if c.Host != tc.wantHost || c.Port != tc.wantPort {
				t.Errorf("Host, Port = %q, %d; want %q, %d", c.Host, c.Port, tc.wantHost, tc.wantPort)
			}
		})
	}
}

func TestValidateEndpoints_HostAgreement(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		host    string
		port    int
		entries []string
		wantSub string // empty means valid
	}{
		{"same host and port", "a.example.com", 9000, []string{"a.example.com:9000", "b"}, ""},
		{"case and trailing dot", "A.Example.com.", 9000, []string{"a.example.com:9000"}, ""},
		{"explicit default port", "a.example.com", 8006, []string{"a.example.com:8006"}, ""},
		{"ipv6 literal", "2001:db8::1", 8006, []string{"[2001:DB8::1]:8006"}, ""},
		{
			"port differs", "a.example.com", 0, []string{"a.example.com:9000"},
			`pve.host "a.example.com" with port 8006 does not match the first entry of pve.endpoints ` +
				`(a.example.com:9000); set pve.port: 9000, or drop pve.host`,
		},
		{
			"host differs", "a.example.com", 8006, []string{"b.example.com:8006"},
			`pve.host "a.example.com" with port 8006 does not match the first entry of pve.endpoints ` +
				`(b.example.com:8006); set pve.host to the first endpoint, or drop pve.host`,
		},
		{
			"host names a later entry", "b.example.com", 8006, []string{"a.example.com", "b.example.com"},
			"does not match the first entry of pve.endpoints (a.example.com:8006)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := baseValidCfg()
			c.Host = tc.host
			c.Port = tc.port
			c.Endpoints = tc.entries
			err := c.Validate()
			if tc.wantSub == "" {
				if err != nil {
					t.Errorf("agreeing host rejected: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("disagreeing host accepted")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestLoad_HostAgreementThroughJSON drives the documented failing example end
// to end: pve.host takes the default port 8006, so a first entry on 9000
// disagrees until pve.port or pve.host changes.
func TestLoad_HostAgreementThroughJSON(t *testing.T) {
	t.Parallel()
	_, err := mustLoad(t, endpointsLoadJSON(`"host": "a", "endpoints": ["a:9000"]`))
	assertCloudError(t, err, "set pve.port: 9000, or drop pve.host")

	cfg, err := mustLoad(t, endpointsLoadJSON(`"host": "a", "port": 9000, "endpoints": ["a:9000"]`))
	if err != nil {
		t.Fatalf("pve.port 9000 should agree: %v", err)
	}
	if cfg.Host != "a" || cfg.Port != 9000 {
		t.Errorf("Host, Port = %q, %d; want a, 9000", cfg.Host, cfg.Port)
	}

	cfg, err = mustLoad(t, endpointsLoadJSON(`"endpoints": ["a:9000"]`))
	if err != nil {
		t.Fatalf("dropping pve.host should load: %v", err)
	}
	if cfg.Host != "a" || cfg.Port != 9000 {
		t.Errorf("Host, Port = %q, %d; want a, 9000", cfg.Host, cfg.Port)
	}
}

func TestValidateRequiredHost_EitherPropertySupplies(t *testing.T) {
	t.Parallel()

	neither := baseValidCfg()
	neither.Host = ""
	err := neither.Validate()
	assertCloudError(t, err, "host is required")
	if !strings.Contains(err.Error(), "pve.host") || !strings.Contains(err.Error(), "pve.endpoints") {
		t.Errorf("error %q does not name both properties", err.Error())
	}

	viaList := baseValidCfg()
	viaList.Host = ""
	viaList.Endpoints = []string{"a.example.com"}
	if err := viaList.Validate(); err != nil {
		t.Errorf("a list in place of pve.host rejected: %v", err)
	}

	_, err = mustLoad(t, endpointsLoadJSON(`"endpoints": []`))
	assertCloudError(t, err, "host is required")
}

// TestValidateEndpoints_ReportsEveryProblem proves one pass reports the cap,
// a bad entry, and a duplicate together, matching how Validate reports the
// rest of the config.
func TestValidateEndpoints_ReportsEveryProblem(t *testing.T) {
	t.Parallel()
	c := baseValidCfg()
	c.Host = ""
	entries := []string{"https://bad.example.com", "a.example.com", "A.example.com"}
	for i := 0; i < 15; i++ {
		entries = append(entries, fmt.Sprintf("filler%d.example.com", i))
	}
	c.Endpoints = entries
	err := c.Validate()
	if err == nil {
		t.Fatal("invalid list accepted")
	}
	for _, want := range []string{"at most 16 are allowed", "endpoints[0] must be host or host:port without a scheme", "repeats endpoints[1]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err.Error(), want)
		}
	}
}

// TestApplyDefaults_FillsEndpointPortsLikeLoad pins that a config built by
// hand validates the same list the same way Load does: the bare "b" takes the
// default port before the first entry's port replaces Port, so "b:9000" is a
// different endpoint from "b".
func TestApplyDefaults_FillsEndpointPortsLikeLoad(t *testing.T) {
	t.Parallel()
	list := []string{"a.example.com:9000", "b.example.com", "b.example.com:9000"}

	byHand := baseValidCfg()
	byHand.Host = ""
	byHand.Port = 0
	byHand.Endpoints = append([]string(nil), list...)
	byHand.ApplyDefaults()
	if err := byHand.Validate(); err != nil {
		t.Fatalf("hand-built config rejected: %v", err)
	}
	if byHand.Endpoints[1] != "b.example.com:8006" {
		t.Errorf("Endpoints[1] = %q, want b.example.com:8006", byHand.Endpoints[1])
	}

	loaded, err := mustLoad(t, endpointsLoadJSON(`"endpoints": ["a.example.com:9000", "b.example.com", "b.example.com:9000"]`))
	if err != nil {
		t.Fatalf("Load rejected the list: %v", err)
	}
	if !reflect.DeepEqual(loaded.Endpoints, byHand.Endpoints) {
		t.Errorf("Load Endpoints = %v, ApplyDefaults Endpoints = %v", loaded.Endpoints, byHand.Endpoints)
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		addr     string
		wantHost string
		wantPort int
		wantOK   bool
	}{
		{"PVE1.Example.COM", "pve1.example.com", 8006, true},
		{"pve1.example.com.:9000", "pve1.example.com", 9000, true},
		{"[FD00::1]:8006", "fd00::1", 8006, true},
		{"FD00:0:0::1", "fd00::1", 8006, true},
		{"10.0.0.5", "10.0.0.5", 8006, true},
		{"", "", 0, false},
		{".", "", 0, false},
		{".:8006", "", 0, false},
		{"[.]:8006", "", 0, false},
		{"https://a", "", 0, false},
		{"a:+80", "", 0, false},
		{"a,b", "", 0, false},
		{"root@a", "", 0, false},
	}
	for _, tc := range cases {
		host, port, ok := config.NormalizeEndpoint(tc.addr)
		if host != tc.wantHost || port != tc.wantPort || ok != tc.wantOK {
			t.Errorf("NormalizeEndpoint(%q) = %q, %d, %v; want %q, %d, %v",
				tc.addr, host, port, ok, tc.wantHost, tc.wantPort, tc.wantOK)
		}
	}
}

// TestNormalizeEndpoint_AgreesWithSameEndpoint pins that the list normalizer
// and the credential-inheritance comparison treat two IPv6 spellings alike.
func TestNormalizeEndpoint_AgreesWithSameEndpoint(t *testing.T) {
	t.Parallel()
	a := &config.CPIConfig{Host: "FD00::1", Port: 8006}
	b := &config.CPIConfig{Host: "fd00:0:0::1.", Port: 0}
	if !config.SameEndpoint(a, b) {
		t.Error("SameEndpoint treats two spellings of one IPv6 address as different endpoints")
	}
	ah, ap, _ := config.NormalizeEndpoint("[FD00::1]:8006")
	bh, bp, _ := config.NormalizeEndpoint("fd00:0:0::1")
	if ah != bh || ap != bp {
		t.Errorf("NormalizeEndpoint disagrees: %q:%d vs %q:%d", ah, ap, bh, bp)
	}
}
