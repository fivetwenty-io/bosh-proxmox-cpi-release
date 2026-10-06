package pve

// A resume that finds a landing on its parker reads the source of every other
// unfinished transfer there, and it counts one as moved only on positive
// evidence. These rows cover each answer that read can give, for another
// disk's transfer and for the resumed disk's own, and the states where only a
// serial could say which landing is whose.

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// resumeStatusThirdID is the serial of disk T, a third disk whose unfinished
// transfer shares the parker.
const resumeStatusThirdID = "bpd-e0e0e0e0e0e0e0e0"

// volumeStorage is a storage service whose point probe finds a volume while
// any guest's config, pending section, or snapshot names it, or while floating
// lists it, which is how a released volume sits with no guest naming it.
func volumeStorage(c *scanFakeClient, floating map[string]bool) *fakeStorageService {
	return &fakeStorageService{existsFn: func(_ context.Context, _, _, volume string) (bool, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if floating[volume] {
			return true, nil
		}
		var sections []map[string]any
		for _, views := range []map[int]map[string]any{c.configs, c.held, c.replaced} {
			for _, cfg := range views {
				sections = append(sections, cfg)
			}
		}
		for _, snapshots := range c.snapshots {
			for _, cfg := range snapshots {
				sections = append(sections, cfg)
			}
		}
		for _, cfg := range sections {
			for _, value := range cfg {
				if text, ok := value.(string); ok && bareDriveVolid(text) == volume {
					return true, nil
				}
			}
		}
		return false, nil
	}}
}

// storageFakeClient is a scanFakeClient with the storage service volumeStorage
// gives it.
type storageFakeClient struct {
	*scanFakeClient
	floating map[string]bool
}

func (c *storageFakeClient) Storage() storage.Service {
	return volumeStorage(c.scanFakeClient, c.floating)
}

// ClusterStorage serves one node-local entry for storage "data", so the
// absence proof can classify it. A test that needs the /storage read to fail
// wraps the client in unclassifiedStorageClient.
func (c *storageFakeClient) ClusterStorage() clusterstorage.Service {
	return &fakeClusterStorageService{listFn: func(context.Context, *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
		resp := clusterstorage.ListStorageResponse{json.RawMessage(localDataStorage)}
		return &resp, nil
	}}
}

// migratedStorageClient is a migratedSourceClient with the storage service
// volumeStorage gives it.
type migratedStorageClient struct {
	*migratedSourceClient
}

func (c *migratedStorageClient) Storage() storage.Service {
	return volumeStorage(c.scanFakeClient, nil)
}

// cidRecord is one unfinished transfer record on parker 90000 that keeps cid.
func cidRecord(cid, volid, slot, source string) parkerProvEntry {
	record := transferRecord(volid, slot, source)
	record.DiskCID = cid
	return record
}

// goneError is PVE's answer to a config read of a VM that isn't on the node.
func goneError(vmid string) error {
	return errors.New("Configuration file 'nodes/pve1/qemu-server/" + vmid + ".conf' does not exist")
}

// thirdIntent is disk T's transfer record as its own resume reads it.
func thirdIntent(slot, volid, source string) DiskTransferIntent {
	return DiskTransferIntent{ParkerVMID: 90000, ParkerNode: "pve1", Slot: slot, Volid: volid, SourceVMCID: source}
}

// TestResumeStatus_LandingOnTheSlotOfARecordWhoseMoveIsUnknown is the shape an
// older release's fallback leaves when A's volume lands on scsi5, the slot T's
// record names, and the CPI dies before A's serial write. A new disk then takes
// A's freed name on 700 and is left on 700's unused0 with no serial. T's own
// transfer never landed, and the read of T can't prove it moved. Its source may
// be gone, its record may name no source, its volume may still exist with no
// guest naming it, its volume may not be named for its source, a snapshot of
// its source may name its volume, or its source may name its volume under
// another disk's serial. Before the change, a gone source, a volume that still
// exists, a volume not named for its source, and a snapshot each counted as
// moved, so A's resume set the landing aside as T's and moved the new disk onto
// the parker with A's serial. A record with no source and a name under another
// disk's serial both refused retriably, which never clears. Now A's resume
// refuses and asks for an audit, and it moves nothing.
func TestResumeStatus_LandingOnTheSlotOfARecordWhoseMoveIsUnknown(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, volid, source string
		configs             map[int]map[string]any
		gone                bool
		floating            map[string]bool
		snapshots           map[int]map[string]map[string]any
		why                 string
	}{
		{name: "its source is gone", volid: "data:vm-702-disk-0", source: "702", gone: true,
			why: "its source vm 702 is gone from the cluster"},
		{name: "its record names no source vm", volid: "data:vm-702-disk-0", source: "",
			why: "its record names no source vm and volume"},
		{name: "its volume still exists and no guest names it", volid: "data:vm-702-disk-0", source: "702",
			configs: map[int]map[string]any{702: {}}, floating: map[string]bool{"data:vm-702-disk-0": true},
			why: "its recorded volume data:vm-702-disk-0 still exists on storage data and no guest names it"},
		{name: "its volume isn't named for its source", volid: "data:vm-9001-disk-0", source: "702",
			configs: map[int]map[string]any{702: {}},
			why:     "its recorded volume data:vm-9001-disk-0 isn't named for its source vm 702, so its park attaches it under that name"},
		{name: "a snapshot of its source names its volume", volid: "data:vm-702-disk-0", source: "702",
			configs:   map[int]map[string]any{702: {}},
			snapshots: map[int]map[string]map[string]any{702: {"before-upgrade": {"scsi0": "data:vm-702-disk-0,size=10G"}}},
			why:       `snapshot "before-upgrade" of its source vm 702 names its recorded volume data:vm-702-disk-0 on scsi0`},
		{name: "its source names its volume under another disk's serial", volid: "data:vm-702-disk-0", source: "702",
			configs: map[int]map[string]any{702: {"scsi1": "data:vm-702-disk-0,serial=" + resumeProofOtherID + ",size=10G"}},
			why:     "its source vm 702 names its recorded volume data:vm-702-disk-0 on scsi1 under another disk's serial " + resumeProofOtherID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			configs := map[int]map[string]any{
				700: {"unused0": "data:vm-700-disk-1"},
				90000: {
					cfgKeyTags: "bosh-cpi;bosh-parker",
					"description": parkerRecords(t, map[string]parkerProvEntry{
						transferStableID:    cidRecord("pvd-a", "data:vm-700-disk-1", "scsi4", "700"),
						resumeStatusThirdID: cidRecord("pvd-t", tc.volid, "scsi5", tc.source),
					}),
					"scsi5": "data:vm-90000-disk-6",
				},
			}
			for vmid, cfg := range tc.configs {
				configs[vmid] = cfg
			}
			inner := newScanFakeClient(configs)
			if tc.gone {
				inner.configErr = map[int]error{702: goneError("702")}
			}
			inner.snapshots = tc.snapshots
			c := &storageFakeClient{scanFakeClient: inner, floating: tc.floating}
			before := cloneConfig(inner.configs[700])

			_, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
			requireResumeRefusal(t, err, `data:vm-90000-disk-6 on scsi5 sits on the slot disk `+resumeStatusThirdID+`'s unfinished record `+
				`names (disk cid pvd-t), but `+tc.why+`, so it can't count as that disk's, and this disk's record (disk cid pvd-a) `+
				`names slot "scsi4" (audit required)`)
			requireNothingMovedOrTagged(t, inner)
			requireSourceUnchanged(t, inner, before)
			if got := inner.configs[90000]["scsi5"]; got != "data:vm-90000-disk-6" {
				t.Errorf("parker scsi5 = %v, want A's landing left bare", got)
			}
		})
	}
}

// TestResumeStatus_LandingOnASlotNoRecordNames covers a landing on scsi9,
// which no unfinished record names, beside T's record on scsi5. When T's move
// is proved and its slot is empty, the landing may be T's, so A's resume
// refuses and asks for an audit. For every other status of T, the landing
// isn't on A's recorded slot, so A's resume refuses the claim. Before the
// change a proved move refused retriably, which never clears.
func TestResumeStatus_LandingOnASlotNoRecordNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, volid string
		configs     map[int]map[string]any
		gone        bool
		want        string
	}{
		{name: "T's move is proved", volid: "data:vm-702-disk-0", configs: map[int]map[string]any{702: {}},
			want: `disk ` + resumeStatusThirdID + `'s unfinished record (disk cid pvd-t, slot "scsi5") reads as moved, because its ` +
				`source vm 702 names its recorded volume data:vm-702-disk-0 nowhere, and storage data no longer holds it, and no ` +
				`landing on its slot accounts for it, so data:vm-90000-disk-6 on scsi9 may be that disk's and not this disk's ` +
				`(disk cid pvd-a, slot "scsi4") (audit required)`},
		{name: "T hasn't moved", volid: "data:vm-702-disk-0", configs: map[int]map[string]any{702: {"unused0": "data:vm-702-disk-0"}},
			want: `scsi9 of parker vmid 90000 holds data:vm-90000-disk-6, a volume named for the parker with no serial, while the ` +
				`record names slot "scsi4"`},
		{name: "T's source is gone", volid: "data:vm-702-disk-0", gone: true,
			want: `scsi9 of parker vmid 90000 holds data:vm-90000-disk-6, a volume named for the parker with no serial, while the ` +
				`record names slot "scsi4"`},
		{name: "T's volume isn't named for its source", volid: "data:vm-9001-disk-0", configs: map[int]map[string]any{702: {}},
			want: `scsi9 of parker vmid 90000 holds data:vm-90000-disk-6, a volume named for the parker with no serial, while the ` +
				`record names slot "scsi4"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			configs := map[int]map[string]any{
				700: {"unused0": "data:vm-700-disk-1"},
				90000: {
					cfgKeyTags: "bosh-cpi;bosh-parker",
					"description": parkerRecords(t, map[string]parkerProvEntry{
						transferStableID:    cidRecord("pvd-a", "data:vm-700-disk-1", "scsi4", "700"),
						resumeStatusThirdID: cidRecord("pvd-t", tc.volid, "scsi5", "702"),
					}),
					"scsi9": "data:vm-90000-disk-6",
				},
			}
			for vmid, cfg := range tc.configs {
				configs[vmid] = cfg
			}
			inner := newScanFakeClient(configs)
			if tc.gone {
				inner.configErr = map[int]error{702: goneError("702")}
			}
			c := &storageFakeClient{scanFakeClient: inner}
			before := cloneConfig(inner.configs[700])

			_, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
			requireResumeRefusal(t, err, tc.want)
			requireNothingMovedOrTagged(t, inner)
			requireSourceUnchanged(t, inner, before)
		})
	}
}

// TestResumeStatus_ClaimBesideARecordWhoseMoveIsUnknown covers A's claim of
// the landing on its own slot while 700 still names A's old name on unused0
// with no serial, and T's record names scsi5, which holds no landing.
// Whatever T's status, the landing may be T's unless T's park attaches its
// volume under its own name, so A's resume refuses and asks for an audit in
// every unknown status. When T's volume isn't named for its source, T's park
// would attach it under that name, so the landing can only be A's, and A's
// resume claims it. Before the change a gone source and a snapshot read as
// moved, so the claim went ahead, and a missing source refused retriably.
func TestResumeStatus_ClaimBesideARecordWhoseMoveIsUnknown(t *testing.T) {
	t.Parallel()
	claim := `data:vm-90000-disk-5 on scsi4, the slot this record names, may be this disk's (disk cid pvd-a) or disk ` +
		resumeStatusThirdID + `'s (disk cid pvd-t), because this disk's source vm 700 still names this disk's recorded volume ` +
		`data:vm-700-disk-1 on unused0, while disk ` + resumeStatusThirdID + `'s unfinished record names slot "scsi5", which ` +
		`holds no landing, and `
	for _, tc := range []struct {
		name, volid, source string
		configs             map[int]map[string]any
		gone                bool
		floating            map[string]bool
		snapshots           map[int]map[string]map[string]any
		why                 string
	}{
		{name: "T's source is gone", volid: "data:vm-702-disk-0", source: "702", gone: true,
			why: "its source vm 702 is gone from the cluster"},
		{name: "T's record names no source vm", volid: "data:vm-702-disk-0", source: "",
			why: "its record names no source vm and volume"},
		{name: "T's volume still exists and no guest names it", volid: "data:vm-702-disk-0", source: "702",
			configs: map[int]map[string]any{702: {}}, floating: map[string]bool{"data:vm-702-disk-0": true},
			why: "its recorded volume data:vm-702-disk-0 still exists on storage data and no guest names it"},
		{name: "a snapshot of T's source names its volume", volid: "data:vm-702-disk-0", source: "702",
			configs:   map[int]map[string]any{702: {}},
			snapshots: map[int]map[string]map[string]any{702: {"before-upgrade": {"scsi0": "data:vm-702-disk-0,size=10G"}}},
			why:       `snapshot "before-upgrade" of its source vm 702 names its recorded volume data:vm-702-disk-0 on scsi0`},
		{name: "T's volume isn't named for its source", volid: "data:vm-9001-disk-0", source: "702",
			configs: map[int]map[string]any{702: {}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			configs := map[int]map[string]any{
				700: {"unused0": "data:vm-700-disk-1"},
				90000: {
					cfgKeyTags: "bosh-cpi;bosh-parker",
					"description": parkerRecords(t, map[string]parkerProvEntry{
						transferStableID:    cidRecord("pvd-a", "data:vm-700-disk-1", "scsi4", "700"),
						resumeStatusThirdID: cidRecord("pvd-t", tc.volid, "scsi5", tc.source),
					}),
					"scsi4": "data:vm-90000-disk-5",
				},
			}
			for vmid, cfg := range tc.configs {
				configs[vmid] = cfg
			}
			inner := newScanFakeClient(configs)
			if tc.gone {
				inner.configErr = map[int]error{702: goneError("702")}
			}
			inner.snapshots = tc.snapshots
			c := &storageFakeClient{scanFakeClient: inner, floating: tc.floating}
			before := cloneConfig(inner.configs[700])

			_, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
			if tc.why == "" {
				if err != nil {
					t.Fatalf("A's resume: %v", err)
				}
				if got := serialSlots(inner); len(got) != 1 || got["scsi4"] != "data:vm-90000-disk-5,serial="+transferStableID {
					t.Errorf("slots carrying A's serial = %v, want only scsi4 holding data:vm-90000-disk-5", got)
				}
			} else {
				requireResumeRefusal(t, err, claim+tc.why+" (audit required)")
				requireNothingMovedOrTagged(t, inner)
			}
			requireSourceUnchanged(t, inner, before)
		})
	}
}

// TestResumeStatus_OwnReleasedVolumeNeverClaimsALanding covers a disk whose
// recorded volume isn't named for its source VM. Its park attaches that volume
// to the parker under its own name by a configuration edit, so a landing that
// PVE renamed for the parker can't be this disk's, even on its recorded slot.
// Before the change the resume wrote the disk's serial onto that landing.
func TestResumeStatus_OwnReleasedVolumeNeverClaimsALanding(t *testing.T) {
	t.Parallel()
	inner := newScanFakeClient(map[int]map[string]any{
		700: {},
		90000: {
			cfgKeyTags:    "bosh-cpi;bosh-parker",
			"description": parkerRecords(t, map[string]parkerProvEntry{transferStableID: cidRecord("pvd-a", "data:vm-9001-disk-0", "scsi4", "700")}),
			"scsi4":       "data:vm-90000-disk-5",
		},
	})
	intent := resumeProofIntent
	intent.Volid = "data:vm-9001-disk-0"

	_, err := ResumeDiskTransferToParker(context.Background(), inner, nil, intent, transferStableID, transferTestCfg, ParkContext{})
	requireResumeRefusal(t, err, `data:vm-90000-disk-5 on scsi4 is named for the parker, while this disk's recorded volume `+
		`data:vm-9001-disk-0 isn't named for its source vm 700, so this disk's park attaches that volume under its own name and `+
		`data:vm-90000-disk-5 isn't this disk's (disk cid pvd-a)`)
	requireNothingMovedOrTagged(t, inner)
	if got := inner.configs[90000]["scsi4"]; got != "data:vm-90000-disk-5" {
		t.Errorf("parker scsi4 = %v, want the landing left bare", got)
	}
}

// TestResumeStatus_AFailedReadOfAnotherSource covers a read of T's source
// that fails while a landing sits on T's slot. A read that may answer next
// time refuses retriably, and a read that already failed permanently stays
// permanent. Before the change every such failure became retriable, and a
// failed listing of the source's snapshots went unread, so T counted as moved.
func TestResumeStatus_AFailedReadOfAnotherSource(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		configErr   error
		snapshotErr error
		retriable   bool
		want        string
	}{
		{name: "the config read fails", configErr: errors.New("500 Internal Server Error"), retriable: true,
			want: "transfer resume: read source vm 702 of disk " + resumeStatusThirdID},
		{name: "the config read fails permanently", configErr: cpierrors.Cloud("ReadQemuViews: VM 702 on node pve1: malformed pending entry: x"),
			want: "malformed pending entry"},
		{name: "the snapshot listing fails", snapshotErr: errors.New("500 Internal Server Error"), retriable: true,
			want: "transfer resume: list the snapshots of source vm 702 of disk " + resumeStatusThirdID + "'s transfer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inner := newScanFakeClient(map[int]map[string]any{
				700: {"unused0": "data:vm-700-disk-1"},
				702: {},
				90000: {
					cfgKeyTags: "bosh-cpi;bosh-parker",
					"description": parkerRecords(t, map[string]parkerProvEntry{
						transferStableID:    cidRecord("pvd-a", "data:vm-700-disk-1", "scsi4", "700"),
						resumeStatusThirdID: cidRecord("pvd-t", "data:vm-702-disk-0", "scsi5", "702"),
					}),
					"scsi5": "data:vm-90000-disk-6",
				},
			})
			if tc.configErr != nil {
				inner.configErr = map[int]error{702: tc.configErr}
			}
			inner.snapshotErr = tc.snapshotErr
			c := &storageFakeClient{scanFakeClient: inner}
			before := cloneConfig(inner.configs[700])

			_, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
			if tc.retriable {
				requireResumeRetry(t, err, tc.want)
			} else {
				requireResumeRefusal(t, err, tc.want)
			}
			requireNothingMovedOrTagged(t, inner)
			requireSourceUnchanged(t, inner, before)
		})
	}
}

// TestResumeStatus_AnotherSourceOnAnotherNodeIsAnOrdinaryRead covers C's
// source after it migrated to pve2. The read only inspects, so it finds C on
// pve2 and reads it there. When C's source names C's volume nowhere and
// storage no longer holds it, C's landing on scsi5 is C's, and A's resume
// moves A's own volume onto scsi4. When C's source still names C's volume,
// the landing on scsi5 can't be C's, and A's resume refuses and asks for an
// audit. Before the change both refused retriably, which never clears.
func TestResumeStatus_AnotherSourceOnAnotherNodeIsAnOrdinaryRead(t *testing.T) {
	t.Parallel()
	parker := func(t *testing.T) map[string]any {
		return map[string]any{
			cfgKeyTags: "bosh-cpi;bosh-parker",
			"description": parkerRecords(t, map[string]parkerProvEntry{
				transferStableID:   cidRecord("pvd-a", "data:vm-700-disk-1", "scsi4", "700"),
				resumeSlotsOtherID: cidRecord("pvd-c", "data:vm-701-disk-0", "scsi5", "701"),
			}),
			"scsi5": "data:vm-90000-disk-6",
		}
	}

	t.Run("C's move is proved", func(t *testing.T) {
		t.Parallel()
		inner := newScanFakeClient(map[int]map[string]any{
			700:   {"unused0": "data:vm-700-disk-1"},
			701:   {},
			90000: parker(t),
		})
		c := &migratedStorageClient{migratedSourceClient: &migratedSourceClient{scanFakeClient: inner, vmid: 701}}
		landed, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
		if err != nil {
			t.Fatalf("A's resume: %v", err)
		}
		if inner.eventIndex("move:700:unused0->90000:scsi4:") < 0 {
			t.Errorf("events = %v, want A's own volume moved off 700's unused0 onto scsi4", inner.events)
		}
		if got := serialSlots(inner); len(got) != 1 || got["scsi4"] != landed+",serial="+transferStableID {
			t.Errorf("slots carrying A's serial = %v, want only scsi4 holding %s", got, landed)
		}
		if got := inner.configs[90000]["scsi5"]; got != "data:vm-90000-disk-6" {
			t.Errorf("parker scsi5 = %v, want C's landing left as it was", got)
		}
	})

	t.Run("C's source still names C's volume", func(t *testing.T) {
		t.Parallel()
		inner := newScanFakeClient(map[int]map[string]any{
			700:   {"unused0": "data:vm-700-disk-1"},
			701:   {"unused0": "data:vm-701-disk-0"},
			90000: parker(t),
		})
		c := &migratedStorageClient{migratedSourceClient: &migratedSourceClient{scanFakeClient: inner, vmid: 701}}
		before := cloneConfig(inner.configs[700])
		_, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
		requireResumeRefusal(t, err, `data:vm-90000-disk-6 on scsi5 sits on the slot disk `+resumeSlotsOtherID+`'s unfinished record `+
			`names (disk cid pvd-c), but its source vm 701 still names its recorded volume data:vm-701-disk-0 on unused0, so it can't `+
			`count as that disk's, and this disk's record (disk cid pvd-a) names slot "scsi4" (audit required)`)
		requireNothingMovedOrTagged(t, inner)
		requireSourceUnchanged(t, inner, before)
	})
}

// TestResumeStatus_AStaleRecordNeedsAnAudit covers a record that outlived its
// disk's unpark. T's source names T's volume nowhere and storage no longer
// holds it, so by the metadata T moved, but no landing sits on T's slot. A's
// own landing on scsi4 may then be T's. Before the change A's resume refused
// retriably, which never clears, because T's transfer never finishes. Now it
// refuses and asks for an audit.
func TestResumeStatus_AStaleRecordNeedsAnAudit(t *testing.T) {
	t.Parallel()
	inner := newScanFakeClient(map[int]map[string]any{
		700: {},
		702: {},
		90000: {
			cfgKeyTags: "bosh-cpi;bosh-parker",
			"description": parkerRecords(t, map[string]parkerProvEntry{
				transferStableID:    cidRecord("pvd-a", "data:vm-700-disk-1", "scsi4", "700"),
				resumeStatusThirdID: cidRecord("pvd-t", "data:vm-702-disk-0", "scsi5", "702"),
			}),
			"scsi4": "data:vm-90000-disk-5",
		},
	})
	c := &storageFakeClient{scanFakeClient: inner}
	_, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
	requireResumeRefusal(t, err, `disk `+resumeStatusThirdID+`'s unfinished record (disk cid pvd-t, slot "scsi5") reads as moved, `+
		`because its source vm 702 names its recorded volume data:vm-702-disk-0 nowhere, and storage data no longer holds it, and no `+
		`landing on its slot accounts for it, so data:vm-90000-disk-5 on scsi4 may be that disk's and not this disk's (disk cid pvd-a, `+
		`slot "scsi4") (audit required)`)
	requireNothingMovedOrTagged(t, inner)
	if got := inner.configs[90000]["scsi4"]; got != "data:vm-90000-disk-5" {
		t.Errorf("parker scsi4 = %v, want the landing left bare", got)
	}
}

// TestResumeStatus_AStaleRecordFromAFailedUnparkNeedsAnAudit is a record that
// outlived a failed unpark. Disk S's unpark renamed S's volume onto 800 and
// wrote S's serial there, then failed before it removed S's record, which
// still names scsi5 on parker 90000. An older release's fallback then landed
// A on scsi5, and the CPI died before A's serial write. A new disk later took
// A's old name on 700 and was left on 700's unused0 with no serial. S's source
// names S's volume nowhere and storage no longer holds it, so by the metadata
// S moved. Before the change A's resume set A's landing aside as S's, ran the
// move window, moved the new disk onto the parker, and wrote A's serial onto
// it. Now the serial on 800 shows S's record is stale, S's status reads as
// unknown, and A's resume refuses and asks for an audit.
func TestResumeStatus_AStaleRecordFromAFailedUnparkNeedsAnAudit(t *testing.T) {
	t.Parallel()
	inner := newScanFakeClient(map[int]map[string]any{
		700: {"unused0": "data:vm-700-disk-1"},
		701: {},
		800: {"scsi0": "data:vm-800-disk-0,serial=" + resumeSlotsOtherID + ",size=10G"},
		90000: {
			cfgKeyTags: "bosh-cpi;bosh-parker",
			"description": parkerRecords(t, map[string]parkerProvEntry{
				transferStableID:   cidRecord("pvd-a", "data:vm-700-disk-1", "scsi4", "700"),
				resumeSlotsOtherID: cidRecord("pvd-s", "data:vm-701-disk-0", "scsi5", "701"),
			}),
			"scsi5": "data:vm-90000-disk-6",
		},
	})
	before := cloneConfig(inner.configs[700])
	c := &storageFakeClient{scanFakeClient: inner}
	_, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
	requireResumeRefusal(t, err, `data:vm-90000-disk-6 on scsi5 sits on the slot disk `+resumeSlotsOtherID+`'s unfinished record `+
		`names (disk cid pvd-s), but vm 800 carries its disk's serial on scsi0, which shows its record is stale, so it can't count `+
		`as that disk's, and this disk's record (disk cid pvd-a) names slot "scsi4" (audit required)`)
	requireNothingMovedOrTagged(t, inner)
	requireSourceUnchanged(t, inner, before)
	if got := inner.configs[90000]["scsi5"]; got != "data:vm-90000-disk-6" {
		t.Errorf("parker scsi5 = %v, want A's landing left bare", got)
	}
}

// listRaceClient runs onList once, at the first guest listing that follows the
// first read of the parker's pending section. That is the window between a
// resume's read of the parker and its scan of the guests, where another call
// can change the parker.
type listRaceClient struct {
	*storageFakeClient
	parker     string
	onList     func(configs map[int]map[string]any)
	parkerRead atomic.Bool
	fired      atomic.Bool
}

func (c *listRaceClient) Nodes() sdknodes.Service {
	inner := c.storageFakeClient.Nodes().(*fakeNodesService)
	pending := inner.listQemuPendingFn
	inner.listQemuPendingFn = func(ctx context.Context, node, vmidText string) (*sdknodes.ListQemuPendingResponse, error) {
		if vmidText == c.parker {
			c.parkerRead.Store(true)
		}
		return pending(ctx, node, vmidText)
	}
	list := inner.listQemuFn
	inner.listQemuFn = func(ctx context.Context, node string, params *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		if c.parkerRead.Load() && c.fired.CompareAndSwap(false, true) {
			c.mu.Lock()
			c.onList(c.configs)
			c.mu.Unlock()
		}
		return list(ctx, node, params)
	}
	return inner
}

// TestResumeStatus_AStaleRecordWhoseDiskFinishedOnThisParkerRefusesRetriably
// is the race the stale-record check meets without the parker's lock. A's
// landing sits on scsi5, the slot disk S's unfinished record names, and S's
// move reads as proved. Another call then finishes S's transfer onto this same
// parker, between A's read of the parker and A's scan of the guests, so the
// scan finds S's serial on parker 90000. That serial doesn't show a stale
// record, because the parker read had no serial for S. It shows the parker
// changed after the read. A's resume refuses retriably, and a rerun settles
// it. Before the change it refused for an audit, permanently.
func TestResumeStatus_AStaleRecordWhoseDiskFinishedOnThisParkerRefusesRetriably(t *testing.T) {
	t.Parallel()
	inner := newScanFakeClient(map[int]map[string]any{
		700: {"unused0": "data:vm-700-disk-1"},
		701: {},
		90000: {
			cfgKeyTags: "bosh-cpi;bosh-parker",
			"description": parkerRecords(t, map[string]parkerProvEntry{
				transferStableID:   cidRecord("pvd-a", "data:vm-700-disk-1", "scsi4", "700"),
				resumeSlotsOtherID: cidRecord("pvd-s", "data:vm-701-disk-0", "scsi5", "701"),
			}),
			"scsi5": "data:vm-90000-disk-6",
		},
	})
	before := cloneConfig(inner.configs[700])
	c := &listRaceClient{storageFakeClient: &storageFakeClient{scanFakeClient: inner}, parker: "90000"}
	c.onList = func(configs map[int]map[string]any) {
		configs[90000]["scsi6"] = "data:vm-90000-disk-7,serial=" + resumeSlotsOtherID + ",size=10G"
		configs[90000]["description"] = parkerRecords(t, map[string]parkerProvEntry{
			transferStableID: cidRecord("pvd-a", "data:vm-700-disk-1", "scsi4", "700"),
		})
	}

	_, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
	if !c.fired.Load() {
		t.Fatal("the parker never changed, want S's serial to land between A's parker read and A's guest scan")
	}
	requireResumeRetry(t, err, "parker vmid 90000 changed after the resume read it, because it now carries disk "+
		resumeSlotsOtherID+"'s serial on scsi6")
	requireNothingMovedOrTagged(t, inner)
	requireSourceUnchanged(t, inner, before)
	if got := inner.configs[90000]["scsi5"]; got != "data:vm-90000-disk-6" {
		t.Errorf("parker scsi5 = %v, want A's landing left bare", got)
	}
}

// TestResumeStatus_AStaleRecordWithNothingOnItsSlotLetsAClaimItsOwnLanding
// pins the answer for a stale record whose slot holds no landing. S's record
// names scsi5, which is empty, and S's serial sits on vm 800, so the record
// reads as unknown and S becomes a neighbour. A's own landing sits on scsi4,
// the slot A's record names, and A's source names A's volume nowhere while
// storage no longer holds it, so A's own move is proved. A's resume claims its
// landing. Before the stale-record check S read as moved with no landing on its
// slot, and A's resume refused for an audit. The claim is sound because S's
// serial shows S's volume is on another guest, so no bare landing on this
// parker can be S's, and the claim still needs A's own move proved.
func TestResumeStatus_AStaleRecordWithNothingOnItsSlotLetsAClaimItsOwnLanding(t *testing.T) {
	t.Parallel()
	inner := newScanFakeClient(map[int]map[string]any{
		700: {},
		701: {},
		800: {"scsi0": "data:vm-800-disk-0,serial=" + resumeSlotsOtherID + ",size=10G"},
		90000: {
			cfgKeyTags: "bosh-cpi;bosh-parker",
			"description": parkerRecords(t, map[string]parkerProvEntry{
				transferStableID:   cidRecord("pvd-a", "data:vm-700-disk-1", "scsi4", "700"),
				resumeSlotsOtherID: cidRecord("pvd-s", "data:vm-701-disk-0", "scsi5", "701"),
			}),
			"scsi4": "data:vm-90000-disk-5",
		},
	})
	c := &storageFakeClient{scanFakeClient: inner}

	if _, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{}); err != nil {
		t.Fatalf("A's resume: %v", err)
	}
	if got := serialSlots(inner); len(got) != 1 || got["scsi4"] != "data:vm-90000-disk-5,serial="+transferStableID {
		t.Errorf("slots carrying A's serial = %v, want only scsi4 holding data:vm-90000-disk-5", got)
	}
	if got := inner.configs[800]["scsi0"]; got != "data:vm-800-disk-0,serial="+resumeSlotsOtherID+",size=10G" {
		t.Errorf("vm 800 scsi0 = %v, want S's disk left as it was", got)
	}
	if got, _ := inner.configs[90000]["scsi5"].(string); got != "" {
		t.Errorf("parker scsi5 = %q, want S's empty slot left empty", got)
	}
}

// TestResumeStatus_OwnMoveMustBeProvedBesideAnUnknownRecord covers A's claim of
// the landing on its own slot while C's record names scsi5, which holds no
// landing, and C's source still names C's volume with no serial. An older
// release's fallback could have landed C on scsi4, so the claim goes ahead only
// when A's own move is proved. A source that names A's volume only under
// another disk's serial shows that name was reused, so it proves the move.
// Before the change, a gone source and a volume that still exists let the claim
// through, and a name under another serial refused with audit required. A
// missing source refused retriably, and a permanent read failure became
// retriable.
func TestResumeStatus_OwnMoveMustBeProvedBesideAnUnknownRecord(t *testing.T) {
	t.Parallel()
	neighbour := `, while disk ` + resumeSlotsOtherID + `'s unfinished record names slot "scsi5", which holds no landing, and its ` +
		`source vm 701 still names its recorded volume data:vm-701-disk-0 on unused0 (audit required)`
	claim := `data:vm-90000-disk-5 on scsi4, the slot this record names, may be this disk's (disk cid pvd-a) or disk ` +
		resumeSlotsOtherID + `'s (disk cid pvd-c), because `
	for _, tc := range []struct {
		name      string
		source    string
		own       map[string]any
		configErr error
		floating  map[string]bool
		outcome   string
		want      string
	}{
		{name: "this disk's source is gone", source: "700", configErr: goneError("700"), outcome: "refused",
			want: claim + "this disk's source vm 700 is gone from the cluster" + neighbour},
		{name: "this disk's record names no source vm", source: "", own: map[string]any{}, outcome: "refused",
			want: claim + "this disk's record names no source vm and volume" + neighbour},
		{name: "this disk's source fails to read", source: "700", configErr: errors.New("500 Internal Server Error"), outcome: "retried",
			want: "transfer resume: read source vm 700 of disk " + transferStableID},
		{name: "this disk's source read fails permanently", source: "700",
			configErr: cpierrors.Cloud("ReadQemuViews: VM 700 on node pve1: malformed pending entry: x"), outcome: "refused",
			want: "malformed pending entry"},
		{name: "this disk's volume still exists and no guest names it", source: "700", own: map[string]any{},
			floating: map[string]bool{"data:vm-700-disk-1": true}, outcome: "refused",
			want: claim + "this disk's recorded volume data:vm-700-disk-1 still exists on storage data and no guest names it" + neighbour},
		{name: "this disk's move is proved", source: "700", own: map[string]any{}, outcome: "claimed"},
		{name: "this disk's source names its volume only under another disk's serial", source: "700",
			own: map[string]any{"scsi1": "data:vm-700-disk-1,serial=" + resumeProofOtherID + ",size=10G"}, outcome: "claimed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			configs := map[int]map[string]any{
				701: {"unused0": "data:vm-701-disk-0"},
				90000: {
					cfgKeyTags: "bosh-cpi;bosh-parker",
					"description": parkerRecords(t, map[string]parkerProvEntry{
						transferStableID:   cidRecord("pvd-a", "data:vm-700-disk-1", "scsi4", tc.source),
						resumeSlotsOtherID: cidRecord("pvd-c", "data:vm-701-disk-0", "scsi5", "701"),
					}),
					"scsi4": "data:vm-90000-disk-5",
				},
			}
			if tc.own != nil {
				configs[700] = tc.own
			}
			inner := newScanFakeClient(configs)
			if tc.configErr != nil {
				inner.configErr = map[int]error{700: tc.configErr}
			}
			c := &storageFakeClient{scanFakeClient: inner, floating: tc.floating}
			intent := resumeProofIntent
			intent.SourceVMCID = tc.source

			_, err := ResumeDiskTransferToParker(context.Background(), c, nil, intent, transferStableID, transferTestCfg, ParkContext{})
			switch tc.outcome {
			case "claimed":
				if err != nil {
					t.Fatalf("A's resume: %v", err)
				}
				if got := serialSlots(inner); len(got) != 1 || got["scsi4"] != "data:vm-90000-disk-5,serial="+transferStableID {
					t.Errorf("slots carrying A's serial = %v, want only scsi4 holding data:vm-90000-disk-5", got)
				}
				return
			case "retried":
				requireResumeRetry(t, err, tc.want)
			default:
				requireResumeRefusal(t, err, tc.want)
			}
			requireNothingMovedOrTagged(t, inner)
			if got := inner.configs[90000]["scsi4"]; got != "data:vm-90000-disk-5" {
				t.Errorf("parker scsi4 = %v, want the landing left bare", got)
			}
		})
	}
}

// TestResumeStatus_CrossedFallbacksNeedAnAudit builds the end state that two
// fallbacks from an older release leave when they cross. C's record names
// scsi5 and D's names scsi7. D's resume fell back to scsi5 and C's to scsi7,
// and each died before its serial write. Both sources name their volumes
// nowhere and storage holds neither, so both moves are proved, and nothing in
// the metadata says which landing is whose. Before the change C's resume set
// the landing on scsi7 aside as D's and claimed D's volume on scsi5 as C's,
// and D's resume then did the same the other way round. Now each resume
// refuses and asks for an audit. The legitimate state, where each landed on
// its own slot, reads the same and refuses the same way.
func TestResumeStatus_CrossedFallbacksNeedAnAudit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		scsi5, scsi7 string
	}{
		{"each landed on the other's slot", "data:vm-90000-disk-8", "data:vm-90000-disk-9"},
		{"each landed on its own slot", "data:vm-90000-disk-9", "data:vm-90000-disk-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inner := newScanFakeClient(map[int]map[string]any{
				701: {},
				702: {},
				90000: {
					cfgKeyTags: "bosh-cpi;bosh-parker",
					"description": parkerRecords(t, map[string]parkerProvEntry{
						resumeSlotsOtherID:  cidRecord("pvd-c", "data:vm-701-disk-0", "scsi5", "701"),
						resumeStatusThirdID: cidRecord("pvd-d", "data:vm-702-disk-0", "scsi7", "702"),
					}),
					"scsi5": tc.scsi5,
					"scsi7": tc.scsi7,
				},
			})
			c := &storageFakeClient{scanFakeClient: inner}

			_, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeSlotsOtherIntent("scsi5"), resumeSlotsOtherID, transferTestCfg, ParkContext{})
			requireResumeRefusal(t, err, tc.scsi7+` on scsi7 counts as disk `+resumeStatusThirdID+`'s (disk cid pvd-d, slot "scsi7") only `+
				`because its record reads as moved, since its source vm 702 names its recorded volume data:vm-702-disk-0 nowhere, and `+
				`storage data no longer holds it, and no serial says so, so `+tc.scsi5+` on scsi5 may be that disk's and not this disk's `+
				`(disk cid pvd-c, slot "scsi5") (audit required)`)

			_, err = ResumeDiskTransferToParker(context.Background(), c, nil, thirdIntent("scsi7", "data:vm-702-disk-0", "702"),
				resumeStatusThirdID, transferTestCfg, ParkContext{})
			requireResumeRefusal(t, err, tc.scsi5+` on scsi5 counts as disk `+resumeSlotsOtherID+`'s (disk cid pvd-c, slot "scsi5") only `+
				`because its record reads as moved, since its source vm 701 names its recorded volume data:vm-701-disk-0 nowhere, and `+
				`storage data no longer holds it, and no serial says so, so `+tc.scsi7+` on scsi7 may be that disk's and not this disk's `+
				`(disk cid pvd-d, slot "scsi7") (audit required)`)

			requireNoMoveOrSerial(t, inner.events, resumeSlotsOtherID, resumeStatusThirdID)
			if inner.configs[90000]["scsi5"] != tc.scsi5 || inner.configs[90000]["scsi7"] != tc.scsi7 {
				t.Errorf("parker scsi5 = %v and scsi7 = %v, want both landings left bare", inner.configs[90000]["scsi5"], inner.configs[90000]["scsi7"])
			}
		})
	}
}

// TestResumeStatus_TwoRecordsShareASourceAndVolume covers two unfinished
// records on one parker that name the same source VM and recorded volume.
// That happens only when a new disk took the first disk's freed name and its
// own transfer then started, so one of the two records names a volume that
// isn't its disk's. Before the change A's resume moved the volume on 700's
// unused0 with A's serial. Now each resume refuses and asks for an audit,
// with no landing on the parker at all.
func TestResumeStatus_TwoRecordsShareASourceAndVolume(t *testing.T) {
	t.Parallel()
	inner := newScanFakeClient(map[int]map[string]any{
		700: {"unused0": "data:vm-700-disk-1"},
		90000: {
			cfgKeyTags: "bosh-cpi;bosh-parker",
			"description": parkerRecords(t, map[string]parkerProvEntry{
				transferStableID:   cidRecord("pvd-a", "data:vm-700-disk-1", "scsi4", "700"),
				resumeProofOtherID: cidRecord("pvd-b", "data:vm-700-disk-1", "scsi6", "700"),
			}),
		},
	})
	before := cloneConfig(inner.configs[700])

	_, err := ResumeDiskTransferToParker(context.Background(), inner, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
	requireResumeRefusal(t, err, `disk `+resumeProofOtherID+`'s unfinished record (disk cid pvd-b, slot "scsi6") names the same source vm `+
		`700 and recorded volume data:vm-700-disk-1 as this disk's record (disk cid pvd-a, slot "scsi4"), so one of the two names a `+
		`volume that took the other's old name (audit required)`)

	_, err = ResumeDiskTransferToParker(context.Background(), inner, nil, thirdIntent("scsi6", "data:vm-700-disk-1", "700"),
		resumeProofOtherID, transferTestCfg, ParkContext{})
	requireResumeRefusal(t, err, `disk `+transferStableID+`'s unfinished record (disk cid pvd-a, slot "scsi4") names the same source vm `+
		`700 and recorded volume data:vm-700-disk-1 as this disk's record (disk cid pvd-b, slot "scsi6"), so one of the two names a `+
		`volume that took the other's old name (audit required)`)

	requireNoMoveOrSerial(t, inner.events, transferStableID, resumeProofOtherID)
	requireSourceUnchanged(t, inner, before)
}
