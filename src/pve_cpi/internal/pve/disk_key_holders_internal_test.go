package pve

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"testing"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

const (
	holderVolume = "a:777/vm-777-disk-1.raw"
	holderSerial = "bpd-0011223344556677"
)

// holderWorld is a one-node cluster whose guests answer the pending endpoint
// with the rows a test gives them, so a guest can hold a key only in its
// pending view, a pending delete, or a tag in one view. errs fails a guest's
// pending read.
type holderWorld struct {
	guests map[int][]map[string]any
	errs   map[int]error
}

func (w *holderWorld) client() Client {
	return &findVMTestClient{backendTestClient: backendTestClient{
		clusterSvc: &fakeCluster{configNodesFn: findVMConfigNodes("n1")},
		nodesSvc: &fakeNodesService{
			listQemuFn: func(context.Context, string, *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
				rows := sdknodes.ListQemuResponse{}
				for vmid := range w.guests {
					raw, err := json.Marshal(map[string]any{"vmid": vmid})
					if err != nil {
						return nil, err
					}
					rows = append(rows, raw)
				}
				return &rows, nil
			},
			listQemuPendingFn: func(_ context.Context, _, vmidText string) (*sdknodes.ListQemuPendingResponse, error) {
				vmid, _ := strconv.Atoi(vmidText)
				if err := w.errs[vmid]; err != nil {
					return nil, err
				}
				resp := sdknodes.ListQemuPendingResponse{}
				for _, row := range w.guests[vmid] {
					raw, err := json.Marshal(row)
					if err != nil {
						return nil, err
					}
					resp = append(resp, raw)
				}
				return &resp, nil
			},
		},
	}}
}

// current, pending, and deleting are pending-endpoint rows: a key with only
// a current value, a key whose value is only pending, and a key whose delete
// is pending.
func current(key, value string) map[string]any { return map[string]any{"key": key, "value": value} }
func pending(key, value string) map[string]any { return map[string]any{"key": key, "pending": value} }
func deleting(key, value string) map[string]any {
	return map[string]any{"key": key, "value": value, "delete": 1}
}

// errConfigMissing is PVE's 500 for a guest whose config file is gone from
// the node the listing named, which is how a guest mid-migration answers.
var errConfigMissing = &sdkerrors.APIError{HTTPCode: 500, Message: "Configuration file 'nodes/n1/qemu-server/888.conf' does not exist"}

// TestFindDiskKeyHoldersReadsBothViews checks that a key counts as a holder
// in either view. A key that exists only as a pending value holds the disk,
// a key whose delete is pending still holds it and no longer names it the
// way a move source does, and a parker tag in either view marks the guest.
func TestFindDiskKeyHoldersReadsBothViews(t *testing.T) {
	for _, row := range []struct {
		name  string
		rows  []map[string]any
		want  []DiskKeyHolder
		vmid  int
		wantE bool
	}{
		{name: "pending-only-volume", vmid: 888, rows: []map[string]any{pending("scsi3", holderVolume)},
			want: []DiskKeyHolder{{VMID: 888, Node: "n1", Slot: "scsi3", NamesVolume: true, StillNames: true}}},
		{name: "pending-only-serial", vmid: 888, rows: []map[string]any{pending("scsi3", "a:888/vm-888-disk-4.raw,serial="+holderSerial)},
			want: []DiskKeyHolder{{VMID: 888, Node: "n1", Slot: "scsi3", CarriesSerial: true}}},
		{name: "pending-delete", vmid: 888, rows: []map[string]any{deleting("scsi2", holderVolume+",serial="+holderSerial)},
			want: []DiskKeyHolder{{VMID: 888, Node: "n1", Slot: "scsi2", NamesVolume: true, CarriesSerial: true}}},
		{name: "parker-tag-current-only", vmid: 90001, rows: []map[string]any{{"key": "tags", "value": ParkerTag, "delete": 1}, current("scsi4", holderVolume)},
			want: []DiskKeyHolder{{VMID: 90001, Node: "n1", Slot: "scsi4", Parker: true, NamesVolume: true, StillNames: true}}},
		{name: "parker-tag-pending-only", vmid: 90001, rows: []map[string]any{pending("tags", ParkerTag), current("scsi4", holderVolume)},
			want: []DiskKeyHolder{{VMID: 90001, Node: "n1", Slot: "scsi4", Parker: true, NamesVolume: true, StillNames: true}}},
		{name: "unrelated-key", vmid: 888, rows: []map[string]any{current("scsi0", "a:888/vm-888-disk-0.raw,serial=bpd-ffeeddccbbaa9988")}},
	} {
		t.Run(row.name, func(t *testing.T) {
			w := &holderWorld{guests: map[int][]map[string]any{row.vmid: row.rows}}
			got, err := FindDiskKeyHolders(context.Background(), w.client(), holderVolume, holderSerial)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, row.want) {
				t.Fatalf("holders:\n got %+v\nwant %+v", got, row.want)
			}
		})
	}
}

// TestFindDiskKeyHoldersFailsOnConfigMissing has one guest answer PVE's 500
// for a config file that is gone from its node. That guest may be moving and
// may hold the disk, so the scan fails instead of skipping it. A 404 means
// the guest was deleted after the listing, which holds nothing, so the scan
// skips it.
func TestFindDiskKeyHoldersFailsOnConfigMissing(t *testing.T) {
	guests := map[int][]map[string]any{777: {current("unused0", holderVolume)}, 888: nil}
	w := &holderWorld{guests: guests, errs: map[int]error{888: errConfigMissing}}
	if got, err := FindDiskKeyHolders(context.Background(), w.client(), holderVolume, holderSerial); err == nil {
		t.Fatalf("the scan skipped a guest whose config is missing and answered %+v", got)
	}
	w.errs[888] = &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
	got, err := FindDiskKeyHolders(context.Background(), w.client(), holderVolume, holderSerial)
	if err != nil {
		t.Fatalf("the scan failed on a deleted guest: %v", err)
	}
	if len(got) != 1 || got[0].VMID != 777 || got[0].Slot != "unused0" {
		t.Fatalf("want only 777's unused0, got %+v", got)
	}
}

// transferParker is parker 90001 holding the transfer record of the disk
// keyed holderSerial, from VM 777 under holderVolume to slot scsi4, plus the
// extra rows a test gives it.
func transferParker(t *testing.T, extra ...map[string]any) []map[string]any {
	t.Helper()
	description := provSentinel(t, map[string]parkerProvEntry{holderSerial: {DiskCID: "pvd-a", SourceVMCID: "777", Node: "n1", Volid: holderVolume, Slot: "scsi4"}})
	return append([]map[string]any{current("tags", ParkerTag), current("description", description)}, extra...)
}

// TestFindDiskTransferRecordsReadsEveryParkerKey checks the landing a parker's
// transfer record reports. A disk key of the parker, the recorded slot or any
// other bus slot or unused entry, that holds in either view a volume named for
// the parker with no serial is an unclaimed landing, and the first such key
// is named. An empty parker, a volume that carries a serial, and a volume
// named for another VM are not.
func TestFindDiskTransferRecordsReadsEveryParkerKey(t *testing.T) {
	for _, row := range []struct {
		name    string
		keys    []map[string]any
		landing string
	}{
		{name: "empty-parker"},
		{name: "landed", keys: []map[string]any{current("scsi4", "a:90001/vm-90001-disk-2.raw")}, landing: "scsi4"},
		{name: "landed-pending", keys: []map[string]any{pending("scsi4", "a:90001/vm-90001-disk-2.raw")}, landing: "scsi4"},
		{name: "landed-pending-delete", keys: []map[string]any{deleting("scsi4", "a:90001/vm-90001-disk-2.raw")}, landing: "scsi4"},
		{name: "fallback-slot", keys: []map[string]any{current("scsi7", "a:90001/vm-90001-disk-2.raw")}, landing: "scsi7"},
		{name: "unused-entry", keys: []map[string]any{current("unused3", "a:90001/vm-90001-disk-2.raw")}, landing: "unused3"},
		{name: "first-key-named", keys: []map[string]any{current("unused3", "a:90001/vm-90001-disk-5.raw"), pending("scsi7", "a:90001/vm-90001-disk-2.raw")}, landing: "scsi7"},
		{name: "claimed", keys: []map[string]any{current("scsi4", "a:90001/vm-90001-disk-2.raw,serial="+holderSerial)}},
		{name: "parked-disk", keys: []map[string]any{current("scsi5", "a:90001/vm-90001-disk-3.raw,serial=bpd-ffeeddccbbaa9988")}},
		{name: "other-vm-volume", keys: []map[string]any{current("scsi4", "a:888/vm-888-disk-2.raw")}},
	} {
		t.Run(row.name, func(t *testing.T) {
			w := &holderWorld{guests: map[int][]map[string]any{
				777:   {current("unused0", holderVolume)},
				90001: transferParker(t, row.keys...),
			}}
			got, err := FindDiskTransferRecords(context.Background(), w.client(), holderSerial)
			if err != nil {
				t.Fatal(err)
			}
			want := []DiskTransferRecord{{ParkerVMID: 90001, ParkerNode: "n1", Slot: "scsi4", Volid: holderVolume, SourceVMCID: "777", UnclaimedLanding: row.landing}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("records:\n got %+v\nwant %+v", got, want)
			}
		})
	}
}

// TestFindDiskTransferRecordsReadsOnlyParkers checks that a record counts only
// on a guest a parker tag marks, in either view, and that a record of another
// disk is never returned.
func TestFindDiskTransferRecordsReadsOnlyParkers(t *testing.T) {
	description := provSentinel(t, map[string]parkerProvEntry{holderSerial: {DiskCID: "pvd-a", SourceVMCID: "777", Node: "n1", Volid: holderVolume, Slot: "scsi4"}})
	other := provSentinel(t, map[string]parkerProvEntry{"bpd-ffeeddccbbaa9988": {DiskCID: "pvd-b", SourceVMCID: "777", Node: "n1", Volid: holderVolume, Slot: "scsi4"}})
	w := &holderWorld{guests: map[int][]map[string]any{
		888:   {current("description", description)},
		90001: {pending("tags", ParkerTag), current("description", description)},
		90002: {current("tags", ParkerTag), current("description", other)},
	}}
	got, err := FindDiskTransferRecords(context.Background(), w.client(), holderSerial)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ParkerVMID != 90001 {
		t.Fatalf("want only parker 90001's record, got %+v", got)
	}
}

// TestFindDiskTransferRecordsFailsOnUnreadGuest has one guest's read fail.
// A 500 fails the scan, because that guest may be a parker holding the
// record, and a 404 skips the guest.
func TestFindDiskTransferRecordsFailsOnUnreadGuest(t *testing.T) {
	w := &holderWorld{guests: map[int][]map[string]any{888: nil, 90001: transferParker(t)}, errs: map[int]error{888: errConfigMissing}}
	if got, err := FindDiskTransferRecords(context.Background(), w.client(), holderSerial); err == nil {
		t.Fatalf("the scan skipped a guest whose config is missing and answered %+v", got)
	}
	w.errs[888] = errors.New("connection reset by peer")
	if _, err := FindDiskTransferRecords(context.Background(), w.client(), holderSerial); err == nil {
		t.Fatal("the scan skipped a guest it could not read")
	}
	w.errs[888] = &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
	got, err := FindDiskTransferRecords(context.Background(), w.client(), holderSerial)
	if err != nil || len(got) != 1 {
		t.Fatalf("want the parker's record with the deleted guest skipped, got %+v, %v", got, err)
	}
}
