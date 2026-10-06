package pve

import (
	"encoding/json"
	"testing"
)

// landedTestDescription renders free text and a sentinel whose parked-disk
// map holds records, each given as raw JSON.
func landedTestDescription(t *testing.T, text string, records map[string]string, other map[string]string) string {
	t.Helper()
	raw := map[string]json.RawMessage{}
	disks := map[string]json.RawMessage{}
	for key, record := range records {
		disks[key] = json.RawMessage(record)
	}
	if records != nil {
		b, err := json.Marshal(disks)
		if err != nil {
			t.Fatalf("marshal records: %v", err)
		}
		raw["bosh_parked_disks"] = b
	}
	for key, value := range other {
		raw[key] = json.RawMessage(value)
	}
	desc, err := RenderSentinel(text, raw)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return desc
}

func TestParkerRecordLanded(t *testing.T) {
	t.Parallel()

	ours := `{"volid":"local-lvm:vm-90000-disk-5","slot":"scsi5"}`
	sent := landedTestDescription(t, "note\n", map[string]string{"bpd-ours": ours}, nil)

	cases := []struct {
		name    string
		current string
		want    bool
	}{
		{"exactly as sent", sent, true},
		{"another holder added its record", landedTestDescription(t, "note\n",
			map[string]string{"bpd-ours": ours, "bpd-theirs": `{"volid":"x"}`}, nil), true},
		{"the operator's text changed", landedTestDescription(t, "edited\n",
			map[string]string{"bpd-ours": ours}, nil), true},
		{"same record with keys reordered", landedTestDescription(t, "note\n",
			map[string]string{"bpd-ours": `{"slot":"scsi5","volid":"local-lvm:vm-90000-disk-5"}`}, nil), true},
		{"our record changed", landedTestDescription(t, "note\n",
			map[string]string{"bpd-ours": `{"volid":"local-lvm:vm-90000-disk-6","slot":"scsi5"}`}, nil), false},
		{"our record is gone", landedTestDescription(t, "note\n",
			map[string]string{"bpd-theirs": `{"volid":"x"}`}, nil), false},
		{"no sentinel at all", "note\n", false},
	}
	for _, tc := range cases {
		if got := ParkerRecordLanded(sent, tc.current, "bpd-ours"); got != tc.want {
			t.Errorf("%s: ParkerRecordLanded = %v, want %v", tc.name, got, tc.want)
		}
	}

	removed := landedTestDescription(t, "note\n", map[string]string{"bpd-theirs": `{"volid":"x"}`}, nil)
	if !ParkerRecordLanded(removed, "note\n", "bpd-ours") {
		t.Errorf("a removal of our record should count as landed when the record is absent")
	}
	if ParkerRecordLanded(removed, sent, "bpd-ours") {
		t.Errorf("a removal of our record should not count as landed while the record is back")
	}
}

func TestDescriptionWriteLanded(t *testing.T) {
	t.Parallel()

	ours := `{"volid":"local-lvm:vm-90000-disk-5"}`
	stale := `{"volid":"local-lvm:vm-90000-disk-3"}`
	theirs := `{"volid":"local-lvm:vm-90000-disk-9"}`
	allocs := map[string]string{"bosh_disk_allocations": `{"a1":{"state":"ready"}}`}
	before := landedTestDescription(t, "note\n", map[string]string{"bpd-stale": stale}, allocs)
	// Our write adds our record and collects the stale one.
	sent := landedTestDescription(t, "note\n", map[string]string{"bpd-ours": ours}, allocs)

	cases := []struct {
		name    string
		current string
		want    bool
	}{
		{"exactly as sent", sent, true},
		{"another holder added its record after ours", landedTestDescription(t, "note\n",
			map[string]string{"bpd-ours": ours, "bpd-theirs": theirs}, allocs), true},
		{"the operator edited text our write left alone", landedTestDescription(t, "edited\n",
			map[string]string{"bpd-ours": ours}, allocs), true},
		{"another key our write left alone changed", landedTestDescription(t, "note\n",
			map[string]string{"bpd-ours": ours}, map[string]string{"bosh_disk_allocations": `{}`}), true},
		{"our record is missing", landedTestDescription(t, "note\n", map[string]string{}, allocs), false},
		{"the record we collected is back", landedTestDescription(t, "note\n",
			map[string]string{"bpd-ours": ours, "bpd-stale": stale}, allocs), false},
		{"our record reads differently", landedTestDescription(t, "note\n",
			map[string]string{"bpd-ours": theirs}, allocs), false},
	}
	for _, tc := range cases {
		if got := DescriptionWriteLanded(before, sent, tc.current); got != tc.want {
			t.Errorf("%s: DescriptionWriteLanded = %v, want %v", tc.name, got, tc.want)
		}
	}

	textEdit := landedTestDescription(t, "rewritten\n", map[string]string{"bpd-stale": stale}, allocs)
	if DescriptionWriteLanded(before, textEdit, before) {
		t.Errorf("a write that changed the text should not count as landed while the old text is back")
	}
	scalarBefore := landedTestDescription(t, "", nil, map[string]string{"bosh_flag": `"a"`})
	scalarSent := landedTestDescription(t, "", nil, map[string]string{"bosh_flag": `"b"`})
	if !DescriptionWriteLanded(scalarBefore, scalarSent, scalarSent) || DescriptionWriteLanded(scalarBefore, scalarSent, scalarBefore) {
		t.Errorf("a value that is not a map of records should count as a whole")
	}
}
