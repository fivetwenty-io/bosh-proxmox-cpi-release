package pve

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
)

// A description write that carries the digest of the read it was built from
// lands exactly as sent, because qemu-server checks that digest under the VM's
// lock before it writes. Another request can still write the same description
// before our readback, and that request's records are none of our business.
// The checks below therefore confirm only what our own write changed, and they
// ignore every record the write left as it found it.

// ParkerRecordLanded reports whether current carries the bosh_parked_disks
// record under key exactly as sent does, or lacks it as sent does. Every other
// record, the sentinel's other keys, and the text outside the sentinel are
// ignored, because another holder may have written them after our write.
func ParkerRecordLanded(sent, current, key string) bool {
	sentRecord, sentHas := parkerRecordRaw(sent, key)
	currentRecord, currentHas := parkerRecordRaw(current, key)
	if sentHas != currentHas {
		return false
	}
	return !sentHas || jsonValuesEqual(sentRecord, currentRecord)
}

// parkerRecordRaw returns the raw bosh_parked_disks record under key in desc,
// and whether desc carries one. A sentinel whose parked-disk map won't decode
// carries none.
func parkerRecordRaw(desc, key string) (json.RawMessage, bool) {
	_, raw := ParseSentinel(desc)
	entries, err := sentinelEntries(raw["bosh_parked_disks"])
	if err != nil {
		return nil, false
	}
	record, ok := entries[key]
	return record, ok
}

// DescriptionWriteLanded reports whether current carries every change that a
// write of sent made to before. before is the description the write was built
// from, read at the digest the write carried. The text outside the sentinel
// counts only when the write changed it. A sentinel key whose value is a map
// of records, such as bosh_parked_disks or bosh_disk_allocations, counts
// record by record: each record the write added or changed must read exactly
// as sent, and each record it removed must be gone. A sentinel key with any
// other value counts as a whole when the write changed it. Records and keys
// the write left alone may read anything, because another writer may have
// changed them since.
func DescriptionWriteLanded(before, sent, current string) bool {
	beforeText, beforeRaw := ParseSentinel(before)
	sentText, sentRaw := ParseSentinel(sent)
	currentText, currentRaw := ParseSentinel(current)
	if sentText != beforeText && currentText != sentText {
		return false
	}
	keys := make(map[string]bool, len(beforeRaw)+len(sentRaw))
	for key := range beforeRaw {
		keys[key] = true
	}
	for key := range sentRaw {
		keys[key] = true
	}
	for key := range keys {
		beforeValue, beforeHas := beforeRaw[key]
		sentValue, sentHas := sentRaw[key]
		if beforeHas == sentHas && (!sentHas || jsonValuesEqual(beforeValue, sentValue)) {
			continue
		}
		currentValue, currentHas := currentRaw[key]
		if !sentinelKeyLanded(beforeValue, sentValue, currentValue, sentHas, currentHas) {
			return false
		}
	}
	return true
}

// sentinelKeyLanded reports whether currentValue carries the change a write
// made to one sentinel key, from beforeValue to sentValue. A value that is
// absent reads as an empty map of records.
func sentinelKeyLanded(beforeValue, sentValue, currentValue json.RawMessage, sentHas, currentHas bool) bool {
	beforeEntries, beforeErr := sentinelEntries(beforeValue)
	sentEntries, sentErr := sentinelEntries(sentValue)
	currentEntries, currentErr := sentinelEntries(currentValue)
	if beforeErr != nil || sentErr != nil || currentErr != nil {
		// Not a map of records on one side or another, so the value is the
		// write's as a whole.
		return sentHas == currentHas && (!sentHas || jsonValuesEqual(sentValue, currentValue))
	}
	records := make(map[string]bool, len(beforeEntries)+len(sentEntries))
	for record := range beforeEntries {
		records[record] = true
	}
	for record := range sentEntries {
		records[record] = true
	}
	for record := range records {
		beforeRecord, beforeHas := beforeEntries[record]
		sentRecord, sentRecordHas := sentEntries[record]
		if beforeHas == sentRecordHas && (!sentRecordHas || jsonValuesEqual(beforeRecord, sentRecord)) {
			continue
		}
		currentRecord, currentRecordHas := currentEntries[record]
		if currentRecordHas != sentRecordHas || (sentRecordHas && !jsonValuesEqual(sentRecord, currentRecord)) {
			return false
		}
	}
	return true
}

// sentinelEntries decodes a sentinel value as a map of records. An absent
// value is an empty map, and a value that is not a JSON object is an error.
func sentinelEntries(value json.RawMessage) (map[string]json.RawMessage, error) {
	entries := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(value)) == 0 {
		return entries, nil
	}
	if err := json.Unmarshal(value, &entries); err != nil {
		return nil, err
	}
	if entries == nil {
		// JSON null decodes without an error into a nil map.
		return nil, errSentinelValueNotObject
	}
	return entries, nil
}

// errSentinelValueNotObject reports a sentinel value that decodes as JSON null
// rather than as a map of records.
var errSentinelValueNotObject = errors.New("sentinel value is not a JSON object")

// jsonValuesEqual reports whether a and b decode to the same JSON value, so
// key order and whitespace don't count. Two values that won't decode are
// equal only when their bytes are.
func jsonValuesEqual(a, b json.RawMessage) bool {
	var left, right any
	leftErr := json.Unmarshal(a, &left)
	rightErr := json.Unmarshal(b, &right)
	if leftErr != nil || rightErr != nil {
		return leftErr != nil && rightErr != nil && bytes.Equal(a, b)
	}
	return reflect.DeepEqual(left, right)
}
