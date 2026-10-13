package pve

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
)

const sightingDirector = "11111111-2222-3333-4444-555555555555"

func sightingCfg() ParkerConfig {
	return ParkerConfig{VMIDRangeStart: 90000, VMIDRangeEnd: 90999, DirectorID: sightingDirector, Prefix: "az1", FallbackNode: "pve1"}
}

func sightingTags(prefix string, director string, extra ...string) string {
	tags := []string{"bosh-cpi", ParkerTag}
	if prefix != "" {
		tags = append(tags, parkerPrefixTag(prefix))
	}
	if director != "" {
		tags = append(tags, parkerDirectorTag(director))
	}
	return strings.Join(append(tags, extra...), ";")
}

func TestParkerBandSightingsClassifiesRows(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		vmid          int
		tags, guest   string
		outside       bool
		otherPrefix   bool
		otherDirector bool
	}{
		{name: "our parker in band", vmid: 90001, tags: sightingTags("az1", sightingDirector)},
		{name: "other prefix in band", vmid: 90002, tags: sightingTags("az2", sightingDirector), otherPrefix: true},
		{name: "other prefix in band by name only", vmid: 90003, tags: sightingTags("", ""), guest: "az2-parker-90003", otherPrefix: true},
		{name: "other director other prefix in band", vmid: 90004, tags: sightingTags("az2", "99999999-0000-0000-0000-000000000000"), otherDirector: true},
		{name: "other prefix in band tagged by us and another director", vmid: 90009, tags: sightingTags("az2", sightingDirector, parkerDirectorTag("99999999-0000-0000-0000-000000000000")), otherPrefix: true},
		{name: "other prefix in band with our tag in another case", vmid: 90010, tags: sightingTags("az2", "", strings.ToUpper(parkerDirectorTag(sightingDirector))), otherPrefix: true},
		{name: "other director same prefix in band", vmid: 90011, tags: sightingTags("az1", "99999999-0000-0000-0000-000000000000")},
		{name: "mover of another prefix in band", vmid: 90005, tags: sightingTags("az2", sightingDirector, DiskMoverTag), otherPrefix: true},
		{name: "unevidenced prefix in band", vmid: 90006, tags: sightingTags("", "")},
		{name: "our parker outside band", vmid: 91001, tags: sightingTags("az1", sightingDirector), outside: true},
		{name: "our prefix by name outside band", vmid: 91002, tags: sightingTags("", sightingDirector), guest: "az1-parker-91002", outside: true},
		{name: "unattributed parker outside band", vmid: 91003, tags: sightingTags("az1", "")},
		{name: "other director outside band", vmid: 91004, tags: sightingTags("az1", "99999999-0000-0000-0000-000000000000")},
		{name: "other prefix outside band", vmid: 91005, tags: sightingTags("az2", sightingDirector)},
		{name: "not a parker", vmid: 90007, tags: "bosh-cpi;" + parkerPrefixTag("az2")},
		{name: "empty tags", vmid: 90008, guest: "az2-parker-90008"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newParkerBandSightings(sightingCfg())
			s.observe("pve1", tc.vmid, tc.tags, tc.guest)
			if got := len(s.outside) == 1; got != tc.outside {
				t.Errorf("outside = %v, want %v", s.outside, tc.outside)
			}
			if got := len(s.otherPrefix) == 1; got != tc.otherPrefix {
				t.Errorf("otherPrefix = %v, want %v", s.otherPrefix, tc.otherPrefix)
			}
			if got := len(s.otherDirector) == 1; got != tc.otherDirector {
				t.Errorf("otherDirector = %v, want %v", s.otherDirector, tc.otherDirector)
			}
		})
	}
}

func TestParkerBandSightingsNeedsADirectorForOutsideBand(t *testing.T) {
	t.Parallel()
	cfg := sightingCfg()
	cfg.DirectorID = ""
	s := newParkerBandSightings(cfg)
	s.observe("pve1", 91001, sightingTags("az1", sightingDirector), "")
	if len(s.outside) != 0 {
		t.Fatalf("outside-band sighting without a director ID: %v", s.outside)
	}
}

// TestParkerBandSightingsWithoutADirectorTreatsEveryTagAsForeign covers a
// call that has no director ID. It can't prove a director-tagged parker of
// another prefix is ours, so that parker gets the warning that never advises
// removal, while an untagged one keeps the ordinary other-prefix warning.
func TestParkerBandSightingsWithoutADirectorTreatsEveryTagAsForeign(t *testing.T) {
	t.Parallel()
	cfg := sightingCfg()
	cfg.DirectorID = ""
	s := newParkerBandSightings(cfg)
	s.observe("pve1", 90002, sightingTags("az2", sightingDirector), "")
	s.observe("pve1", 90003, sightingTags("az2", ""), "")
	if fmt.Sprint(s.otherDirector) != "[pve1/90002(az2)]" || fmt.Sprint(s.otherPrefix) != "[pve1/90003(az2)]" {
		t.Fatalf("otherDirector = %v, otherPrefix = %v", s.otherDirector, s.otherPrefix)
	}
}

// TestParkerBandSightingsAnotherDirectorWarningNeverAdvisesRemoval checks the
// third warning. A parker another Director may own must never be presented
// as a leftover we can remove, because that Director can park a disk on it
// between our audit and our destroy.
func TestParkerBandSightingsAnotherDirectorWarningNeverAdvisesRemoval(t *testing.T) {
	t.Parallel()
	for _, directorID := range []string{sightingDirector, ""} {
		logger, obs := log.NewObservedLogger(log.LevelWarn)
		ctx := log.IntoContext(context.Background(), logger)
		cfg := sightingCfg()
		cfg.DirectorID = directorID
		s := newParkerBandSightings(cfg)
		s.observe("pve2", 90004, sightingTags("bosh", "99999999-0000-0000-0000-000000000000"), "")
		s.report(ctx, "test_listing")
		entries := obs.All()
		if len(entries) != 1 {
			t.Fatalf("director %q: want one warning, got %+v", directorID, entries)
		}
		e := entries[0]
		for _, want := range []string{"another BOSH Director on this cluster may own them", "never remove them", "its own parked_disk_vmid_range"} {
			if !strings.Contains(e.Message, want) {
				t.Fatalf("director %q: warning %q lacks %q", directorID, e.Message, want)
			}
		}
		for _, banned := range []string{"leftovers", "Remove a parker", "drain"} {
			if strings.Contains(e.Message, banned) {
				t.Fatalf("director %q: warning %q suggests %q", directorID, e.Message, banned)
			}
		}
		if e.Attrs["parkers"] != "pve2/90004(bosh)" || e.Attrs["count"] != int64(1) || e.Attrs["director_known"] != (directorID != "") {
			t.Fatalf("director %q: attrs = %+v", directorID, e.Attrs)
		}
	}
}

// TestParkerBandSightingsReportOneWarningPerShape checks the aggregation: one
// line per shape however many guests matched, with the full count and a
// capped sample.
func TestParkerBandSightingsReportOneWarningPerShape(t *testing.T) {
	t.Parallel()
	logger, obs := log.NewObservedLogger(log.LevelWarn)
	ctx := log.IntoContext(context.Background(), logger)
	s := newParkerBandSightings(sightingCfg())
	for i := range parkerSightingSampleLimit + 5 {
		s.observe("pve1", 90100+i, sightingTags("az2", ""), "")
	}
	s.observe("pve2", 95000, sightingTags("az1", sightingDirector), "")
	s.report(ctx, "test_listing")

	entries := obs.All()
	if len(entries) != 2 {
		t.Fatalf("want two warnings, got %+v", entries)
	}
	byMessage := map[string]log.Entry{}
	for _, e := range entries {
		if e.Level != log.LevelWarn {
			t.Fatalf("level = %v", e.Level)
		}
		byMessage[e.Message] = e
	}
	var outside, other log.Entry
	for message, e := range byMessage {
		switch {
		case strings.Contains(message, "outside the configured parker band"):
			outside = e
		case strings.Contains(message, "inside this entry's parker band"):
			other = e
		}
	}
	if outside.Attrs["parkers"] != "pve2/95000" || outside.Attrs["count"] != int64(1) || outside.Attrs["listing"] != "test_listing" {
		t.Fatalf("outside warning = %+v", outside.Attrs)
	}
	// The out-of-band warning can't see whether a parker holds disks, so its
	// advice must put covering the parkers first and condition removal on
	// proof that the parker is empty.
	for _, want := range []string{"refuses to attach, update, or delete", "cover them again instead of removing them", "only after confirming it holds no disks", "Recovering empty parker VMs"} {
		if !strings.Contains(outside.Message, want) {
			t.Fatalf("outside warning %q lacks %q", outside.Message, want)
		}
	}
	for _, want := range []string{"leftovers from a parker prefix change", "Never move a band away from parkers that still hold disks"} {
		if !strings.Contains(other.Message, want) {
			t.Fatalf("other-prefix warning %q lacks %q", other.Message, want)
		}
	}
	if other.Attrs["count"] != int64(parkerSightingSampleLimit+5) || other.Attrs["parker_band"] != "90000-90999" || other.Attrs["parker_prefix"] != "az1" {
		t.Fatalf("other-prefix warning = %+v", other.Attrs)
	}
	sample, _ := other.Attrs["parkers"].(string)
	if !strings.HasPrefix(sample, "pve1/90100(az2),") || !strings.HasSuffix(sample, ",... (5 more)") || strings.Count(sample, "(az2)") != parkerSightingSampleLimit {
		t.Fatalf("sample = %q", sample)
	}
}

func TestParkerBandSightingsReportIsSilentWhenClean(t *testing.T) {
	t.Parallel()
	logger, obs := log.NewObservedLogger(log.LevelDebug)
	ctx := log.IntoContext(context.Background(), logger)
	s := newParkerBandSightings(sightingCfg())
	s.observe("pve1", 90001, sightingTags("az1", sightingDirector), "")
	s.report(ctx, "test_listing")
	var nilSightings *parkerBandSightings
	nilSightings.observe("pve1", 90002, sightingTags("az2", ""), "")
	nilSightings.report(ctx, "test_listing")
	if obs.Len() != 0 {
		t.Fatalf("clean listing logged %+v", obs.All())
	}
}

// TestListParkersClusterWarnsFromRowsItAlreadyHas drives the provenance
// scan's cluster listing. The candidates it returns are unchanged by the
// sightings, both warnings come from the one listing, and nothing is written.
func TestListParkersClusterWarnsFromRowsItAlreadyHas(t *testing.T) {
	t.Parallel()
	logger, obs := log.NewObservedLogger(log.LevelWarn)
	ctx := log.IntoContext(context.Background(), logger)
	row := func(vmid int, tags, name string) map[string]any {
		r := clusterRow(vmid, tags)
		if name != "" {
			r["name"] = name
		}
		return r
	}
	c := &scanFakeClient{rows: []map[string]any{
		row(90001, sightingTags("az1", sightingDirector), "az1-parker-90001"),
		row(90002, sightingTags("az2", ""), "az2-parker-90002"),
		row(91001, sightingTags("az1", sightingDirector), "az1-parker-91001"),
		row(100, "bosh-cpi", "vm-1"),
		// A tags field of the wrong type must not drop the in-band row.
		{"vmid": 90003, "node": "pve1", "type": clusterResourceTypeQemu, "tags": 7},
	}}
	got, err := listParkersCluster(ctx, c, sightingCfg())
	if err != nil {
		t.Fatalf("listParkersCluster: %v", err)
	}
	if fmt.Sprint(got) != fmt.Sprint([]parkerCandidate{{90001, "pve1"}, {90002, "pve1"}, {90003, "pve1"}}) {
		t.Fatalf("candidates = %+v", got)
	}
	if len(c.events) != 0 {
		t.Fatalf("listing made state-changing calls: %v", c.events)
	}
	entries := obs.All()
	if len(entries) != 2 {
		t.Fatalf("want both warnings, got %+v", entries)
	}
	for _, e := range entries {
		if e.Attrs["listing"] != "provenance_scan" {
			t.Fatalf("listing = %v", e.Attrs["listing"])
		}
	}
}
