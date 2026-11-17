package ha

import (
	"strconv"
	"strings"
)

// MarkerPrefix opens the ownership marker the CPI writes into the comment of
// every HA rule, HA resource, and replication job it creates. The version lets
// a later format change be told apart from this one.
const MarkerPrefix = "bosh-cpi:v1"

// MarkerUnset is the value a marker field carries when the CPI had nothing to
// put there, so the key stays visible.
const MarkerUnset = "-"

// MaxMarkerDecodeRounds caps the rounds of PVE's comment encoding that
// MarkerFields undoes. PVE adds one round to every job's comment for each
// extra write of replication.cfg in one pvescheduler pass, which means one
// round for every replicated guest after the first that a failover steals
// onto the same node, and the rounds pile up over passes until the CPI writes
// the plain marker back. Each round makes the prefix two characters longer,
// so the prefix after n rounds is 2n+11 characters, and PVE's API takes a
// comment of at most 4096 characters. 2048 rounds is therefore more than any
// comment PVE accepts can carry, so the cap never cuts off a marker PVE still
// stores, and it bounds the work a hostile comment can cost (see
// MarkerFields).
const MaxMarkerDecodeRounds = 2048

// MarkerFields reads the ownership marker at the start of comment. ok is true
// when the first whitespace-separated field is MarkerPrefix, and fields are
// the comment's other fields, which a caller parses as key=value pairs.
//
// PVE stores a replication job's comment in replication.cfg through
// encode_text, which percent-encodes ':' and '%', and the API decodes it once.
// PVE's write_config encodes the comments of the config object it was given
// in place, so a pvescheduler pass that writes the file more than once from
// one parsed object, as when HA recovers several replicated guests onto one
// node, encodes every job's comment again on each extra write. The marker
// then reads "bosh-cpi%3Av1" or "bosh-cpi%253Av1" through the API. Each round
// undoes exactly one such encoding over the whole comment: "%25" becomes '%'
// and "%3A" becomes ':', the only escapes encode_text writes for the
// characters the CPI puts in a marker. No other escape is decoded, so a
// foreign comment that merely holds a '%' or another escape never reads as a
// marker.
//
// The first field alone decides how many rounds the comment carries, because
// the only string that n rounds of that decoding turn into MarkerPrefix is
// "bosh-cpi%" followed by n-1 repeats of "25" and then "3Av1" (see
// markerPrefixRounds). A first field of any other shape is not the CPI's and
// costs one scan, with no decoding at all. The other fields then get the same
// number of rounds, and each one stops early once a round changes nothing,
// which for a real marker is its first round. Decoding never adds or removes
// whitespace, so the fields can be decoded one at a time. The work is at most
// MaxMarkerDecodeRounds scans of the comment.
//
// encoded is true when the prefix needed at least one round, which means PVE
// rewrote the comment and an older CPI no longer recognizes it.
func MarkerFields(comment string) (fields []string, encoded, ok bool) {
	all := strings.Fields(comment)
	if len(all) == 0 {
		return nil, false, false
	}
	rounds, ok := markerPrefixRounds(all[0])
	if !ok {
		return nil, false, false
	}
	fields = all[1:]
	for i, f := range fields {
		fields[i] = decodeMarkerRounds(f, rounds)
	}
	return fields, rounds > 0, true
}

// DecodedMarkerComment returns comment with the rounds of PVE's encoding that
// MarkerFields undoes removed, so a rendered marker can be compared with it.
// It is also the plain form the CPI writes back over an encoded marker and
// the form the doctor's repair command names. A comment that holds no marker
// is returned as it is.
func DecodedMarkerComment(comment string) string {
	fields, _, ok := MarkerFields(comment)
	if !ok {
		return comment
	}
	return strings.Join(append([]string{MarkerPrefix}, fields...), " ")
}

// markerPrefixRounds reports how many rounds of encode_text separate field
// from MarkerPrefix, and false when no number of rounds up to
// MaxMarkerDecodeRounds does. encode_text turns the prefix's one ':' into
// "%3A", and each later round turns that escape's '%' into "%25", so n rounds
// leave "bosh-cpi%", then n-1 repeats of "25", then "3Av1". decodeMarkerRound
// reverses exactly that and nothing else. A '%' it leaves must come from
// "%25", because a literal '%' before "3A" decodes to ':' in the same round,
// and the letters around the escape cannot come from an escape at all. So
// this shape is the only one that decodes to the prefix, and the escape may
// read "3a" because the decoder accepts either case.
func markerPrefixRounds(field string) (int, bool) {
	if field == MarkerPrefix {
		return 0, true
	}
	head, tail, _ := strings.Cut(MarkerPrefix, ":")
	escapes, ok := strings.CutPrefix(field, head+"%")
	if !ok {
		return 0, false
	}
	if escapes, ok = strings.CutSuffix(escapes, tail); !ok || len(escapes) < 2 {
		return 0, false
	}
	if !strings.EqualFold(escapes[len(escapes)-2:], "3A") {
		return 0, false
	}
	pad := escapes[:len(escapes)-2]
	if len(pad)%2 != 0 || len(pad)/2+1 > MaxMarkerDecodeRounds {
		return 0, false
	}
	for i := 0; i < len(pad); i += 2 {
		if pad[i:i+2] != "25" {
			return 0, false
		}
	}
	return len(pad)/2 + 1, true
}

// decodeMarkerRounds applies up to rounds rounds of decodeMarkerRound to s,
// and stops early once a round changes nothing, because no later round can.
func decodeMarkerRounds(s string, rounds int) string {
	for range rounds {
		next, changed := decodeMarkerRound(s)
		if !changed {
			break
		}
		s = next
	}
	return s
}

// decodeMarkerRound undoes one round of encode_text for the escapes a marker
// can carry, scanning left to right the way decode_text does, so "%25253A"
// becomes "%253A" and not "%3A". changed is false when s holds no such escape.
func decodeMarkerRound(s string) (string, bool) {
	if !strings.Contains(s, "%") {
		return s, false
	}
	var b strings.Builder
	b.Grow(len(s))
	changed := false
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			switch esc := strings.ToUpper(s[i+1 : i+3]); esc {
			case "25":
				b.WriteByte('%')
				i += 2
				changed = true
				continue
			case "3A":
				b.WriteByte(':')
				i += 2
				changed = true
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String(), changed
}

// ReplicaJobMarker is the ownership marker in the comment of a replication job
// the CPI created, "bosh-cpi:v1 owner=<director-uuid> gen=<n>".
type ReplicaJobMarker struct {
	// Owner is the marker's owner field, the Director UUID as the CPI wrote it.
	Owner string
	// Generation is the CPI's generation of the job, which the allocation
	// journal records before the CPI writes the comment.
	Generation uint64
	// Encoded is true when PVE percent-encoded the comment again after the
	// CPI wrote it, so the marker reads plainly only once decoded.
	Encoded bool
}

// ParseReplicaJobMarker reads the ownership marker at the start of a
// replication job's comment. It is the one reading of a job's marker that the
// CPI and the doctor share, so they agree on which jobs carry a marker and on
// which of those PVE encoded again. It reports false when the comment carries
// no marker, a marker without an owner or a generation, a repeated owner or
// generation, or a generation that is not a positive integer, because each of
// those could be a reused job ID or a hand edit, and neither proves the job is
// one the CPI made. Unknown keys are ignored so a later field does not orphan
// the job. The marker is read through MarkerFields, so a comment PVE encoded
// again still reads as the marker it holds, and Encoded says so.
func ParseReplicaJobMarker(comment string) (ReplicaJobMarker, bool) {
	fields, encoded, ok := MarkerFields(comment)
	if !ok {
		return ReplicaJobMarker{}, false
	}
	marker := ReplicaJobMarker{Encoded: encoded}
	seenOwner, seenGeneration := false, false
	for _, field := range fields {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch key {
		case "owner":
			if seenOwner || value == "" || value == MarkerUnset {
				return ReplicaJobMarker{}, false
			}
			seenOwner, marker.Owner = true, value
		case "gen":
			generation, err := strconv.ParseUint(value, 10, 64)
			if seenGeneration || err != nil || generation == 0 {
				return ReplicaJobMarker{}, false
			}
			seenGeneration, marker.Generation = true, generation
		}
	}
	if !seenOwner || !seenGeneration {
		return ReplicaJobMarker{}, false
	}
	return marker, true
}
