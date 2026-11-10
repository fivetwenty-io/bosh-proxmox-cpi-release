package config

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The job template renders cpi.json from the job properties and refuses a bad
// discovery value with the same text the config gives, so a manifest fails at
// deploy time with a message the CPI would also have produced. The two
// fixtures below tie the two sides together. scripts/_test_erb_render.sh reads
// the same files and asserts that the template produces each text exactly.

// templateRefusal is one entry of testdata/discovery_refusals.json. Config
// holds cpi.json members that sit on top of a valid password config, and the
// template reads the same names as pve.* properties.
type templateRefusal struct {
	Name    string                     `json:"name"`
	Config  map[string]json.RawMessage `json:"config"`
	Message string                     `json:"message"`
}

func readTemplateFixture(t *testing.T, name string, into any) {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
}

// refusalMessage strips the prefix Load puts in front of the first error.
func refusalMessage(t *testing.T, err error) string {
	t.Helper()
	text := err.Error()
	for _, prefix := range []string{"config validation failed: ", "config: decode failed: "} {
		if rest, ok := strings.CutPrefix(text, prefix); ok {
			return rest
		}
	}
	t.Fatalf("error %q has neither the validation nor the decode prefix", text)
	return ""
}

func TestLoad_RefusalTextsMatchTheTemplateFixture(t *testing.T) {
	t.Parallel()
	var cases []templateRefusal
	readTemplateFixture(t, "discovery_refusals.json", &cases)
	if len(cases) == 0 {
		t.Fatal("the fixture holds no cases")
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		if seen[tc.Name] {
			t.Fatalf("the fixture names %q twice", tc.Name)
		}
		seen[tc.Name] = true
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			members := map[string]json.RawMessage{
				"host":           json.RawMessage(`"pve1.example.com"`),
				"user":           json.RawMessage(`"root@pam"`),
				"password":       json.RawMessage(`"secret"`),
				"vm_storage":     json.RawMessage(`"local-lvm"`),
				"disk_storage":   json.RawMessage(`"local-lvm"`),
				"network_bridge": json.RawMessage(`"vmbr0"`),
			}
			for k, v := range tc.Config {
				members[k] = v
			}
			doc, err := json.Marshal(members)
			if err != nil {
				t.Fatalf("compose cpi.json: %v", err)
			}
			_, err = Load(strings.NewReader(string(doc)))
			if err == nil {
				t.Fatalf("Load accepted %s, want the refusal %q", doc, tc.Message)
			}
			if got := refusalMessage(t, err); got != tc.Message {
				t.Errorf("Load refused with\n  %q\nwant the template's text\n  %q", got, tc.Message)
			}
		})
	}
}

// templateQuoteCase is one entry of testdata/template_quote_cases.json. Hex
// holds the input bytes, which may be invalid UTF-8, and Quoted holds what
// strconv.Quote returns for them. The template's quoting helper has to produce
// the same text, because the CPI's messages use %q.
type templateQuoteCase struct {
	Name   string `json:"name"`
	Hex    string `json:"hex"`
	Quoted string `json:"quoted"`
}

func TestTemplateQuoteFixtureMatchesStrconvQuote(t *testing.T) {
	t.Parallel()
	var cases []templateQuoteCase
	readTemplateFixture(t, "template_quote_cases.json", &cases)
	if len(cases) == 0 {
		t.Fatal("the fixture holds no cases")
	}
	for _, tc := range cases {
		in, err := hex.DecodeString(tc.Hex)
		if err != nil {
			t.Fatalf("%s: bad hex %q: %v", tc.Name, tc.Hex, err)
		}
		if got := strconv.Quote(string(in)); got != tc.Quoted {
			t.Errorf("%s: strconv.Quote gives %q, the fixture holds %q", tc.Name, got, tc.Quoted)
		}
	}
}
