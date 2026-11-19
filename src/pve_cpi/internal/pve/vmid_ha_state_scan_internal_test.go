package pve

import (
	"encoding/json"
	"slices"
	"testing"
)

// TestHARuleResourceSIDs_ReadsEachShapeTheFieldTakes proves a rule's
// resources field is read as a comma-separated string or as a list, that an
// absent or null field lists none, and that any other shape is an error, so
// the allocation fails closed rather than missing a VMID.
func TestHARuleResourceSIDs_ReadsEachShapeTheFieldTakes(t *testing.T) {
	for name, tc := range map[string]struct {
		raw     string
		want    []string
		wantErr bool
	}{
		"absent":                {raw: "", want: nil},
		"null":                  {raw: "null", want: nil},
		"a comma string":        {raw: `"vm:100, vm:101,,ct:102"`, want: []string{"vm:100", "vm:101", "ct:102"}},
		"a list":                {raw: `["vm:100","ct:102"]`, want: []string{"vm:100", "ct:102"}},
		"a list with no string": {raw: `["vm:100",7]`, wantErr: true},
		"a number":              {raw: `7`, wantErr: true},
		"broken JSON":           {raw: `"vm:100`, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := haRuleResourceSIDs(json.RawMessage(tc.raw))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err %v, want an error %v", err, tc.wantErr)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
