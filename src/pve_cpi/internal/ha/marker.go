package ha

import (
	"strings"
)

// MarkerPrefix opens the ownership marker the CPI writes into the comment of
// every HA rule, HA resource, and replication job it creates. The version lets
// a later format change be told apart from this one.
const MarkerPrefix = "bosh-cpi:v1"

// maxMarkerDecodeRounds caps the rounds of PVE's comment encoding that
// MarkerFields undoes. PVE adds one round per extra write of replication.cfg
// from the same parsed config, so a real comment carries a handful at most,
// and the cap keeps a hostile comment from costing more than a few passes.
const maxMarkerDecodeRounds = 8

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
// the prefix needs undoes exactly one such encoding over the whole comment:
// "%25" becomes '%' and "%3A" becomes ':', the only escapes encode_text writes
// for the characters the CPI puts in a marker. Decoding stops once the prefix
// reads MarkerPrefix, once a round changes nothing, or after
// maxMarkerDecodeRounds rounds, and a comment whose prefix still differs is
// not the CPI's. No other escape is decoded, so a foreign comment that merely
// holds a '%' or another escape never reads as a marker.
//
// encoded is true when the prefix needed at least one round, which means PVE
// rewrote the comment and an older CPI no longer recognizes it.
func MarkerFields(comment string) (fields []string, encoded, ok bool) {
	all := strings.Fields(comment)
	for range maxMarkerDecodeRounds {
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

// DecodedMarkerComment returns comment with the rounds of PVE's encoding that
// MarkerFields undoes removed, so a rendered marker can be compared with it.
// A comment that holds no marker is returned as it is.
func DecodedMarkerComment(comment string) string {
	fields, _, ok := MarkerFields(comment)
	if !ok {
		return comment
	}
	return strings.Join(append([]string{MarkerPrefix}, fields...), " ")
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
