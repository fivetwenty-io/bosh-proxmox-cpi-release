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
		"eight rounds at cap": {encodeTextRounds(canonical, MaxMarkerDecodeRounds), true},
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
		encodeTextRounds(MarkerPrefix+" owner=u gen=1", MaxMarkerDecodeRounds+1),
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

// roundByRoundMarkerFields is MarkerFields as a plain loop: it undoes one
// round over the whole comment at a time until the first field reads
// MarkerPrefix, a round changes nothing, or MaxMarkerDecodeRounds rounds ran.
// MarkerFields must agree with it on every input.
func roundByRoundMarkerFields(comment string) (fields []string, encoded, ok bool) {
	all := strings.Fields(comment)
	for range MaxMarkerDecodeRounds {
		if len(all) == 0 || all[0] == MarkerPrefix {
			break
		}
		next, changed := decodeMarkerRound(comment)
		if !changed {
			break
		}
		comment, encoded = next, true
		all = strings.Fields(comment)
	}
	if len(all) == 0 || all[0] != MarkerPrefix {
		return nil, false, false
	}
	return all[1:], encoded, true
}

func TestMarkerFieldsReadsMoreRoundsThanOneFailoverPassAdds(t *testing.T) {
	t.Parallel()
	canonical := MarkerPrefix + " owner=" + markerTestOwner + " gen=7"
	want := []string{"owner=" + markerTestOwner, "gen=7"}
	// Eight rounds was the old cap. One pass that steals ten guests onto a
	// node adds nine, and passes add up until the CPI writes the marker back.
	for _, rounds := range []int{9, 10, 17, 64, 500, MaxMarkerDecodeRounds - 1, MaxMarkerDecodeRounds} {
		comment := encodeTextRounds(canonical, rounds)
		fields, encoded, ok := MarkerFields(comment)
		if !ok || !encoded || !slices.Equal(fields, want) {
			t.Errorf("%d rounds: MarkerFields = %q, encoded %v, ok %v; want %q", rounds, fields, encoded, ok, want)
		}
		if got := DecodedMarkerComment(comment); got != canonical {
			t.Errorf("%d rounds: DecodedMarkerComment = %q, want %q", rounds, got, canonical)
		}
		if m, ok := ParseReplicaJobMarker(comment); !ok || !m.Encoded || m.Owner != markerTestOwner ||
			m.Generation != 7 {
			t.Errorf("%d rounds: ParseReplicaJobMarker = %+v, %v", rounds, m, ok)
		}
	}
}

func TestMarkerFieldsAgreesWithTheRoundByRoundDecoder(t *testing.T) {
	t.Parallel()
	canonical := MarkerPrefix + " owner=" + markerTestOwner + " gen=3"
	inputs := []string{
		"", " ", MarkerPrefix, canonical, "bosh-cpi%3av1 x", "bosh-cpi%253av1 x", "bosh-cpi%253Av1%25 x",
		"bosh-cpi%2525 x", "bosh-cpi%3A x", "bosh-cpi%v1", "bosh-cpi%3Av1x", "bosh-cpi%25%3Av1", "bosh-cpi%2:v1",
		"bosh-cpi%253A%3Av1", "bosh-cpi%25253Av1 a%25253A b%2525%3A c%2D", "bosh-cpi%253Av1 x:%25 %2525253A",
		"xbosh-cpi%3Av1", "bosh-cpi:v1 owner=%253A gen=1", "bosh-cpi%3Av1\towner=u\ngen=1",
		encodeTextRounds(canonical+" note=a:b%c", 5), encodeTextRounds(canonical, MaxMarkerDecodeRounds+1),
	}
	for _, in := range inputs {
		gotFields, gotEnc, gotOK := MarkerFields(in)
		wantFields, wantEnc, wantOK := roundByRoundMarkerFields(in)
		if gotOK != wantOK || gotEnc != wantEnc || !slices.Equal(gotFields, wantFields) {
			t.Errorf("MarkerFields(%q) = %q, %v, %v; the round-by-round decoder gives %q, %v, %v", in, gotFields,
				gotEnc, gotOK, wantFields, wantEnc, wantOK)
		}
	}
}

func FuzzMarkerFieldsAgreesWithTheRoundByRoundDecoder(f *testing.F) {
	for _, seed := range []string{
		"bosh-cpi%253Av1 owner=u gen=1", "bosh-cpi%3av1 a%2525 b%3A", "bosh-cpi%25%253Av1", "100% bosh-cpi:v1",
		"bosh-cpi%2525253Av1 owner=u%253A gen=2 x=%25%25",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		gotFields, gotEnc, gotOK := MarkerFields(in)
		wantFields, wantEnc, wantOK := roundByRoundMarkerFields(in)
		if gotOK != wantOK || gotEnc != wantEnc || !slices.Equal(gotFields, wantFields) {
			t.Fatalf("MarkerFields(%q) = %q, %v, %v; the round-by-round decoder gives %q, %v, %v", in, gotFields,
				gotEnc, gotOK, wantFields, wantEnc, wantOK)
		}
	})
}

func TestMarkerFieldsBoundsAHostileComment(t *testing.T) {
	t.Parallel()
	// A first field one round past the cap is no marker, however long the
	// comment is, and costs a single scan.
	past := "bosh-cpi%" + strings.Repeat("25", MaxMarkerDecodeRounds) + "3Av1"
	if _, _, ok := MarkerFields(past + " " + strings.Repeat("%2525", 1<<16)); ok {
		t.Error("a prefix one round past the cap read as a marker")
	}
	// A first field of the wrong shape is no marker either, with no decoding.
	if _, _, ok := MarkerFields(strings.Repeat("%25", 1<<18)); ok {
		t.Error("a run of escapes read as a marker")
	}
	// A prefix at the cap with a nested escape in another field decodes that
	// field exactly as many rounds as the prefix needed, and no more.
	atCap := "bosh-cpi%" + strings.Repeat("25", MaxMarkerDecodeRounds-1) + "3Av1"
	nested := "x=%" + strings.Repeat("25", MaxMarkerDecodeRounds+5) + "3A"
	fields, encoded, ok := MarkerFields(atCap + " owner=u gen=1 " + nested)
	wantNested := "x=%" + strings.Repeat("25", 5) + "3A"
	if !ok || !encoded || len(fields) != 3 || fields[2] != wantNested {
		t.Errorf("MarkerFields at the cap = %q, %v, %v; want the nested field %q", fields, encoded, ok, wantNested)
	}
}

func TestParseReplicaJobMarkerRefusesAMarkerItCannotVouchFor(t *testing.T) {
	t.Parallel()
	for _, comment := range []string{
		MarkerPrefix + " owner=u",
		MarkerPrefix + " gen=1",
		MarkerPrefix + " owner=" + MarkerUnset + " gen=1",
		MarkerPrefix + " owner= gen=1",
		MarkerPrefix + " owner=u gen=0",
		MarkerPrefix + " owner=u gen=-1",
		MarkerPrefix + " owner=u gen=1 gen=1",
		MarkerPrefix + " owner=u owner=u gen=1",
		encodeTextRounds(MarkerPrefix+" owner=u gen=1", MaxMarkerDecodeRounds+1),
	} {
		if m, ok := ParseReplicaJobMarker(comment); ok {
			t.Errorf("ParseReplicaJobMarker(%q) = %+v, want no marker", comment, m)
		}
	}
	m, ok := ParseReplicaJobMarker(MarkerPrefix + " owner=u gen=5 later=field")
	if !ok || m.Owner != "u" || m.Generation != 5 || m.Encoded {
		t.Errorf("plain marker parsed as %+v, %v", m, ok)
	}
}
