package ha

import (
	"slices"
	"strings"
	"testing"
)

const markerTestOwner = "a1d3f809-511b-4ed5-a41f-ef891aa8b4a9"

// encodeTextOnce applies one round of PVE's encode_text to the characters a
// marker can hold, the way write_config encodes a comment it was given.
func encodeTextOnce(s string) string {
	return strings.NewReplacer("%", "%25", ":", "%3A").Replace(s)
}

// encodeTextRounds applies n rounds of encodeTextOnce.
func encodeTextRounds(s string, n int) string {
	for range n {
		s = encodeTextOnce(s)
	}
	return s
}

func TestMarkerFieldsUndoesRepeatedEncoding(t *testing.T) {
	t.Parallel()
	canonical := MarkerPrefix + " owner=" + markerTestOwner + " gen=12"
	cases := map[string]struct {
		comment string
		encoded bool
	}{
		"canonical API form":                   {canonical, false},
		"one round, a healthy file form":       {encodeTextRounds(canonical, 1), true},
		"two rounds, the API form after three": {encodeTextRounds(canonical, 2), true},
		"three rounds, the file form after three writes": {
			encodeTextRounds(canonical, 3), true,
		},
		"lab API form": {"bosh-cpi%253Av1 owner=" + markerTestOwner + " gen=12", true},
		"lab file form": {
			"bosh-cpi%25253Av1 owner=" + markerTestOwner + " gen=12", true,
		},
		"lower-case escape":   {"bosh-cpi%253av1 owner=" + markerTestOwner + " gen=12", true},
		"eight rounds at cap": {encodeTextRounds(canonical, maxMarkerDecodeRounds), true},
	}
	want := []string{"owner=" + markerTestOwner, "gen=12"}
	for name, tc := range cases {
		fields, encoded, ok := MarkerFields(tc.comment)
		if !ok || encoded != tc.encoded || !slices.Equal(fields, want) {
			t.Errorf("%s: MarkerFields(%q) = %q, encoded %v, ok %v; want %q, encoded %v", name, tc.comment, fields,
				encoded, ok, want, tc.encoded)
		}
		if got := DecodedMarkerComment(tc.comment); got != canonical {
			t.Errorf("%s: DecodedMarkerComment(%q) = %q, want %q", name, tc.comment, got, canonical)
		}
	}
}

func TestMarkerFieldsRefusesForeignComments(t *testing.T) {
	t.Parallel()
	for _, comment := range []string{
		"",
		"   ",
		"nightly replication",
		"bosh-cpi:v2 owner=u gen=1",
		"bosh-cpi%3Av2 owner=u gen=1",
		"bosh-cpi%253Av2 owner=u gen=1",
		"100% bosh-cpi:v1 owner=u gen=1",
		"50%25 done",
		"bosh-cpi%3Av1%25 owner=u gen=1",
		"bosh-cpi:v1x owner=u gen=1",
		"bosh-cpi%3Av1x owner=u gen=1",
		// encode_text never writes %2D for '-', so a comment that needs it is
		// not one PVE encoded from the CPI's marker.
		"bosh%2Dcpi%3Av1 owner=u gen=1",
		"bosh-cpi%3av1%3A owner=u gen=1",
		"other bosh-cpi%3Av1 owner=u gen=1",
		"%",
		"%2",
		"bosh-cpi%3",
		encodeTextRounds(MarkerPrefix+" owner=u gen=1", maxMarkerDecodeRounds+1),
	} {
		if fields, encoded, ok := MarkerFields(comment); ok {
			t.Errorf("MarkerFields(%q) = %q, encoded %v, want no marker", comment, fields, encoded)
		}
		if got := DecodedMarkerComment(comment); got != comment {
			t.Errorf("DecodedMarkerComment(%q) = %q, want it unchanged", comment, got)
		}
	}
}

func TestDecodeMarkerRoundIsOneLeftToRightPass(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"bosh-cpi%25253Av1": "bosh-cpi%253Av1",
		"bosh-cpi%253Av1":   "bosh-cpi%3Av1",
		"bosh-cpi%3Av1":     "bosh-cpi:v1",
		"a%2525b":           "a%25b",
		"%3A%3a":            "::",
		"keep%2Dthis":       "keep%2Dthis",
		"tail%":             "tail%",
		"tail%2":            "tail%2",
	} {
		if got, _ := decodeMarkerRound(in); got != want {
			t.Errorf("decodeMarkerRound(%q) = %q, want %q", in, got, want)
		}
	}
}
