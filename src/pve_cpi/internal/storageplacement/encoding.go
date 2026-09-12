package storageplacement

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"unicode/utf8"
)

// EncodeTuple is the v1 protocol: UTF-8 fields, each preceded by a uint32
// big-endian byte length. No delimiters or Unicode normalization are applied.
func EncodeTuple(fields ...string) ([]byte, error) {
	var size uint64
	lengths := make([]uint32, len(fields))
	for i, field := range fields {
		if !utf8.ValidString(field) {
			return nil, fmt.Errorf("tuple field is not UTF-8")
		}
		length := uint64(len(field))
		if length > math.MaxUint32 {
			return nil, fmt.Errorf("tuple field exceeds uint32 length")
		}
		lengths[i] = uint32(length)
		size += 4 + length
		if size > uint64(int(^uint(0)>>1)) {
			return nil, fmt.Errorf("tuple exceeds addressable length")
		}
	}
	encoded := make([]byte, 0, int(size))
	for i, field := range fields {
		encoded = binary.BigEndian.AppendUint32(encoded, lengths[i])
		encoded = append(encoded, field...)
	}
	return encoded, nil
}

// The v1 tuple begins with algorithm/vN, namespace, allocation key, and group.
// Rendezvous appends the canonical backing key. Weighted members append
// (member, backing key); domains append (domain, declared|backing, identity).
// These tags keep operator domain names separate from implicit backing domains.
func tuple(r RequestSnapshot, suffix ...string) ([]byte, error) {
	fields := make([]string, 0, 4+len(suffix))
	fields = append(fields, r.Policy.Name+"/v"+strconv.Itoa(r.Policy.Version), r.Namespace, r.AllocationKey, r.AllocationGroup)
	return EncodeTuple(append(fields, suffix...)...)
}

func rendezvous(r RequestSnapshot, backing string) ([32]byte, error) {
	encoded, err := tuple(r, backing)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}

func weightedScore(r RequestSnapshot, weight uint64, suffix ...string) (float64, error) {
	if weight == 0 {
		return 0, fmt.Errorf("weighted draw requires positive weight")
	}
	encoded, err := tuple(r, suffix...)
	if err != nil {
		return 0, err
	}
	h := hmac.New(sha256.New, r.Seed[:])
	// hash.Hash.Write never returns an error.
	_, _ = h.Write(encoded)
	digest := h.Sum(nil)
	x := binary.BigEndian.Uint64(digest[:8]) >> 12
	u := float64(x+1) / float64((uint64(1)<<52)+1)
	score := -math.Log(u) / float64(weight)
	if u <= 0 || u >= 1 || score <= 0 || math.IsNaN(score) || math.IsInf(score, 0) {
		return 0, fmt.Errorf("invalid weighted draw")
	}
	return score, nil
}
