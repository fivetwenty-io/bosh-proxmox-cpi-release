package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math"
	"math/big"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
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
// with the bounded echo. Each value goes through jsonrpc.Context first, as a
// request's does, so a number written as 5.0 reaches the override as written.
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
			_, _, _, err := ApplyContextOverrides(
				discoveryOverrideBase(),
				requestExtra(t, `{"pve_api_endpoint_discovery":`+string(raw)+`}`),
			)
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

// requestExtra decodes a request context the way the dispatcher does and
// returns its Extra map.
func requestExtra(t *testing.T, context string) map[string]any {
	t.Helper()
	var ctx jsonrpc.Context
	if err := json.Unmarshal([]byte(context), &ctx); err != nil {
		t.Fatalf("decode context %s: %v", context, err)
	}
	return ctx.Extra
}

// TestMarkInheritedCredsLooserTLS_RaisesTheFlagOnlyForAnEntryThatLoosensTLS
// asserts that an entry which names no TLS key leaves the flag down, and that
// one which loosens TLS under inherited credentials raises it.
func TestMarkInheritedCredsLooserTLS_RaisesTheFlagOnlyForAnEntryThatLoosensTLS(t *testing.T) {
	t.Parallel()
	clean := discoveryOverrideBase()
	eff := *clean
	markInheritedCredsLooserTLS(clean, &eff, map[string]any{"pve_vm_storage": "other"})
	if eff.inheritedCredsLooserTLS {
		t.Error("the flag was raised for an entry that changed no TLS setting")
	}

	eff = *clean
	eff.VerifySSL = new(bool)
	markInheritedCredsLooserTLS(clean, &eff, map[string]any{"pve_verify_ssl": false})
	if !eff.inheritedCredsLooserTLS {
		t.Error("the flag was not raised for an entry that turned verification off")
	}

	// An entry that names no TLS key is skipped before any comparison, even if
	// its config differs from the job's in a TLS field.
	eff = *clean
	eff.VerifySSL = new(bool)
	markInheritedCredsLooserTLS(clean, &eff, map[string]any{"pve_vm_storage": "other"})
	if eff.inheritedCredsLooserTLS {
		t.Error("the flag was raised for an entry that named no TLS key")
	}
}

// discoveryTestCA returns a freshly generated self-signed CA certificate as PEM.
func discoveryTestCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "discovery-test-ca"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// rewrapPEM writes the base64 body of a single-certificate PEM on lines of
// width characters.
func rewrapPEM(t *testing.T, certPEM string, width int) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(certPEM), "\n")
	body := strings.Join(lines[1:len(lines)-1], "")
	var out []string
	out = append(out, lines[0])
	for len(body) > width {
		out, body = append(out, body[:width]), body[width:]
	}
	out = append(out, body, lines[len(lines)-1])
	return strings.Join(out, "\n") + "\n"
}

// withPEMHeader writes the single certificate in certPEM with a header line,
// the way x509.CertPool.AppendCertsFromPEM refuses to load.
func withPEMHeader(t *testing.T, certPEM string) string {
	t.Helper()
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		t.Fatal("no PEM block to add a header to")
	}
	block.Headers = map[string]string{"X-Note": "kept for the audit"}
	return string(pem.EncodeToMemory(block))
}

// TestMarkInheritedCredsLooserTLS_ComparesCAsAsSetsOfCertificates asserts that
// an entry which restates the job's CA in another spelling is not looser,
// that one which loads a different set of roots is, and that a bundle which
// loads no roots fails closed. A block the client's pool skips, whether for
// its headers, its type, or DER that does not parse, is skipped here too.
func TestMarkInheritedCredsLooserTLS_ComparesCAsAsSetsOfCertificates(t *testing.T) {
	t.Parallel()
	one, two := discoveryTestCA(t), discoveryTestCA(t)
	bundle := one + two
	headed := withPEMHeader(t, two)
	notACert := "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"
	privateKey := strings.ReplaceAll(two, "CERTIFICATE", "PRIVATE KEY")
	for _, tc := range []struct {
		name   string
		jobCA  string
		entry  string
		looser bool
	}{
		{"the same bundle", bundle, bundle, false},
		{"surrounding whitespace", bundle, "\n\n" + bundle + "\n\n", false},
		{"CRLF line endings", bundle, strings.ReplaceAll(bundle, "\n", "\r\n"), false},
		{"a leading comment line", bundle, "# the issuing CA\n" + bundle, false},
		{"a comment between blocks", bundle, one + "# second root\n" + two, false},
		{"a duplicated block", bundle, bundle + one, false},
		{"a reordered bundle", bundle, two + one, false},
		{"different base64 wrapping", one, rewrapPEM(t, one, 40), false},
		{"a single certificate repeated", one, one + one, false},
		{"fewer roots than the job", bundle, one, true},
		{"more roots than the job", one, bundle, true},
		{"an unrelated certificate", one, two, true},
		{"the system pool in place of a CA", one, "", true},
		{"a CA where the job uses the system pool", "", one, true},
		{"both on the system pool", "", "", false},
		{"upper-cased text", one, strings.ToUpper(one), true},
		{"lower-cased text", one, strings.ToLower(one), true},
		{"text that holds no certificate", one, "not a certificate\n", true},
		{"a block of another type", one, strings.ReplaceAll(one, "CERTIFICATE", "PRIVATE KEY"), true},
		{"a certificate beside a block of another type in the entry", one, one + privateKey, false},
		{"a certificate beside a block of another type in the job", one + privateKey, one, false},
		{"a block of another type beside the second root", bundle, one + privateKey, true},
		{"a certificate beside bytes that are not a certificate", one, one + notACert, false},
		{"the same unparseable text on both sides", "not a certificate\n", "not a certificate\n", true},
		{"the same block of another type on both sides", privateKey, privateKey, true},
		{"a certificate block with bytes that are not a certificate", one, notACert, true},
		{"the same bytes that are not a certificate on both sides", notACert, notACert, true},
		{"whitespace in place of a CA on both sides", " \n", " \n", true},
		{"a header-bearing block in the job's CA, the entry trusting both", one + headed, bundle, true},
		{"a header-bearing block in the entry's CA, the job trusting both", bundle, one + headed, true},
		{"a header-bearing block on both sides", one + headed, one + headed, false},
		{"a header-bearing block beside the plain one", one + headed, one, false},
		{"only a header-bearing block in the job's CA", headed, two, true},
		{"only a header-bearing block in the entry's CA", two, headed, true},
		{"a header-bearing block alone on both sides", headed, headed, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := discoveryOverrideBase()
			base.PVECACertPEM = tc.jobCA
			eff := *base
			eff.PVECACertPEM = tc.entry
			markInheritedCredsLooserTLS(base, &eff, map[string]any{"pve_ca_cert": tc.entry})
			if eff.inheritedCredsLooserTLS != tc.looser {
				t.Errorf("flag = %v, want %v", eff.inheritedCredsLooserTLS, tc.looser)
			}
			if got := sameCASet(tc.jobCA, tc.entry); got == tc.looser {
				t.Errorf("sameCASet = %v, want %v", got, !tc.looser)
			}
		})
	}
}

// TestApplyContextOverrides_AHeaderBearingBlockDoesNotHideAnExtraRoot drives
// the case where the job's CA holds a block with headers, which the client
// does not load. The entry then trusts a root the job does not, so the entry
// has to count as looser and the mode on has to be refused, and an entry that
// leaves the headed block out of its own CA has to count the same way.
func TestApplyContextOverrides_AHeaderBearingBlockDoesNotHideAnExtraRoot(t *testing.T) {
	t.Parallel()
	one, two := discoveryTestCA(t), discoveryTestCA(t)
	headed := withPEMHeader(t, two)
	loaded := func(bundle string) *x509.CertPool {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM([]byte(bundle))
		return pool
	}
	for name, tc := range map[string]struct {
		jobCA, entryCA string
	}{
		"the job's CA holds the headed block":   {one + headed, one + two},
		"the entry's CA holds the headed block": {one + two, one + headed},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if loaded(tc.jobCA).Equal(loaded(tc.entryCA)) {
				t.Fatal("the two bundles load the same roots, want different roots for this case")
			}
			base := discoveryOverrideBase()
			base.PVECACertPEM = tc.jobCA
			_, _, _, err := ApplyContextOverrides(base, map[string]any{
				"pve_ca_cert": tc.entryCA, "pve_api_endpoint_discovery": "on",
			})
			if err == nil || !strings.Contains(err.Error(), "looser TLS than the job") {
				t.Errorf("mode on beside a different set of roots: err = %v, want the looser TLS refusal", err)
			}
			eff, _, _, err := ApplyContextOverrides(base, map[string]any{"pve_ca_cert": tc.entryCA})
			if err != nil {
				t.Fatalf("ApplyContextOverrides: %v", err)
			}
			if !eff.InheritedCredsLooserTLS() {
				t.Error("InheritedCredsLooserTLS = false, want true for an entry that loads other roots")
			}
		})
	}
}

// TestApplyContextOverrides_RestatedCASpellingsAreNotLooser drives the same
// spellings through ApplyContextOverrides, where the effective config has to
// validate and the mode on has to be accepted.
func TestApplyContextOverrides_RestatedCASpellingsAreNotLooser(t *testing.T) {
	t.Parallel()
	one, two := discoveryTestCA(t), discoveryTestCA(t)
	bundle := one + two
	for name, entry := range map[string]string{
		"CRLF line endings":  strings.ReplaceAll(bundle, "\n", "\r\n"),
		"a comment line":     "# the issuing CA\n" + bundle,
		"a duplicated block": bundle + one,
		"a reordered bundle": two + one,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			base := discoveryOverrideBase()
			base.PVECACertPEM = bundle
			eff, _, _, err := ApplyContextOverrides(base, map[string]any{
				"pve_ca_cert": entry, "pve_api_endpoint_discovery": "on",
			})
			if err != nil {
				t.Fatalf("a restated CA with mode on was refused: %v", err)
			}
			if eff.InheritedCredsLooserTLS() {
				t.Error("InheritedCredsLooserTLS = true, want false for the same CA in another spelling")
			}
		})
	}
}

// TestApplyContextOverrides_DiscoveryModeWrongTypeEchoesTheRawText asserts that
// a request whose mode has the wrong JSON type is refused with exactly the
// text the job-level decoder gives for the same bytes, whatever the spelling.
func TestApplyContextOverrides_DiscoveryModeWrongTypeEchoesTheRawText(t *testing.T) {
	t.Parallel()
	const prefix = `config: context override "pve_api_endpoint_discovery": `
	for _, raw := range []string{
		`5`, `5.0`, `1e3`, `-0.0`, `12345678901234567890`, `1.5`,
		`{"a":1}`, `{ "b" : 2 , "a" : [ 1 , 2 ] }`, `[ "on" ]`, `["<&>"]`,
		`[` + strings.Repeat("1", discoveryEchoLimit+10) + `]`,
		`1e400`, `-1e400`, `1E+999`, strings.Repeat("9", 400), `[1e400]`, `{"a":1e400}`,
	} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			var job DiscoveryMode
			jobErr := json.Unmarshal([]byte(raw), &job)
			if jobErr == nil {
				t.Fatalf("the job-level decoder accepted %s", raw)
			}
			for _, context := range []string{
				`{"pve_api_endpoint_discovery":` + raw + `}`,
				`{"pve":{"api_endpoint_discovery":` + raw + `}}`,
			} {
				_, _, _, err := ApplyContextOverrides(discoveryOverrideBase(), requestExtra(t, context))
				if err == nil {
					t.Fatalf("%s: accepted", context)
				}
				// json.Unmarshal wraps nothing for a Go error from UnmarshalJSON, so
				// the job-level text is the error text itself.
				if want := prefix + jobErr.Error(); err.Error() != want {
					t.Errorf("%s: error\n  %q\nwant\n  %q", context, err.Error(), want)
				}
			}
		})
	}
}

// TestCoerceOverrideDiscoveryMode_EchoesAValueThatCannotBeEncoded asserts that
// a value the encoder refuses, which decoded JSON never holds, still gives a
// refusal that names its Go type, and that raw values keep their spelling.
func TestCoerceOverrideDiscoveryMode_EchoesAValueThatCannotBeEncoded(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		value any
		want  string
	}{
		"NaN": {
			math.NaN(),
			"api_endpoint_discovery: expected a string or a boolean, got a value of type float64",
		},
		"infinity": {
			math.Inf(1),
			"api_endpoint_discovery: expected a string or a boolean, got a value of type float64",
		},
		"channel": {
			make(chan int),
			"api_endpoint_discovery: expected a string or a boolean, got a value of type chan int",
		},
		"a decoded float": {float64(5), "api_endpoint_discovery: expected a string or a boolean, got 5"},
		"raw text": {
			json.RawMessage(" 5.0\n"),
			"api_endpoint_discovery: expected a string or a boolean, got 5.0",
		},
		"a json.Number":    {json.Number("1e3"), "api_endpoint_discovery: expected a string or a boolean, got 1e3"},
		"an escaped value": {[]any{"<&>"}, `api_endpoint_discovery: expected a string or a boolean, got ["<&>"]`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mode, err := coerceOverrideDiscoveryMode(tc.value)
			if err == nil || err.Error() != tc.want || mode != "" {
				t.Errorf("coerceOverrideDiscoveryMode = %q, %v, want an error %q", mode, err, tc.want)
			}
		})
	}
}

// TestApplyContextOverrides_LooserTLSRefusalNamesTheEntry asserts that the
// refusal for the mode on names the host of the entry that caused it, in the
// same wording as the credential refusals, and still carries the reason.
func TestApplyContextOverrides_LooserTLSRefusalNamesTheEntry(t *testing.T) {
	t.Parallel()
	base := discoveryOverrideBase()
	_, _, _, err := ApplyContextOverrides(base, map[string]any{
		"pve_verify_ssl": false, "pve_api_endpoint_discovery": "on",
	})
	if err == nil {
		t.Fatal("ApplyContextOverrides accepted mode on beside inherited credentials and looser TLS")
	}
	for _, want := range []string{
		`context entry for host "pve-a.example"`,
		"effective override config failed validation",
		"looser TLS than the job",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
}

// TestApplyContextOverrides_ValidationRefusalNamesTheEntrysHostNotTheJobs
// asserts that when the entry sits on another host from the job, the refusal
// names the entry's host and leaves the job's out. The entry has its own user
// and password, turns verification off, and asks for the mode on, which
// validation refuses for a password without verification.
func TestApplyContextOverrides_ValidationRefusalNamesTheEntrysHostNotTheJobs(t *testing.T) {
	t.Parallel()
	base := discoveryOverrideBase()
	_, _, _, err := ApplyContextOverrides(base, map[string]any{
		"pve_host":                   "pve-b.example",
		"pve_user":                   "root",
		"pve_password":               "entry-password",
		"pve_verify_ssl":             false,
		"pve_api_endpoint_discovery": "on",
	})
	if err == nil {
		t.Fatal("ApplyContextOverrides accepted mode on with a password and no verification")
	}
	const want = `context entry for host "pve-b.example": effective override config failed validation`
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q lacks %q", err, want)
	}
	if strings.Contains(err.Error(), base.Host) {
		t.Errorf("error %q names the job's host %q", err, base.Host)
	}
}

// tlsFieldWords matches the names of config fields and override keys that
// concern TLS.
var tlsFieldWords = regexp.MustCompile(`(?i)tls|ssl|cert|fingerprint|(^|_)ca($|_)`)

// tlsFieldsOutsideTrust are the config fields whose names mention TLS but
// which decide neither whether the server's certificate is checked nor which
// roots it is checked against, so an override of one cannot loosen TLS.
var tlsFieldsOutsideTrust = map[string]bool{
	"RejectTLSDowngradeOverrides":         true,
	"StemcellFetchTLSHandshakeTimeoutSec": true,
	"PVETLSHandshakeTimeoutSec":           true,
}

// tlsConfigFields returns the names of the exported CPIConfig fields that
// decide how the client verifies the server, found from the field name and
// its JSON name.
func tlsConfigFields(t *testing.T) map[string]bool {
	t.Helper()
	fields := map[string]bool{}
	typ := reflect.TypeOf(CPIConfig{})
	for i := range typ.NumField() {
		field := typ.Field(i)
		jsonName, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if field.IsExported() && !tlsFieldsOutsideTrust[field.Name] &&
			(tlsFieldWords.MatchString(field.Name) || tlsFieldWords.MatchString(jsonName)) {
			fields[field.Name] = true
		}
	}
	return fields
}

// changedFields applies one override handler with value v to an empty config
// and returns the exported fields it set. A handler that refuses v, or panics
// on it, sets nothing.
func changedFields(apply func(*CPIConfig, any) error, v any) (changed []string) {
	defer func() {
		if recover() != nil {
			changed = nil
		}
	}()
	var cfg CPIConfig
	if apply(&cfg, v) != nil {
		return nil
	}
	val := reflect.ValueOf(cfg)
	zero := reflect.ValueOf(CPIConfig{})
	for i := range val.NumField() {
		if val.Type().Field(i).IsExported() && !reflect.DeepEqual(val.Field(i).Interface(), zero.Field(i).Interface()) {
			changed = append(changed, val.Type().Field(i).Name)
		}
	}
	return changed
}

// TestTLSOverrideKeys_NameEveryKeyThatReachesATLSField is the guard for the
// looser TLS check. It finds the config fields that decide how the server is
// verified, drives every override handler with a few values of different
// types, and asserts that each key which sets such a field, or which carries
// a TLS word in its name, is in tlsOverrideKeys, the list the check reads. A
// new override key that touches TLS fails here until it joins the list and
// the comparison in markInheritedCredsLooserTLS.
func TestTLSOverrideKeys_NameEveryKeyThatReachesATLSField(t *testing.T) {
	t.Parallel()
	fields := tlsConfigFields(t)
	for _, name := range []string{"VerifySSL", "PVECACertPEM"} {
		if !fields[name] {
			t.Fatalf("the TLS field search did not find %s, found %v", name, fields)
		}
	}
	probes := []any{"x", true, float64(1), []any{"x"}, map[string]any{"x": "y"}}
	reached := map[string]bool{}
	for _, key := range contextOverrideFieldOrder {
		apply, ok := contextOverrideFields[key]
		if !ok {
			t.Fatalf("%s has no apply function", key)
		}
		touches := tlsFieldWords.MatchString(key)
		for _, probe := range probes {
			for _, name := range changedFields(apply, probe) {
				if fields[name] {
					touches = true
					reached[name] = true
				}
			}
		}
		if touches && !slices.Contains(tlsOverrideKeys, key) {
			t.Errorf("%s reaches a TLS field but is not in tlsOverrideKeys, so the looser TLS check never sees it", key)
		}
	}
	for _, name := range []string{"VerifySSL", "PVECACertPEM"} {
		if !reached[name] {
			t.Errorf("no override key reached %s, so the probe values no longer drive the handlers", name)
		}
	}
	for _, key := range tlsOverrideKeys {
		if !slices.Contains(contextOverrideFieldOrder, key) {
			t.Errorf("tlsOverrideKeys names %s, which is not an override key", key)
		}
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
