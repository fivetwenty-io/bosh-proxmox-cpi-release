// Package handlers internal tests for the delete_stemcell prune gate.
package handlers

import "testing"

// TestDirectorRefsAllowPrune pins the scope contract of the orphan prune's
// per-candidate gate: an empty ref set always allows; a sole ref equal to the
// calling director allows only when the candidate's provenance sha8 matches
// the stemcell being deleted (a sole-ref template for a different stemcell is
// a healthy cache this director still has registered); any other non-empty
// set refuses, because some director still actively references the template.
func TestDirectorRefsAllowPrune(t *testing.T) {
	t.Parallel()
	const dir = "dir-a"
	const sha = "abcd1234"

	cases := []struct {
		name      string
		refs      []string
		provSHA8  string
		deleteSHA string
		want      bool
	}{
		{"empty refs always allow", nil, "ffff0000", sha, true},
		{"sole own ref with matching sha8 allows", []string{dir}, sha, sha, true},
		{"sole own ref with different sha8 refuses", []string{dir}, "ffff0000", sha, false},
		{"sole own ref with empty delete sha8 refuses", []string{dir}, sha, "", false},
		{"sole foreign ref refuses", []string{"dir-b"}, sha, sha, false},
		{"own plus foreign ref refuses", []string{dir, "dir-b"}, sha, sha, false},
		{"duplicate own refs refuse (not a sole ref)", []string{dir, dir}, sha, sha, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			prov := stemcellProvenance{SHA8: tc.provSHA8, DirectorRefs: tc.refs}
			if got := directorRefsAllowPrune(prov, dir, tc.deleteSHA); got != tc.want {
				t.Errorf("directorRefsAllowPrune(refs=%v, provSHA8=%q, deleteSHA8=%q) = %v; want %v",
					tc.refs, tc.provSHA8, tc.deleteSHA, got, tc.want)
			}
		})
	}
}
