package handlers

import (
	"slices"
	"strings"
	"testing"
)

// TestLegacyTagsAreSpelledInLowerCase: the older job and deployment tags are
// built in lower case, the way PVE stores them and the owner tag is built, so
// the rule scan and the soft-spread scorer name the same tags and two groups
// that differ only in case do not read as different ones.
func TestLegacyTagsAreSpelledInLowerCase(t *testing.T) {
	t.Parallel()
	ident := haIdentity{Owner: "AAAA-BBBB", Deployment: "Cell", Group: "Web"}
	want := []string{"job--web", "deployment--cell", ownerTagFor("AAAA-BBBB")}
	if got := legacyMemberTags(ident); !slices.Equal(got, want) {
		t.Errorf("legacyMemberTags = %v, want %v", got, want)
	}
	upper := legacyMemberTags(haIdentity{Owner: "AAAA-BBBB", Deployment: "CELL", Group: "WEB"})
	if !slices.Equal(upper, want) {
		t.Errorf("a case-only difference must name the same legacy tags, got %v", upper)
	}
	if tagsContainAll("job--web;deployment--cell;"+want[2], legacyMemberTags(ident)) != true {
		t.Error("the lower-case tags PVE stores must match")
	}
}

// TestScorerGroupLegacyTagsAreLowerCase: the soft-spread scorer counts legacy
// members by the same lower-case tags the rule scan uses.
func TestScorerGroupLegacyTagsAreLowerCase(t *testing.T) {
	t.Parallel()
	env := map[string]any{"bosh": map[string]any{
		"group":  "director-Cell-Web",
		"groups": []any{"director", "Cell", "Web", "director-Cell", "director-Cell-Web"},
	}}
	deps := spreadDeps(nil)
	group := antiAffinityScorerGroup(deps, env)
	for _, tag := range group.legacy {
		if tag != strings.ToLower(tag) {
			t.Errorf("legacy tag %q is not lower case: %v", tag, group.legacy)
		}
	}
	if !slices.Contains(group.legacy, "job--web") || !slices.Contains(group.legacy, "deployment--cell") {
		t.Errorf("legacy tags = %v, want job--web and deployment--cell", group.legacy)
	}
}
