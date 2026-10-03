package pve

// A disk's move to the parker can land and then crash before its serial write.
// The record still names the volume's pre-move name, which PVE freed when it
// renamed the volume for the parker, and another disk B can take that name on
// the same source VM. These rows put B's volume on the source under the
// recorded name and check that the resume neither moves it nor writes the
// disk's serial onto it, and that the paths with no reuse still converge.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// resumeProofOtherID is disk B's serial.
const resumeProofOtherID = "bpd-99887766ffeeddcc"

// resumeProofRecord is a parker description that keeps a record of the
// transfer of stableID from source under volid, aimed at slot.
func resumeProofRecord(stableID, volid, slot, source string) string {
	return `<!--BOSH:{"bosh_parked_disks":{"` + stableID + `":{"disk_cid":"pvd-x","parked_at":"t","node":"pve1",` +
		`"volid":"` + volid + `","slot":"` + slot + `","source_vm_cid":"` + source + `"}}}-->`
}

// resumeProofIntent is the record the resume follows. It names slot scsi4 of
// parker 90000 and source 700's recorded name, data:vm-700-disk-1.
var resumeProofIntent = DiskTransferIntent{
	ParkerVMID: 90000, ParkerNode: "pve1", Slot: "scsi4", Volid: "data:vm-700-disk-1", SourceVMCID: "700",
}

// resumeProofParker is parker 90000 with the record of resumeProofIntent and
// the given disk keys.
func resumeProofParker(keys map[string]any) map[string]any {
	cfg := map[string]any{
		cfgKeyTags:    "bosh-cpi;bosh-parker",
		"description": resumeProofRecord(transferStableID, "data:vm-700-disk-1", "scsi4", "700"),
	}
	for key, value := range keys {
		cfg[key] = value
	}
	return cfg
}

func cloneConfig(cfg map[string]any) map[string]any {
	out := make(map[string]any, len(cfg))
	for key, value := range cfg {
		out[key] = value
	}
	return out
}

// requireResumeRefusal fails unless err is a Cloud error that isn't retriable
// and whose text contains want.
func requireResumeRefusal(t *testing.T, err error, want string) {
	t.Helper()
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || typed.Type() != cpierrors.TypeCloud || typed.OkToRetry() {
		t.Errorf("resume error = %v, want a Cloud error that isn't retriable", err)
		return
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("resume error = %q, want it to contain %q", err.Error(), want)
	}
}

// requireNothingMovedOrTagged fails when the resume sent a move, wrote the
// disk's serial anywhere, or finalized a record.
func requireNothingMovedOrTagged(t *testing.T, c *scanFakeClient) {
	t.Helper()
	for _, event := range c.events {
		if strings.HasPrefix(event, "move:") || strings.Contains(event, "serial="+transferStableID) || strings.HasPrefix(event, "description:") {
			t.Errorf("events = %v, want no move, no serial write, and no record write", c.events)
			return
		}
	}
}

// requireSourceUnchanged fails unless source 700 has the config it had before.
func requireSourceUnchanged(t *testing.T, c *scanFakeClient, before map[string]any) {
	t.Helper()
	if got := c.configs[700]; !reflect.DeepEqual(got, before) {
		t.Fatalf("source 700 = %v, want it unchanged from %v", got, before)
	}
}

// TestResumeProof_StaleIntentClaimsTheLandingAndLeavesTheReusedName is the
// stale-intent row. The disk's move landed on the recorded slot and lost its
// serial write, and B took the freed name on 700's unused1. Before the proof
// the move window ran first, moved B to the parker, and wrote the disk's
// serial onto it.
func TestResumeProof_StaleIntentClaimsTheLandingAndLeavesTheReusedName(t *testing.T) {
	t.Parallel()
	c := newScanFakeClient(map[int]map[string]any{
		700:   {"unused1": "data:vm-700-disk-1"},
		90000: resumeProofParker(map[string]any{"scsi4": "data:vm-90000-disk-3"}),
	})
	before := cloneConfig(c.configs[700])
	landed, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if landed != "data:vm-90000-disk-3" {
		t.Fatalf("landed = %q, want the disk's own landing data:vm-90000-disk-3", landed)
	}
	if c.eventIndex("move:") >= 0 {
		t.Fatalf("events = %v, want no move", c.events)
	}
	if got, _ := c.configs[90000]["scsi4"].(string); got != "data:vm-90000-disk-3,serial="+transferStableID {
		t.Fatalf("parker scsi4 = %q, want the serial on the disk's own landing", got)
	}
	requireSourceUnchanged(t, c, before)
}

// TestResumeProof_LandingOnAnotherSlotRefuses is the fallback-slot row. The
// landing sits on scsi7, not the recorded scsi4, so nothing ties it to this
// disk. Before the proof the move window moved B and wrote the disk's serial
// onto it.
func TestResumeProof_LandingOnAnotherSlotRefuses(t *testing.T) {
	t.Parallel()
	c := newScanFakeClient(map[int]map[string]any{
		700:   {"unused1": "data:vm-700-disk-1"},
		90000: resumeProofParker(map[string]any{"scsi7": "data:vm-90000-disk-3"}),
	})
	before := cloneConfig(c.configs[700])
	_, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
	requireResumeRefusal(t, err, `scsi7 of parker vmid 90000 holds data:vm-90000-disk-3, a volume named for the parker with no serial, while the record names slot "scsi4"`)
	requireNothingMovedOrTagged(t, c)
	requireSourceUnchanged(t, c, before)
}

// TestResumeProof_SecondLandingRefusesTheClaim covers a second volume named
// for the parker with no serial, in the current view or only as a pending
// value. Before the proof the resume claimed the recorded slot on the slot's
// shape alone and wrote the disk's serial onto it.
func TestResumeProof_SecondLandingRefusesTheClaim(t *testing.T) {
	t.Parallel()

	t.Run("current view", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			700: {},
			90000: resumeProofParker(map[string]any{
				"scsi4": "data:vm-90000-disk-3",
				"scsi5": "data:vm-90000-disk-4",
			}),
		})
		_, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
		requireResumeRefusal(t, err, "parker vmid 90000 holds more than one volume named for it with no serial, data:vm-90000-disk-3 on scsi4 and data:vm-90000-disk-4 on scsi5")
		requireNothingMovedOrTagged(t, c)
	})

	t.Run("pending view", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			700: {},
			90000: resumeProofParker(map[string]any{
				"scsi4": "data:vm-90000-disk-3",
				"scsi5": "data:vm-90000-disk-4",
			}),
		})
		c.replaced = map[int]map[string]any{90000: {"scsi5": "data:vm-90000-disk-8,serial=" + resumeProofOtherID}}
		_, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
		requireResumeRefusal(t, err, "data:vm-90000-disk-4 on scsi5")
		requireNothingMovedOrTagged(t, c)
	})
}

// TestResumeProof_TwoSourceKeysNameTheVolumeRefuses is the two-key row. The
// recorded name sits on unused0 and as the pending value of unused1. Before
// the proof the move window moved whichever key it met first.
func TestResumeProof_TwoSourceKeysNameTheVolumeRefuses(t *testing.T) {
	t.Parallel()
	c := newScanFakeClient(map[int]map[string]any{
		700: {
			"unused0": "data:vm-700-disk-1",
			"unused1": "data:vm-700-disk-1",
		},
		90000: resumeProofParker(nil),
	})
	c.replaced = map[int]map[string]any{700: {"unused1": "data:vm-700-disk-9"}}
	before := cloneConfig(c.configs[700])
	_, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
	requireResumeRefusal(t, err, "disk keys unused0, unused1 of source vm 700 all name volume data:vm-700-disk-1")
	requireNothingMovedOrTagged(t, c)
	requireSourceUnchanged(t, c, before)
}

// TestResumeProof_AnotherParkerKeepsARecordRefuses covers a second parker that
// keeps a record of the disk's transfer, ahead of a move and ahead of a claim.
// Before the proof the resume read only its own parker, so it moved the
// source's entry or claimed the recorded slot.
func TestResumeProof_AnotherParkerKeepsARecordRefuses(t *testing.T) {
	t.Parallel()
	other := map[string]any{
		cfgKeyTags:    "bosh-cpi;bosh-parker",
		"description": resumeProofRecord(transferStableID, "data:vm-700-disk-1", "scsi2", "700"),
	}
	const want = "parker vmid 90001 keeps a record of the disk's transfer as well as parker vmid 90000"

	t.Run("before a move", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			700:   {"unused0": "data:vm-700-disk-1"},
			90000: resumeProofParker(nil),
			90001: cloneConfig(other),
		})
		before := cloneConfig(c.configs[700])
		_, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
		requireResumeRefusal(t, err, want)
		requireNothingMovedOrTagged(t, c)
		requireSourceUnchanged(t, c, before)
	})

	t.Run("before a claim", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			700:   {},
			90000: resumeProofParker(map[string]any{"scsi4": "data:vm-90000-disk-3"}),
			90001: cloneConfig(other),
		})
		_, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
		requireResumeRefusal(t, err, want)
		requireNothingMovedOrTagged(t, c)
	})
}

// TestResumeProof_SerialOnParkerFinishesDespiteAnotherRecord is the guard row
// for the window where the disk's serial already sits on a parker slot. The
// serial proves which volume is the disk, so a second parker's record doesn't
// stop the resume. It finishes on the parker that holds the serial, writes
// nothing to the other parker, and leaves that parker's record in place.
func TestResumeProof_SerialOnParkerFinishesDespiteAnotherRecord(t *testing.T) {
	t.Parallel()
	other := map[string]any{
		cfgKeyTags:    "bosh-cpi;bosh-parker",
		"description": resumeProofRecord(transferStableID, "data:vm-700-disk-1", "scsi2", "700"),
	}
	c := newScanFakeClient(map[int]map[string]any{
		700:   {},
		90000: resumeProofParker(map[string]any{"scsi4": "data:vm-90000-disk-3,serial=" + transferStableID}),
		90001: cloneConfig(other),
	})
	landed, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if landed != "data:vm-90000-disk-3" {
		t.Errorf("landed = %q, want the volume that carries the serial, data:vm-90000-disk-3", landed)
	}
	if c.eventIndex("description:90000") < 0 {
		t.Errorf("events = %v, want the record on parker 90000 finalized", c.events)
	}
	for _, event := range c.events {
		if strings.Contains(event, "90001") {
			t.Errorf("events = %v, want nothing written to parker 90001", c.events)
			break
		}
	}
	if !reflect.DeepEqual(c.configs[90001], other) {
		t.Errorf("parker 90001 = %v, want it and its record unchanged from %v", c.configs[90001], other)
	}
}

// TestResumeProof_AnotherHolderRefusesTheMove covers a guest that carries the
// disk's serial and a parker that names the recorded volume. Before the proof
// the move window moved the source's entry without reading either.
func TestResumeProof_AnotherHolderRefusesTheMove(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		guest map[int]map[string]any
		want  string
	}{
		{
			name:  "a guest carries the serial",
			guest: map[int]map[string]any{701: {"scsi0": "data:vm-701-disk-0,serial=" + transferStableID}},
			want:  "vm 701 holds the disk's serial on scsi0",
		},
		{
			name:  "another parker names the volume",
			guest: map[int]map[string]any{90001: {cfgKeyTags: "bosh-cpi;bosh-parker", "unused0": "data:vm-700-disk-1"}},
			want:  "parker vmid 90001 names volume data:vm-700-disk-1 on unused0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			configs := map[int]map[string]any{
				700:   {"unused0": "data:vm-700-disk-1"},
				90000: resumeProofParker(nil),
			}
			for vmid, cfg := range tc.guest {
				configs[vmid] = cloneConfig(cfg)
			}
			c := newScanFakeClient(configs)
			before := cloneConfig(c.configs[700])
			_, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
			requireResumeRefusal(t, err, tc.want)
			requireNothingMovedOrTagged(t, c)
			requireSourceUnchanged(t, c, before)
		})
	}
}

// TestResumeProof_PendingDeleteWithAnotherSerialRefuses covers a source bus
// slot whose delete is pending and whose drive carries B's serial under the
// recorded name. Before the proof the resume handed back the found pending
// delete for the caller to class, and a caller that moves the disk off a
// stopped source then applied it and moved B.
func TestResumeProof_PendingDeleteWithAnotherSerialRefuses(t *testing.T) {
	t.Parallel()
	c := newScanFakeClient(map[int]map[string]any{
		700:   {"hotplug": "network,usb"},
		90000: resumeProofParker(nil),
	})
	c.held = map[int]map[string]any{700: {"scsi1": "data:vm-700-disk-1,serial=" + resumeProofOtherID + ",size=10G"}}
	_, err := ResumeDiskTransferToParker(noBackoff(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{DiskCID: "pvd-test"})
	requireResumeRefusal(t, err, "scsi1 of source vm 700 names volume data:vm-700-disk-1 with another disk's serial "+resumeProofOtherID)
	requireNothingMovedOrTagged(t, c)
	if _, held := c.held[700]["scsi1"]; !held {
		t.Fatal("the resume cleared B's pending delete")
	}
}

// TestResumeProof_ReleasedVolumeWithALandingRefusesTheAttach covers the
// config-edit park of a volume the source released. A landing on another slot
// of the parker and a guest carrying the disk's serial each stop it. Before
// the proof the resume attached the recorded volume with the disk's serial.
func TestResumeProof_ReleasedVolumeWithALandingRefusesTheAttach(t *testing.T) {
	t.Parallel()

	t.Run("a landing on another slot", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			700:   {},
			90000: resumeProofParker(map[string]any{"scsi7": "data:vm-90000-disk-3"}),
		})
		_, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
		requireResumeRefusal(t, err, "scsi7 of parker vmid 90000 holds data:vm-90000-disk-3")
		requireNothingMovedOrTagged(t, c)
	})

	t.Run("a guest carries the serial", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			700:   {},
			701:   {"scsi0": "data:vm-701-disk-0,serial=" + transferStableID},
			90000: resumeProofParker(nil),
		})
		_, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
		requireResumeRefusal(t, err, "vm 701 holds the disk's serial on scsi0")
		requireNothingMovedOrTagged(t, c)
	})
}

// TestResumeProof_FailedReadIsRetriableAndMovesNothing covers a failed read of
// the parker's views and of another guest during the cluster scan. Each keeps
// its retriable class and never counts as an absence. Before the proof
// neither read happened and the resume moved the source's entry.
func TestResumeProof_FailedReadIsRetriableAndMovesNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		vmid int
	}{
		{"the parker's views", 90000},
		{"another guest in the cluster scan", 702},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newScanFakeClient(map[int]map[string]any{
				700:   {"unused0": "data:vm-700-disk-1"},
				702:   {},
				90000: resumeProofParker(nil),
			})
			c.pendingErrOnce = map[int]error{tc.vmid: &sdkerrors.ConnectionError{Host: "pve1", Port: 8006, Message: "connection reset by peer"}}
			before := cloneConfig(c.configs[700])
			_, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
			var typed *cpierrors.Error
			if !errors.As(err, &typed) || !typed.OkToRetry() {
				t.Errorf("resume error = %v, want a retriable error", err)
			}
			requireNothingMovedOrTagged(t, c)
			requireSourceUnchanged(t, c, before)
		})
	}
}

// TestResumeProof_ControlsStillConverge holds control rows that pass before
// and after the proof. A move that never ran still moves the disk and writes
// its serial, a move that landed on the recorded slot is still claimed, and a
// source that no longer exists still takes the config-edit park, each with
// the resume's own parker keeping the record.
func TestResumeProof_ControlsStillConverge(t *testing.T) {
	t.Parallel()

	t.Run("the move never ran", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			700:   {"unused0": "data:vm-700-disk-1"},
			90000: resumeProofParker(nil),
		})
		landed, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if c.eventIndex("move:700:unused0->90000:scsi4:") < 0 {
			t.Fatalf("events = %v, want the move of unused0 onto scsi4", c.events)
		}
		if got, _ := c.configs[90000]["scsi4"].(string); got != landed+",serial="+transferStableID {
			t.Fatalf("parker scsi4 = %q, want %s with the serial", got, landed)
		}
		if len(FindUnusedDiskEntries(c.configs[700])) != 0 {
			t.Fatalf("source 700 = %v, want no unused entry left", c.configs[700])
		}
	})

	t.Run("the move landed on the recorded slot", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			700:   {},
			90000: resumeProofParker(map[string]any{"scsi4": "data:vm-90000-disk-3"}),
		})
		landed, err := ResumeDiskTransferToParker(context.Background(), c, nil, resumeProofIntent, transferStableID, transferTestCfg, ParkContext{})
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if landed != "data:vm-90000-disk-3" || c.eventIndex("move:") >= 0 {
			t.Fatalf("landed = %q, events = %v, want the recorded slot claimed with no move", landed, c.events)
		}
	})

	t.Run("the source no longer exists", func(t *testing.T) {
		t.Parallel()
		c := newScanFakeClient(map[int]map[string]any{
			90000: resumeProofParker(nil),
		})
		c.configErr = map[int]error{
			700: errors.New("Configuration file 'nodes/pve1/qemu-server/700.conf' does not exist"),
		}
		intent := resumeProofIntent
		intent.Volid = "data:vm-11949-disk-0"
		landed, err := ResumeDiskTransferToParker(context.Background(), c, nil, intent, transferStableID, transferTestCfg, ParkContext{})
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if got, _ := c.configs[90000]["scsi0"].(string); landed != "data:vm-11949-disk-0" || got != landed+",serial="+transferStableID {
			t.Fatalf("landed = %q, parker scsi0 = %q, want the volume attached under its own name with the serial", landed, got)
		}
	})
}

// TestResumeProofRunbookHeadingExists pins the heading the refusal quotes to a
// heading that exists in docs/troubleshooting.md.
func TestResumeProofRunbookHeadingExists(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "docs", "troubleshooting.md"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.Lines(string(doc)) {
		if strings.HasPrefix(line, "#") && strings.TrimSpace(strings.TrimLeft(line, "#")) == resumeProofRunbookHeading {
			return
		}
	}
	t.Fatalf("docs/troubleshooting.md has no heading %q", resumeProofRunbookHeading)
}
