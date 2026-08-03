package pve_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

// buildImportContent marshals volid strings into a ListStorageContentResponse
// simulating PVE content-type "import" entries.
func buildImportContent(volids ...string) *sdknodes.ListStorageContentResponse {
	resp := make(sdknodes.ListStorageContentResponse, 0, len(volids))
	for _, v := range volids {
		entry := struct {
			VolID   string `json:"volid"`
			Content string `json:"content"`
		}{VolID: v, Content: "import"}
		raw, _ := json.Marshal(entry)
		resp = append(resp, raw)
	}
	return &resp
}

// ---- TestBuildStemcellFilename ----

func TestBuildStemcellFilename(t *testing.T) {
	t.Parallel()
	got := pve.BuildStemcellFilename("ubuntu-jammy", "1.234", "abc12345def67890abcdef")
	want := "bosh-stemcell-ubuntu-jammy-1.234-abc12345.qcow2"
	if got != want {
		t.Errorf("BuildStemcellFilename = %q; want %q", got, want)
	}
}

// ---- TestBuildStemcellCID ----

func TestBuildStemcellCID(t *testing.T) {
	t.Parallel()
	got := pve.BuildStemcellCID("nfs-pool", "foo.qcow2")
	want := "nfs-pool:import/foo.qcow2"
	if got != want {
		t.Errorf("BuildStemcellCID = %q; want %q", got, want)
	}
}

// ---- TestParseStemcellCID_Valid ----

func TestParseStemcellCID_Valid(t *testing.T) {
	t.Parallel()
	storage, path, err := pve.ParseStemcellCID("nfs-pool:import/foo.qcow2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if storage != "nfs-pool" {
		t.Errorf("storage = %q; want %q", storage, "nfs-pool")
	}
	if path != "import/foo.qcow2" {
		t.Errorf("volumePath = %q; want %q", path, "import/foo.qcow2")
	}
}

// ---- TestFindStemcellByFilename_Found ----

func TestFindStemcellByFilename_Found(t *testing.T) {
	t.Parallel()

	// mockClient is defined in task_test.go (same pve_test package).
	// Wire only the nodes service; all other accessors return nil.
	client := &mockClient{
		nodesSvc: &stubNodesService{
			listStorageContentFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
				return buildImportContent("nfs-pool:import/foo.qcow2"), nil
			},
		},
	}

	volid, err := pve.FindStemcellByFilename(context.Background(), client, "pve", "nfs-pool", "foo.qcow2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if volid != "nfs-pool:import/foo.qcow2" {
		t.Errorf("volid = %q; want %q", volid, "nfs-pool:import/foo.qcow2")
	}
}

// ---- ParseStemcellCID edge cases ----

func TestParseStemcellCID_Empty(t *testing.T) {
	t.Parallel()
	_, _, err := pve.ParseStemcellCID("")
	if err == nil {
		t.Fatal("expected error for empty CID, got nil")
	}
}

func TestParseStemcellCID_IntegerLegacyFormat(t *testing.T) {
	t.Parallel()
	_, _, err := pve.ParseStemcellCID("5042")
	if err == nil {
		t.Fatal("expected error for legacy integer CID, got nil")
	}
}

func TestParseStemcellCID_NoColon(t *testing.T) {
	t.Parallel()
	_, _, err := pve.ParseStemcellCID("foopath")
	if err == nil {
		t.Fatal("expected error for CID missing ':', got nil")
	}
}

func TestParseStemcellCID_MissingImportPrefix(t *testing.T) {
	t.Parallel()
	_, _, err := pve.ParseStemcellCID("storage:other/foo.qcow2")
	if err == nil {
		t.Fatal("expected error for CID without 'import/' prefix, got nil")
	}
}

func TestParseStemcellCID_EmptyFilename(t *testing.T) {
	t.Parallel()
	// "storage:import/" has an empty filename segment. The format check passes
	// (path starts with "import/") so ParseStemcellCID succeeds, returning an
	// empty filename. Callers validate the filename separately.
	storage, path, err := pve.ParseStemcellCID("storage:import/")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if storage != "storage" {
		t.Errorf("storage = %q; want %q", storage, "storage")
	}
	if path != "import/" {
		t.Errorf("volumePath = %q; want %q", path, "import/")
	}
}

func TestParseStemcellCID_MultipleColons(t *testing.T) {
	t.Parallel()
	// Design choice: storage = first segment before ':', path = everything after.
	// "storage:import/has:colon.qcow2" → storage="storage", path="import/has:colon.qcow2".
	storage, path, err := pve.ParseStemcellCID("storage:import/has:colon.qcow2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if storage != "storage" {
		t.Errorf("storage = %q; want %q", storage, "storage")
	}
	if path != "import/has:colon.qcow2" {
		t.Errorf("volumePath = %q; want %q", path, "import/has:colon.qcow2")
	}
}

// ---- FindStemcellByFilename edge cases ----

func TestFindStemcellByFilename_NotFound(t *testing.T) {
	t.Parallel()
	client := &mockClient{
		nodesSvc: &stubNodesService{
			listStorageContentFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
				return buildImportContent("nfs-pool:import/other.qcow2"), nil
			},
		},
	}
	volid, err := pve.FindStemcellByFilename(context.Background(), client, "pve", "nfs-pool", "foo.qcow2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if volid != "" {
		t.Errorf("expected empty volid for no match, got %q", volid)
	}
}

func TestFindStemcellByFilename_APIError(t *testing.T) {
	t.Parallel()
	apiErr := errors.New("storage not available")
	client := &mockClient{
		nodesSvc: &stubNodesService{
			listStorageContentFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
				return nil, apiErr
			},
		},
	}
	_, err := pve.FindStemcellByFilename(context.Background(), client, "pve", "nfs-pool", "foo.qcow2")
	if err == nil {
		t.Fatal("expected error propagated from API, got nil")
	}
	if !errors.Is(err, apiErr) {
		t.Errorf("expected wrapped apiErr, got %v", err)
	}
}

func TestFindStemcellByFilename_NilResponse(t *testing.T) {
	t.Parallel()
	// SDK returns nil response pointer with no error → function returns ("", nil).
	client := &mockClient{
		nodesSvc: &stubNodesService{
			listStorageContentFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
				return nil, nil
			},
		},
	}
	volid, err := pve.FindStemcellByFilename(context.Background(), client, "pve", "nfs-pool", "foo.qcow2")
	if err != nil {
		t.Fatalf("unexpected error for nil response: %v", err)
	}
	if volid != "" {
		t.Errorf("expected empty volid for nil response, got %q", volid)
	}
}

func TestFindStemcellByFilename_EmptyList(t *testing.T) {
	t.Parallel()
	client := &mockClient{
		nodesSvc: &stubNodesService{
			listStorageContentFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
				return buildImportContent(), nil
			},
		},
	}
	volid, err := pve.FindStemcellByFilename(context.Background(), client, "pve", "nfs-pool", "foo.qcow2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if volid != "" {
		t.Errorf("expected empty volid for empty list, got %q", volid)
	}
}

func TestFindStemcellByFilename_MalformedItem(t *testing.T) {
	t.Parallel()
	// Design choice: malformed JSON items (not valid storageContentItem) are
	// skipped silently. A single well-formed match after the malformed entry
	// is still found.
	resp := make(sdknodes.ListStorageContentResponse, 0, 2)
	resp = append(resp, json.RawMessage(`{not valid json`))
	goodEntry, _ := json.Marshal(struct {
		VolID   string `json:"volid"`
		Content string `json:"content"`
	}{VolID: "nfs-pool:import/foo.qcow2", Content: "import"})
	resp = append(resp, json.RawMessage(goodEntry))

	client := &mockClient{
		nodesSvc: &stubNodesService{
			listStorageContentFn: func(_ context.Context, _, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
				return &resp, nil
			},
		},
	}
	volid, err := pve.FindStemcellByFilename(context.Background(), client, "pve", "nfs-pool", "foo.qcow2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if volid != "nfs-pool:import/foo.qcow2" {
		t.Errorf("expected match after malformed entry, got %q", volid)
	}
}

// ---- BuildStemcellFilename sanitization edge cases ----

func TestBuildStemcellFilename_UppercaseLowered(t *testing.T) {
	t.Parallel()
	got := pve.BuildStemcellFilename("Ubuntu", "1.0", "abcdef1234567890")
	want := "bosh-stemcell-ubuntu-1.0-abcdef12.qcow2"
	if got != want {
		t.Errorf("BuildStemcellFilename = %q; want %q", got, want)
	}
}

func TestBuildStemcellFilename_SpacesReplaced(t *testing.T) {
	t.Parallel()
	got := pve.BuildStemcellFilename("ubuntu jammy", "1.0", "abcdef1234567890")
	want := "bosh-stemcell-ubuntu-jammy-1.0-abcdef12.qcow2"
	if got != want {
		t.Errorf("BuildStemcellFilename = %q; want %q", got, want)
	}
}

func TestBuildStemcellFilename_SpecialChars(t *testing.T) {
	t.Parallel()
	// '/', '@' are not in [a-z0-9._] so each collapses to a single '-'.
	got := pve.BuildStemcellFilename("foo/bar@baz", "1.0", "abcdef1234567890")
	want := "bosh-stemcell-foo-bar-baz-1.0-abcdef12.qcow2"
	if got != want {
		t.Errorf("BuildStemcellFilename = %q; want %q", got, want)
	}
}

func TestBuildStemcellFilename_LongInputTruncated(t *testing.T) {
	t.Parallel()
	// Name 300 chars + version 300 chars exceeds maxStemcellFilenameLen (200).
	// Result must be ≤ 255 chars total (NAME_MAX).
	longName := strings.Repeat("a", 300)
	longVersion := strings.Repeat("b", 300)
	got := pve.BuildStemcellFilename(longName, longVersion, "abcdef1234567890")
	// suffix "-abcdef12.qcow2" = 15 chars; total must be ≤ 255.
	if len(got) > 255 {
		t.Errorf("BuildStemcellFilename length %d exceeds 255 (NAME_MAX)", len(got))
	}
	if !strings.HasSuffix(got, ".qcow2") {
		t.Errorf("BuildStemcellFilename %q missing .qcow2 suffix", got)
	}
}

func TestBuildStemcellFilename_ShortSHA(t *testing.T) {
	t.Parallel()
	// sha256hex < 8 chars → function uses "00000000" placeholder.
	got := pve.BuildStemcellFilename("ubuntu", "1.0", "abc")
	if !strings.HasSuffix(got, "-00000000.qcow2") {
		t.Errorf("BuildStemcellFilename with short sha = %q; want suffix -00000000.qcow2", got)
	}
}

func TestBuildStemcellFilename_EmptySHA(t *testing.T) {
	t.Parallel()
	// Empty sha → same "00000000" placeholder.
	got := pve.BuildStemcellFilename("ubuntu", "1.0", "")
	if !strings.HasSuffix(got, "-00000000.qcow2") {
		t.Errorf("BuildStemcellFilename with empty sha = %q; want suffix -00000000.qcow2", got)
	}
}

// TestSanitizeStemcellPart_MultibyteUTF8_ProducesAsciiOnly verifies that
// multi-byte UTF-8 characters (e.g., CJK, emoji, combining marks) are treated
// as a single disallowed unit and produce exactly one "-" rather than one "-"
// per byte. This validates the rune-iteration behavior.
func TestSanitizeStemcellPart_MultibyteUTF8_ProducesAsciiOnly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string // stemcell name/version containing multi-byte runes
		// want describes only the ASCII-only guarantee and rune-vs-byte collapse.
		// Exact output checked where deterministic.
		wantContainsOnly func(s string) bool
		wantOutput       string // exact expected output; empty means skip exact check
	}{
		{
			name:       "CJK middle of name",
			input:      "ubuntu-中文-jammy", // "ubuntu-中文-jammy"
			wantOutput: "ubuntu-jammy",    // CJK collapses to single "-"
		},
		{
			name:       "emoji single rune",
			input:      "stemcell-\U0001F600-test", // snowman is 3 bytes; emoji is 4 bytes
			wantOutput: "stemcell-test",
		},
		{
			name:       "ascii only passthrough",
			input:      "ubuntu-jammy",
			wantOutput: "ubuntu-jammy",
		},
		{
			name:       "combining mark after ascii",
			input:      "café-test", // "café-test": é (U+00E9) is 2 bytes
			wantOutput: "caf-test",  // 'é' → single '-', collapses with following '-'
		},
		{
			name:  "all non-ascii",
			input: "中文", // two CJK runes, 6 bytes total
			// Each rune → '-', consecutive → collapsed, then trimmed → empty.
			wantOutput: "",
		},
		{
			name:       "mixed ascii and multibyte",
			input:      "v1.à.0", // "v1.à.0" — à (U+00E0) is 2 bytes
			wantOutput: "v1.-.0", // à → '-'; dots are allowed so no collapse across them
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// BuildStemcellFilename calls sanitizeStemcellPart internally.
			// Drive it with version "1.0" and a known sha; check the name segment.
			got := pve.BuildStemcellFilename(tc.input, "1.0", "abcdef1234567890")
			// Output must be ASCII-only.
			for _, r := range got {
				if r > 127 {
					t.Errorf("non-ASCII rune %U in output %q for input %q", r, got, tc.input)
				}
			}
			if tc.wantOutput != "" {
				// The name segment is between "bosh-stemcell-" prefix and
				// the first "-1.0-" (version separator).
				prefix := "bosh-stemcell-"
				versionSep := "-1.0-"
				if !strings.HasPrefix(got, prefix) {
					t.Errorf("output %q missing expected prefix %q", got, prefix)
					return
				}
				after := got[len(prefix):]
				idx := strings.Index(after, versionSep)
				if idx < 0 {
					t.Errorf("output %q missing version separator %q", got, versionSep)
					return
				}
				namePart := after[:idx]
				if namePart != tc.wantOutput {
					t.Errorf("sanitized name = %q; want %q (full output: %q)", namePart, tc.wantOutput, got)
				}
			}
		})
	}
}

// ---- TestIsLegacyIntegerStemcellCID ----

// ---- Light-stemcell CID helpers ----

// ---- Template-stemcell CID helpers ----

// TestParseStemcellCID_RejectsTemplateCID is a regression assertion: the old
// ParseStemcellCID must return an error for "template:6042" so that template
// CIDs are NOT silently misrouted through the legacy volume path.
func TestParseStemcellCID_RejectsTemplateCID(t *testing.T) {
	t.Parallel()
	_, _, err := pve.ParseStemcellCID("template:6042")
	if err == nil {
		t.Fatal("ParseStemcellCID(\"template:6042\") = nil error; want rejection so template CIDs are not misrouted as volume CIDs")
	}
}

// ---- Path-identity stemcell CIDs ----

func TestBuildLightStemcellCID(t *testing.T) {
	t.Parallel()
	got := pve.BuildLightStemcellCID("nfs-stemcells", "bosh-stemcell-ubuntu-1.0-deadbeef.qcow2")
	want := ":light:nfs-stemcells:import/bosh-stemcell-ubuntu-1.0-deadbeef.qcow2"
	if got != want {
		t.Errorf("BuildLightStemcellCID = %q; want %q", got, want)
	}
}

func TestBuildHeavyStemcellCID(t *testing.T) {
	t.Parallel()
	got := pve.BuildHeavyStemcellCID("local-lvm", "bosh-stemcell-ubuntu-1.0-deadbeef.qcow2")
	want := ":heavy:local-lvm:import/bosh-stemcell-ubuntu-1.0-deadbeef.qcow2"
	if got != want {
		t.Errorf("BuildHeavyStemcellCID = %q; want %q", got, want)
	}
}

func TestIsStemcellPathCID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		cid  string
		want bool
	}{
		{":light:nfs:import/foo.qcow2", true},
		{":heavy:nfs:import/foo.qcow2", true},
		{":garbage", true}, // predicate only checks the discriminator
		{"light:nfs:import/foo.qcow2", false},
		{"template:6042", false},
		{"nfs:import/foo.qcow2", false},
		{"5042", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := pve.IsStemcellPathCID(tc.cid); got != tc.want {
			t.Errorf("IsStemcellPathCID(%q) = %v; want %v", tc.cid, got, tc.want)
		}
	}
}

func TestParseStemcellPathCID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		cid         string
		wantKind    pve.StemcellKind
		wantStorage string
		wantVolPath string
		wantErr     string
	}{
		{
			name:        "valid light",
			cid:         ":light:nfs-stemcells:import/bosh-stemcell-ubuntu-1.0-deadbeef.qcow2",
			wantKind:    pve.StemcellKindLight,
			wantStorage: "nfs-stemcells",
			wantVolPath: "import/bosh-stemcell-ubuntu-1.0-deadbeef.qcow2",
		},
		{
			name:        "valid heavy",
			cid:         ":heavy:local-lvm:import/foo.qcow2",
			wantKind:    pve.StemcellKindHeavy,
			wantStorage: "local-lvm",
			wantVolPath: "import/foo.qcow2",
		},
		{
			name:        "storage pool literally named light",
			cid:         ":light:light:import/foo.qcow2",
			wantKind:    pve.StemcellKindLight,
			wantStorage: "light",
			wantVolPath: "import/foo.qcow2",
		},
		{
			name:        "storage pool literally named heavy under heavy kind",
			cid:         ":heavy:heavy:import/foo.qcow2",
			wantKind:    pve.StemcellKindHeavy,
			wantStorage: "heavy",
			wantVolPath: "import/foo.qcow2",
		},
		{name: "empty", cid: "", wantErr: "empty CID"},
		{name: "retired light prefix without leading colon", cid: "light:nfs:import/foo.qcow2", wantErr: "missing leading ':'"},
		{name: "retired template CID", cid: "template:6042", wantErr: "missing leading ':'"},
		{name: "bare volid form", cid: "nfs:import/foo.qcow2", wantErr: "missing leading ':'"},
		{name: "retired integer CID", cid: "5042", wantErr: "missing leading ':'"},
		{name: "unknown kind", cid: ":medium:nfs:import/foo.qcow2", wantErr: "unknown kind"},
		{name: "bare light prefix no payload", cid: ":light:", wantErr: "payload invalid"},
		{name: "doubled prefix", cid: ":light::heavy:nfs:import/foo.qcow2", wantErr: "empty storage segment"},
		{name: "payload missing import path", cid: ":light:nfs:iso/foo.iso", wantErr: "does not start with \"import/\""},
		{name: "payload missing colon", cid: ":heavy:justastring", wantErr: "payload invalid"},
		{name: "uppercase kind rejected", cid: ":Light:nfs:import/foo.qcow2", wantErr: "unknown kind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kind, storage, volPath, err := pve.ParseStemcellPathCID(tc.cid)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseStemcellPathCID(%q) = (%q, %q, %q, nil); want error containing %q", tc.cid, kind, storage, volPath, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ParseStemcellPathCID(%q) error %q; want it to contain %q", tc.cid, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseStemcellPathCID(%q) unexpected error: %v", tc.cid, err)
			}
			if kind != tc.wantKind || storage != tc.wantStorage || volPath != tc.wantVolPath {
				t.Errorf("ParseStemcellPathCID(%q) = (%q, %q, %q); want (%q, %q, %q)",
					tc.cid, kind, storage, volPath, tc.wantKind, tc.wantStorage, tc.wantVolPath)
			}
		})
	}
}

func TestParseStemcellPathCID_RoundTrip(t *testing.T) {
	t.Parallel()
	filename := pve.BuildStemcellFilename("ubuntu-jammy", "1.719", "cafebabe00112233")
	for _, build := range []struct {
		kind pve.StemcellKind
		cid  string
	}{
		{pve.StemcellKindLight, pve.BuildLightStemcellCID("nfs-a", filename)},
		{pve.StemcellKindHeavy, pve.BuildHeavyStemcellCID("nfs-a", filename)},
	} {
		kind, storage, volPath, err := pve.ParseStemcellPathCID(build.cid)
		if err != nil {
			t.Fatalf("round-trip %q: %v", build.cid, err)
		}
		if kind != build.kind || storage != "nfs-a" || volPath != "import/"+filename {
			t.Errorf("round-trip %q = (%q, %q, %q)", build.cid, kind, storage, volPath)
		}
	}
}
