package config

import (
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// specPropertyLine matches a property name line of jobs/pve_cpi/spec: two
// spaces of indent, a pve. or agent. name, and a colon. Description text is
// indented further, so it never matches.
var specPropertyLine = regexp.MustCompile(`^  ((?:pve|agent)\.[A-Za-z0-9_.-]+):`)

// TestJobSpecPropertyNamesMatchSpec keeps the generated name list equal to
// the job spec, so a property added to the spec cannot be rejected as a typo
// and a removed one cannot linger.
func TestJobSpecPropertyNamesMatchSpec(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../../../jobs/pve_cpi/spec")
	if err != nil {
		t.Fatalf("read the job spec: %v", err)
	}
	var want []string
	for _, line := range strings.Split(string(raw), "\n") {
		if m := specPropertyLine.FindStringSubmatch(line); m != nil {
			want = append(want, m[1])
		}
	}
	sort.Strings(want)
	got := slices.Clone(jobSpecPropertyNames)
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Fatalf("jobSpecPropertyNames differs from jobs/pve_cpi/spec (regenerate it as its comment says)\n"+
			"only in the list: %v\nonly in the spec: %v", diff(got, want), diff(want, got))
	}
	if !sort.StringsAreSorted(jobSpecPropertyNames) {
		t.Error("jobSpecPropertyNames must stay sorted so regeneration produces no churn")
	}
	if len(want) == 0 {
		t.Fatal("the spec parse found no property names")
	}
}

// TestJobSpecPropertiesSectionIsFullyParsed scans the spec's properties
// section with a looser rule than specPropertyLine, so a property line the
// strict pattern misses (a quoted key, odd indent) fails here instead of
// vanishing from both the list and the test.
func TestJobSpecPropertiesSectionIsFullyParsed(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../../../jobs/pve_cpi/spec")
	if err != nil {
		t.Fatalf("read the job spec: %v", err)
	}
	inProperties := false
	found := 0
	for i, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "properties:") {
			inProperties = true
			continue
		}
		if !inProperties {
			continue
		}
		if line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "#") {
			break // the next top-level section
		}
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if indent != 2 || trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "- ") {
			continue
		}
		found++
		if !specPropertyLine.MatchString(line) {
			t.Errorf("spec line %d is a property the strict pattern rejects: %q", i+1, line)
		}
	}
	if found == 0 {
		t.Fatal("the loose scan found no property lines")
	}
}

func diff(a, b []string) []string {
	var out []string
	for _, s := range a {
		if !slices.Contains(b, s) {
			out = append(out, s)
		}
	}
	return out
}

// TestEveryContextOverrideKeyIsAcceptedByInspect proves the typo check never
// rejects a key ApplyContextOverrides applies, in the nested and flat forms.
func TestEveryContextOverrideKeyIsAcceptedByInspect(t *testing.T) {
	t.Parallel()
	for _, key := range contextOverrideFieldOrder {
		flat := map[string]any{key: "x"}
		if _, err := InspectContextKeys(flat); err != nil {
			t.Errorf("flat %s rejected: %v", key, err)
		}
		nested := map[string]any{"pve": map[string]any{strings.TrimPrefix(key, contextOverridePrefix): "x"}}
		if _, err := InspectContextKeys(nested); err != nil {
			t.Errorf("nested pve.%s rejected: %v", strings.TrimPrefix(key, contextOverridePrefix), err)
		}
	}
}

// TestDirectorContextKeysAreNotSpecNames guards the no-flat-alias rule: a key
// the Director itself writes must never read as a misnested spec name. The
// stemcell key is left out because jsonrpc.Context decodes it as a typed
// field, so it never reaches the extra keys.
func TestDirectorContextKeysAreNotSpecNames(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"director_uuid", "request_id", "vm"} {
		if topLevelMatchesSpec(key) {
			t.Errorf("Director key %q matches a spec name", key)
		}
	}
}
