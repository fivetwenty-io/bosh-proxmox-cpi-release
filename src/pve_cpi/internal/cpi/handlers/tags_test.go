package handlers

import (
	"slices"
	"strings"
	"testing"
)

func TestSanitizeTagValue(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"plain", "plain"},
		{"with space", "with-space"},
		{"a_b/c.d", "a-b-c-d"},
		{"--leading", "leading"},
		{"trailing--", "trailing"},
		{"keep-dash", "keep-dash"},
		{"MixedCase123", "MixedCase123"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			got := sanitizeTagValue(c.in)
			if got != c.want {
				t.Errorf("sanitizeTagValue(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestBuildCustomTags_Empty(t *testing.T) {
	t.Parallel()
	if got := buildCustomTags(nil); got != nil {
		t.Errorf("nil map: got %v, want nil", got)
	}
	if got := buildCustomTags(map[string]string{}); got != nil {
		t.Errorf("empty map: got %v, want nil", got)
	}
}

func TestBuildCustomTags_DeterministicSort(t *testing.T) {
	t.Parallel()
	in := map[string]string{
		"zeta":  "Z",
		"alpha": "A",
		"mu":    "M",
	}
	got := buildCustomTags(in)
	want := []string{"alpha--A", "mu--M", "zeta--Z"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i, p := range want {
		if got[i] != p {
			t.Errorf("entry %d = %q, want %q", i, got[i], p)
		}
	}
}

func TestBuildCustomTags_SkipEmptyAndSanitize(t *testing.T) {
	t.Parallel()
	in := map[string]string{
		"bad key":  "with spaces",
		"empty":    "",
		"":         "ignored",
		"env":      "prod",
		"--only--": "x",
	}
	got := buildCustomTags(in)
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "bad-key--with-spaces") {
		t.Errorf("expected sanitized 'bad-key--with-spaces' in %v", got)
	}
	if !strings.Contains(joined, "env--prod") {
		t.Errorf("expected 'env--prod' in %v", got)
	}
	for _, p := range got {
		if !strings.Contains(p, "--") {
			t.Errorf("malformed entry %q", p)
		}
		if strings.HasPrefix(p, "--") || strings.HasSuffix(p, "--") {
			t.Errorf("entry has stray leading/trailing '--': %q", p)
		}
	}
}

func TestMergeTagList_Truncation(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 80)
	additions := []string{
		"k1--" + long,
		"k2--" + long,
		"k3--" + long,
		"k4--" + long,
	}
	got := mergeTagList(nil, additions, 255)
	if len(got) > 255 {
		t.Errorf("len = %d > 255", len(got))
	}
	for _, p := range strings.Split(got, ";") {
		if !strings.Contains(p, "--") {
			t.Errorf("partial entry %q in %q", p, got)
		}
	}
}

func TestMergeTagList_DedupesAndPreservesOrder(t *testing.T) {
	t.Parallel()
	existing := []string{"env--prod", "owner--alice"}
	additions := []string{"env--prod", "tier--gold"}
	got := mergeTagList(existing, additions, 0)
	want := "env--prod;owner--alice;tier--gold"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMergeTagList_Empty(t *testing.T) {
	t.Parallel()
	if got := mergeTagList(nil, nil, 255); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestMergeTagListReporting_NothingDropped checks that a list which fits under
// the cap reports no dropped entry at all, so the caller stays silent.
func TestMergeTagListReporting_NothingDropped(t *testing.T) {
	t.Parallel()
	merged, dropped := mergeTagListReporting([]string{"env--prod"}, []string{"tier--gold"}, 255)
	if merged != "env--prod;tier--gold" {
		t.Errorf("merged = %q, want %q", merged, "env--prod;tier--gold")
	}
	if dropped != nil {
		t.Errorf("dropped = %v, want nil", dropped)
	}
}

// TestMergeTagListReporting_OneDropped checks that the single entry the cap
// pushes off the end comes back by name.
func TestMergeTagListReporting_OneDropped(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 80)
	additions := []string{"k1--" + long, "k2--" + long, "vm-prefix--blue"}
	// Two 84-byte entries and one separator fill 169 bytes, so a cap of 180
	// leaves no room for the identity tag.
	merged, dropped := mergeTagListReporting(nil, additions, 180)
	if strings.Contains(merged, "vm-prefix--blue") {
		t.Errorf("merged should not hold the identity tag; got %q", merged)
	}
	want := []string{"vm-prefix--blue"}
	if !slices.Equal(dropped, want) {
		t.Errorf("dropped = %v, want %v", dropped, want)
	}
}

// TestMergeTagListReporting_SeveralDropped checks that every entry from the
// first one that does not fit onwards is reported, in the order we dropped it.
func TestMergeTagListReporting_SeveralDropped(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 80)
	additions := []string{"k1--" + long, "k2--" + long, "k3--" + long, "k4--" + long}
	merged, dropped := mergeTagListReporting(nil, additions, 180)
	if merged != additions[0]+";"+additions[1] {
		t.Errorf("merged = %q, want the first two entries", merged)
	}
	want := []string{additions[2], additions[3]}
	if !slices.Equal(dropped, want) {
		t.Errorf("dropped = %v, want %v", dropped, want)
	}
}

// TestMergeTagListReporting_DuplicatesNotDropped checks that an entry the merge
// deduplicates is never reported as a casualty of the cap, because it is still
// in the merged string.
func TestMergeTagListReporting_DuplicatesNotDropped(t *testing.T) {
	t.Parallel()
	existing := []string{"env--prod", "owner--alice"}
	additions := []string{"env--prod", "owner--alice", "tier--gold"}
	merged, dropped := mergeTagListReporting(existing, additions, 0)
	if merged != "env--prod;owner--alice;tier--gold" {
		t.Errorf("merged = %q, want the three distinct entries", merged)
	}
	if dropped != nil {
		t.Errorf("dropped = %v, want nil", dropped)
	}
}

// TestMergeTagListReporting_FirstEntryTooLong checks the corner where even one
// entry overflows the cap. The merged string comes back empty and every entry
// is reported.
func TestMergeTagListReporting_FirstEntryTooLong(t *testing.T) {
	t.Parallel()
	additions := []string{"k1--" + strings.Repeat("a", 80), "k2--short"}
	merged, dropped := mergeTagListReporting(nil, additions, 10)
	if merged != "" {
		t.Errorf("merged = %q, want empty", merged)
	}
	if !slices.Equal(dropped, additions) {
		t.Errorf("dropped = %v, want %v", dropped, additions)
	}
}

func TestParseTagsField(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"single", "a", []string{"a"}},
		{"semicolon-sep", "a;b;c", []string{"a", "b", "c"}},
		{"comma-sep", "a,b,c", []string{"a", "b", "c"}},
		{"with-spaces", "a; b ;c", []string{"a", "b", "c"}},
		{"empty-tokens", ";;a;;", []string{"a"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := parseTagsField(c.in)
			if !slices.Equal(got, c.want) {
				t.Errorf("parseTagsField(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestStripReservedBoshTags(t *testing.T) {
	t.Parallel()
	in := []string{
		"env--prod",
		"director--abc",
		"team--payments",
		"deployment--cf",
		"job--diego-cell",
		"owner--alice",
	}
	got := stripReservedBoshTags(in)
	want := []string{"env--prod", "team--payments", "owner--alice"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestStripReservedBoshTags_Empty(t *testing.T) {
	t.Parallel()
	if got := stripReservedBoshTags(nil); got != nil {
		t.Errorf("nil: got %v, want nil", got)
	}
}

// TestStripReservedBoshTags_PreservesBoshCPI verifies that the "bosh-cpi"
// ownership marker is NOT stripped by stripReservedBoshTags. The marker must
// survive set_vm_metadata's tag-rebuild cycle so hand-made VMs remain
// distinguishable from CPI-managed ones.
func TestStripReservedBoshTags_PreservesBoshCPI(t *testing.T) {
	t.Parallel()
	in := []string{
		"bosh-cpi",
		"director--abc",
		"deployment--cf",
		"job--worker",
		"index--0",
	}
	got := stripReservedBoshTags(in)
	want := []string{"bosh-cpi"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestStripReservedBoshTags_StripsVMPrefix verifies the vm-prefix-- identity
// tag is stripped like every other CPI-owned key. It has to be, because
// set_vm_metadata re-applies it from the current configuration, and a tag left
// standing here would sit beside the new one after an operator changes the
// prefix.
func TestStripReservedBoshTags_StripsVMPrefix(t *testing.T) {
	t.Parallel()
	in := []string{
		"bosh-cpi",
		"vm-prefix--old",
		"env--prod",
		"advrt-abc12345",
	}
	got := stripReservedBoshTags(in)
	want := []string{"bosh-cpi", "env--prod", "advrt-abc12345"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestBuildBoshManagedTags_PrefixTagAppendedLast pins the ordering promise in
// buildBoshManagedTags's doc comment. mergeTagList truncates at a tag boundary
// and drops the last entry first, so the identity tag has to sit last and go
// first when a long deployment and job set overflows the cap.
func TestBuildBoshManagedTags_PrefixTagAppendedLast(t *testing.T) {
	t.Parallel()
	got := buildBoshManagedTags(map[string]any{
		"director":       "d1",
		"deployment":     "cf",
		"instance_group": "web",
		"job":            "web",
		"index":          "0",
		"name":           "web/abc",
	}, "blue")
	want := []string{
		"director--d1",
		"deployment--cf",
		"instance-group--web",
		"job--web",
		"index--0",
		"web--abc",
		"vm-prefix--blue",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestBuildBoshManagedTags_EmptyPrefixOmitsTag covers the direct-call case the
// accessor cannot produce. An empty prefix emits no tag at all rather than a
// bare "vm-prefix--".
func TestBuildBoshManagedTags_EmptyPrefixOmitsTag(t *testing.T) {
	t.Parallel()
	got := buildBoshManagedTags(map[string]any{"deployment": "cf"}, "")
	want := []string{"deployment--cf"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestBuildBoshManagedTags_PrefixSanitized verifies the prefix runs through the
// same sanitizer every other tag value does.
func TestBuildBoshManagedTags_PrefixSanitized(t *testing.T) {
	t.Parallel()
	got := buildBoshManagedTags(map[string]any{}, "my_prefix")
	want := []string{"vm-prefix--my-prefix"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestMergeTagList_BoshCPIDedup verifies that mergeTagList deduplicates
// "bosh-cpi" when passed in both existing and additions slices.
func TestMergeTagList_BoshCPIDedup(t *testing.T) {
	t.Parallel()
	got := mergeTagList([]string{"bosh-cpi"}, []string{"bosh-cpi", "env--prod"}, 0)
	// "bosh-cpi" must appear exactly once.
	parts := strings.Split(got, ";")
	count := 0
	for _, p := range parts {
		if p == "bosh-cpi" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("mergeTagList with duplicate bosh-cpi: got %q, want exactly one occurrence", got)
	}
}

func TestSanitizeVMName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"uuid-path", "diego-cell/2844c990-aef3-4de7-8bf3-d936fc2201be", "diego-cell-2844c990-aef3-4de7-8bf3-d936fc2201be"},
		{"simple-path", "bosh/0", "bosh-0"},
		{"underscores", "job_with_underscores/abc", "job-with-underscores-abc"},
		{"multi-segment", "a/b/c", "a-b-c"},
		{"leading-trailing-dashes", "---leading-and-trailing---", "leading-and-trailing"},
		{"consecutive-invalids", "a..b", "a-b"},
		{"mixed-invalids", "a/_/b", "a-b"},
		{"mixed-case", "MixedCase/123", "MixedCase-123"},
		{"all-invalids", "////", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := sanitizeVMName(c.in)
			if got != c.want {
				t.Errorf("sanitizeVMName(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestSanitizeVMName_TruncatesTo63(t *testing.T) {
	t.Parallel()
	long := "diego-cell/" + strings.Repeat("a", 80)
	got := sanitizeVMName(long)
	if len(got) > 63 {
		t.Errorf("sanitizeVMName length = %d, want <= 63; got %q", len(got), got)
	}
	if strings.HasSuffix(got, "-") {
		t.Errorf("sanitizeVMName must not end in '-'; got %q", got)
	}
}
