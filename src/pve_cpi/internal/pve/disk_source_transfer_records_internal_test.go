package pve

import (
	"context"
	"errors"
	"strings"
	"testing"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// sourceRecordsDescription is a parker description holding the given records,
// each written as one entry of bosh_parked_disks.
func sourceRecordsDescription(entries ...string) string {
	return `<!--BOSH:{"bosh_parked_disks":{` + strings.Join(entries, ",") + `}}-->`
}

// sourceRecordEntry is one parker record for the given stable ID, source VM,
// and volid. An empty volid leaves the field out, the way a finished or
// legacy record does.
func sourceRecordEntry(stableID, source, volid string) string {
	volidField := ""
	if volid != "" {
		volidField = `,"volid":"` + volid + `","slot":"scsi4"`
	}
	return `"` + stableID + `":{"disk_cid":"pvd-` + stableID + `","source_vm_cid":"` + source +
		`","parked_at":"2026-10-02T00:00:00Z","node":"pve1"` + volidField + `}`
}

func TestFindSourceTransferRecords(t *testing.T) {
	t.Parallel()
	cfg := ParkerConfig{VMIDRangeStart: 90000, VMIDRangeEnd: 90999, FallbackNode: "pve1", ParkedEnabled: true}

	t.Run("collects matching records across parkers in parker then stable ID order", func(t *testing.T) {
		t.Parallel()
		// The listing is deliberately out of order, so a sorted answer can't
		// come from the order the parkers were read in.
		c := &scanFakeClient{
			configs: map[int]map[string]any{
				90002: {
					cfgKeyTags: "bosh-cpi;bosh-parker",
					"description": sourceRecordsDescription(
						sourceRecordEntry("bpd-0000000000000009", "700", "data:vm-700-disk-9"),
						// A record for another source VM is left out.
						sourceRecordEntry("bpd-0000000000000008", "701", "data:vm-701-disk-1"),
						// A record with no volid is left out.
						sourceRecordEntry("bpd-0000000000000007", "700", ""),
					),
				},
				90000: {
					cfgKeyTags: "bosh-cpi;bosh-parker",
					"description": sourceRecordsDescription(
						sourceRecordEntry("bpd-00000000000000bb", "700", "data:vm-700-disk-2"),
						sourceRecordEntry("bpd-00000000000000aa", "700", "data:vm-700-disk-1"),
					),
				},
				90001: {
					cfgKeyTags:    "bosh-cpi;bosh-parker",
					"description": sourceRecordsDescription(sourceRecordEntry("bpd-0000000000000005", "700", "data:vm-700-disk-5")),
				},
				// A guest that isn't a parker holds no records, even when its
				// description carries one.
				90003: {
					cfgKeyTags:    "bosh-cpi",
					"description": sourceRecordsDescription(sourceRecordEntry("bpd-0000000000000003", "700", "data:vm-700-disk-3")),
				},
			},
			rows: []map[string]any{
				clusterRow(90002, ""), clusterRow(90000, ""), clusterRow(90003, ""), clusterRow(90001, ""),
			},
		}
		got, err := FindSourceTransferRecords(context.Background(), c, "700", cfg)
		if err != nil {
			t.Fatalf("FindSourceTransferRecords: %v", err)
		}
		type key struct {
			parker int
			id     string
			volid  string
		}
		gotKeys := make([]key, 0, len(got))
		for _, r := range got {
			gotKeys = append(gotKeys, key{r.Intent.ParkerVMID, r.StableID, r.Intent.Volid})
		}
		want := []key{
			{90000, "bpd-00000000000000aa", "data:vm-700-disk-1"},
			{90000, "bpd-00000000000000bb", "data:vm-700-disk-2"},
			{90001, "bpd-0000000000000005", "data:vm-700-disk-5"},
			{90002, "bpd-0000000000000009", "data:vm-700-disk-9"},
		}
		if len(gotKeys) != len(want) {
			t.Fatalf("records = %v, want %v", gotKeys, want)
		}
		for i := range want {
			if gotKeys[i] != want[i] {
				t.Fatalf("records = %v, want %v", gotKeys, want)
			}
		}
		if first := got[0]; first.DiskCID != "pvd-bpd-00000000000000aa" || first.Intent.SourceVMCID != "700" || first.Intent.ParkerNode != "pve1" || first.Intent.Slot != "scsi4" {
			t.Fatalf("first record = %+v, want the parker's own fields carried through", first)
		}
	})

	t.Run("a source with no records gets an empty answer", func(t *testing.T) {
		t.Parallel()
		c := &scanFakeClient{
			configs: map[int]map[string]any{
				90000: {
					cfgKeyTags:    "bosh-cpi;bosh-parker",
					"description": sourceRecordsDescription(sourceRecordEntry("bpd-00000000000000aa", "701", "data:vm-701-disk-1")),
				},
			},
			rows: []map[string]any{clusterRow(90000, "")},
		}
		got, err := FindSourceTransferRecords(context.Background(), c, "700", cfg)
		if err != nil || len(got) != 0 {
			t.Fatalf("records = %v, err = %v, want none", got, err)
		}
	})

	t.Run("a parker that is gone is skipped and the others still answer", func(t *testing.T) {
		t.Parallel()
		c := &scanFakeClient{
			configs: map[int]map[string]any{
				90001: {
					cfgKeyTags:    "bosh-cpi;bosh-parker",
					"description": sourceRecordsDescription(sourceRecordEntry("bpd-0000000000000005", "700", "data:vm-700-disk-5")),
				},
			},
			rows:      []map[string]any{clusterRow(90000, ""), clusterRow(90001, "")},
			configErr: map[int]error{90000: sdkerrors.ParseAPIError(404, []byte(`{"message":"no such vm"}`))},
		}
		got, err := FindSourceTransferRecords(context.Background(), c, "700", cfg)
		if err != nil {
			t.Fatalf("FindSourceTransferRecords: %v", err)
		}
		if len(got) != 1 || got[0].Intent.ParkerVMID != 90001 {
			t.Fatalf("records = %+v, want the one on parker 90001", got)
		}
	})

	t.Run("a parker read that fails for another reason fails the scan", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("injected parker config failure")
		c := &scanFakeClient{
			configs: map[int]map[string]any{
				90000: {cfgKeyTags: "bosh-cpi;bosh-parker"},
				90001: {
					cfgKeyTags:    "bosh-cpi;bosh-parker",
					"description": sourceRecordsDescription(sourceRecordEntry("bpd-0000000000000005", "700", "data:vm-700-disk-5")),
				},
			},
			rows:      []map[string]any{clusterRow(90001, ""), clusterRow(90000, "")},
			configErr: map[int]error{90000: boom},
		}
		got, err := FindSourceTransferRecords(context.Background(), c, "700", cfg)
		if err == nil || !errors.Is(err, boom) {
			t.Fatalf("records = %v, err = %v, want the read failure", got, err)
		}
		if got != nil {
			t.Fatalf("a failed scan returned records %v", got)
		}
	})

	t.Run("an unusable band finds nothing and an empty source is refused", func(t *testing.T) {
		t.Parallel()
		got, err := FindSourceTransferRecords(context.Background(), &scanFakeClient{}, "700", ParkerConfig{})
		if err != nil || len(got) != 0 {
			t.Fatalf("records = %v, err = %v, want none without a band", got, err)
		}
		if _, err := FindSourceTransferRecords(context.Background(), &scanFakeClient{}, "", cfg); err == nil {
			t.Fatal("an empty source VM CID was accepted")
		}
	})
}
