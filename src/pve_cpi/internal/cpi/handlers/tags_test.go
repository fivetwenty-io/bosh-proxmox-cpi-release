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
