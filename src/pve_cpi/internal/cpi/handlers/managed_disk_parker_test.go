package handlers

import "testing"

// TestManagedDiskFieldMatches_TagsOrderInsensitive covers the readback
// comparison's tags special case: PVE is free to reorder a tag string it
// stores, so a reordered readback must still match, while a readback that
// drops or adds a tag must still mismatch.
func TestManagedDiskFieldMatches_TagsOrderInsensitive(t *testing.T) {
	for _, tc := range []struct {
		name       string
		got, want  string
		wantsMatch bool
	}{
		{
			name:       "identical order matches",
			got:        "bosh-cpi;bosh-parker;director--d1;vm-prefix--bosh",
			want:       "bosh-cpi;bosh-parker;director--d1;vm-prefix--bosh",
			wantsMatch: true,
		},
		{
			name:       "PVE-reordered readback still matches",
			got:        "bosh-cpi;bosh-parker;director--d1;vm-prefix--bosh",
			want:       "vm-prefix--bosh;director--d1;bosh-cpi;bosh-parker",
			wantsMatch: true,
		},
		{
			name:       "mover tags reordered still match",
			got:        "bosh-cpi;bosh-disk-mover;bosh-parker;director--d1;vm-prefix--bosh",
			want:       "bosh-cpi;bosh-parker;director--d1;vm-prefix--bosh;bosh-disk-mover",
			wantsMatch: true,
		},
		{
			name:       "genuinely different tag set mismatches",
			got:        "bosh-cpi;bosh-parker;director--d1;vm-prefix--bosh",
			want:       "bosh-cpi;bosh-parker;director--d2;vm-prefix--bosh",
			wantsMatch: false,
		},
		{
			name:       "missing tag mismatches",
			got:        "bosh-cpi;bosh-parker;director--d1",
			want:       "bosh-cpi;bosh-parker;director--d1;vm-prefix--bosh",
			wantsMatch: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := managedDiskFieldMatches(jsonKeyTags, tc.got, tc.want); got != tc.wantsMatch {
				t.Fatalf("managedDiskFieldMatches(%q, %q) = %v, want %v", tc.got, tc.want, got, tc.wantsMatch)
			}
		})
	}
}

// TestManagedDiskFieldMatches_NonTagsKeyIsExact covers that every key other
// than tags keeps the prior exact-scalar comparison: reordering is not
// tolerated there, because a reordered non-tag value is a real difference.
func TestManagedDiskFieldMatches_NonTagsKeyIsExact(t *testing.T) {
	for _, tc := range []struct {
		name       string
		key        string
		got, want  any
		wantsMatch bool
	}{
		{name: "matching name", key: "name", got: "bosh-parker-90000", want: "bosh-parker-90000", wantsMatch: true},
		{name: "differing name", key: "name", got: "bosh-parker-90000", want: "acme-parker-90000", wantsMatch: false},
		{name: "onboot bool vs scalar zero", key: "onboot", got: false, want: 0, wantsMatch: true},
		{name: "protection bool vs scalar one", key: "protection", got: true, want: 1, wantsMatch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := managedDiskFieldMatches(tc.key, tc.got, tc.want); got != tc.wantsMatch {
				t.Fatalf("managedDiskFieldMatches(%q, %v, %v) = %v, want %v", tc.key, tc.got, tc.want, got, tc.wantsMatch)
			}
		})
	}
}

// TestSortedTagString covers the split/sort/rejoin helper directly.
func TestSortedTagString(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{name: "already sorted", in: "bosh-cpi;bosh-parker", want: "bosh-cpi;bosh-parker"},
		{name: "reordered", in: "bosh-parker;bosh-cpi", want: "bosh-cpi;bosh-parker"},
		{name: "single tag", in: "bosh-cpi", want: "bosh-cpi"},
		{name: "empty string", in: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sortedTagString(tc.in); got != tc.want {
				t.Fatalf("sortedTagString(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
