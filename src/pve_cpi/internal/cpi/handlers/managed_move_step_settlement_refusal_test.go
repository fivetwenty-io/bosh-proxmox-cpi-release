package handlers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// sourceVMShapes are the shapes whose move takes the disk from 777's unused
// entry: the detach itself and the resumes an attach_disk or a delete_disk
// sends for a park the detach left there.
var sourceVMShapes = unfiredMoveShapes[:3]

// freeKey returns the first key of VM vmid, counting up from prefix0, that
// the VM's configuration doesn't hold.
func (m *unfiredMove) freeKey(t *testing.T, vmid int, prefix string) string {
	t.Helper()
	for i := range 30 {
		key := fmt.Sprintf("%s%d", prefix, i)
		if _, taken := m.client.state.configs[vmid][key]; !taken {
			return key
		}
	}
	t.Fatalf("VM %d has no free %s key: %v", vmid, prefix, m.client.state.configs[vmid])
	return ""
}

// reuseName lands the task PVE forked for the lost POST, which frees the
// volume's name on the source, and then hands that name to disk B on key of
// the source with value's options, the way PVE's find_free_diskname gives the
// lowest free index to the next volume renamed onto the same VM. It returns
// the lost move, which names the parker and the slot A landed on.
func (m *unfiredMove) reuseName(t *testing.T, key, options string) lostMove {
	t.Helper()
	volume := m.step.Target.IntendedVolume
	info := *m.client.state.volumes[volume]
	if outcome := m.client.runLostMove(0); !strings.HasPrefix(outcome, "moved ") {
		t.Fatalf("the forked task did not land: %s", outcome)
	}
	if m.client.state.volumes[volume] != nil {
		t.Fatalf("the landed task left %s behind", volume)
	}
	m.client.state.volumes[volume] = &info
	m.volumes++
	cfg := m.client.state.configs[m.source]
	cfg[key] = volume + options
	m.client.generation++
	cfg["digest"] = fmt.Sprint(m.client.generation + 100)
	return m.client.lostMoves[0]
}

// diskBToken is disk B's serial, a stable ID that is not the record's.
const diskBToken = pve.DiskStableIDPrefix + "0b0b0b0b-0b0b-4b0b-8b0b-0b0b0b0b0b0b"

// TestMoveSettlementRefusesNameReusedOnBusSlot lands A's lost move and then
// gives A's old volume name to disk B on a bus slot of the source, with B's
// serial. The source names the volume on exactly one key, the volume exists,
// and nothing else holds A's serial or names the volume, so before this
// change the settlement settled A's step with VolIDs naming B's data. The
// slot carries another disk's serial, so the settlement now refuses, leaves
// the record unchanged, and deletes nothing.
func TestMoveSettlementRefusesNameReusedOnBusSlot(t *testing.T) {
	for _, shape := range unfiredMoveShapes {
		t.Run(shape.name, func(t *testing.T) {
			m := shape.build(t, true)
			key := m.freeKey(t, m.source, "scsi")
			m.reuseName(t, key, ",serial="+diskBToken)
			volume := m.step.Target.IntendedVolume
			m.requireRefusedBy(t, func() error { return m.finalizeCleanup(settleAt(pastQuietPeriod)) },
				fmt.Sprintf("%s of VM %d names volume %s with another disk's serial %s", key, m.source, volume, diskBToken))
			if value, _ := m.client.state.configs[m.source][key].(string); value != volume+",serial="+diskBToken || m.client.state.volumes[volume] == nil {
				t.Fatalf("disk B changed: %d.%s is %q", m.source, key, value)
			}
		})
	}
}

// TestMoveSettlementRefusesNameReusedOnUnusedEntry lands A's lost move from
// 777's unused entry and then leaves A's old volume name to disk B on an
// unused entry of 777. Before this change the settlement settled A's step,
// because the source names the volume and an unused entry carries no serial.
// The parker that kept A's transfer record now holds a volume named for it
// with no serial, on the recorded slot, so the settlement refuses, leaves the
// record unchanged, and deletes nothing.
func TestMoveSettlementRefusesNameReusedOnUnusedEntry(t *testing.T) {
	for _, shape := range sourceVMShapes {
		t.Run(shape.name, func(t *testing.T) {
			m := shape.build(t, true)
			key := m.freeKey(t, m.source, "unused")
			landed := m.reuseName(t, key, "")
			volume := m.step.Target.IntendedVolume
			m.requireRefusedBy(t, func() error { return m.finalizeCleanup(settleAt(pastQuietPeriod)) }, landingText(landed.slot, landed.target))
			if value, _ := m.client.state.configs[m.source][key].(string); value != volume || m.client.state.volumes[volume] == nil {
				t.Fatalf("disk B changed: %d.%s is %q", m.source, key, value)
			}
		})
	}
}

// setDiskKey sets key of VM vmid to value, or removes it when value is empty,
// and gives the VM a new digest, the way any write of its configuration does.
func (m *unfiredMove) setDiskKey(vmid int, key, value string) {
	cfg := m.client.state.configs[vmid]
	if value == "" {
		delete(cfg, key)
	} else {
		cfg[key] = value
	}
	m.client.generation++
	cfg["digest"] = fmt.Sprint(m.client.generation + 100)
}

// landingText is the refusal for a parker that holds a volume named for it
// with no serial on key.
func landingText(key string, parker int) string {
	return fmt.Sprintf("%s of parker %d, which keeps the disk's transfer record, holds a volume named for the parker with no serial", key, parker)
}

// TestMoveSettlementRefusesLandingOnFallbackSlot lands A's lost move, gives
// A's old volume name to disk B on an unused entry of 777, and then leaves
// the landed volume on another slot of the parker with the recorded slot
// empty, the way a resume that lands on a fallback slot leaves it. A check of
// the recorded slot alone sees nothing there, so the settlement would settle
// A's step with VolIDs naming B's data. Every disk key of the parker is read,
// so the settlement refuses, leaves the record unchanged, and deletes nothing.
func TestMoveSettlementRefusesLandingOnFallbackSlot(t *testing.T) {
	for _, shape := range sourceVMShapes {
		t.Run(shape.name, func(t *testing.T) {
			m := shape.build(t, true)
			key := m.freeKey(t, m.source, "unused")
			landed := m.reuseName(t, key, "")
			parker := m.client.state.configs[landed.target]
			fallback := m.freeKey(t, landed.target, "scsi")
			value, _ := parker[landed.slot].(string)
			m.setDiskKey(landed.target, fallback, value)
			m.setDiskKey(landed.target, landed.slot, "")
			m.requireRefusedBy(t, func() error { return m.finalizeCleanup(settleAt(pastQuietPeriod)) }, landingText(fallback, landed.target))
			if _, held := parker[landed.slot]; held || parker[fallback] != value || m.client.state.volumes[strings.Split(value, ",")[0]] == nil {
				t.Fatalf("the parker's landing changed: %v", parker)
			}
		})
	}
}

// keepingParker returns the one parker whose description names the disk's
// token, which is the parker that keeps the disk's transfer record.
func (m *unfiredMove) keepingParker(t *testing.T) int {
	t.Helper()
	var keepers []int
	for vmid, cfg := range m.client.state.configs {
		tags, _ := cfg["tags"].(string)
		if pve.TagsMarkParker(tags) && strings.Contains(pve.DescriptionFromConfig(cfg), m.token) {
			keepers = append(keepers, vmid)
		}
	}
	if len(keepers) != 1 {
		t.Fatalf("want one parker keeping the disk's transfer record, got %v", keepers)
	}
	return keepers[0]
}

// TestMoveSettlementRefusesBesideAnotherDisksLanding leaves A's move unfired
// and gives the parker that keeps A's transfer record another disk's
// unclaimed landing, a volume named for the parker with no serial. Nothing
// ties that volume to A, but the settlement can't tell it from A's own
// landing, so it refuses, leaves the record unchanged, and deletes nothing.
// That's an accepted cost, because such a landing is itself an exception that
// should clear within the grace hour.
func TestMoveSettlementRefusesBesideAnotherDisksLanding(t *testing.T) {
	for _, shape := range sourceVMShapes {
		t.Run(shape.name, func(t *testing.T) {
			m := shape.build(t, false)
			parker := m.keepingParker(t)
			storage, _, err := pve.ParseDiskCID(m.step.Target.IntendedVolume)
			if err != nil {
				t.Fatal(err)
			}
			other := fmt.Sprintf("%s:%d/vm-%d-disk-90.raw", storage, parker, parker)
			info := *m.client.state.volumes[m.step.Target.IntendedVolume]
			m.client.state.volumes[other] = &info
			m.volumes++
			slot := m.freeKey(t, parker, "scsi")
			m.setDiskKey(parker, slot, other)
			m.requireRefusedBy(t, func() error { return m.finalizeCleanup(settleAt(pastQuietPeriod)) }, landingText(slot, parker))
			if m.client.state.configs[parker][slot] != other || m.client.state.volumes[other] == nil {
				t.Fatalf("the other disk's landing changed: %v", m.client.state.configs[parker])
			}
		})
	}
}

// TestMoveSettlementRefusesWhenTwoParkersKeepRecords leaves A's move unfired
// and gives a second parker a copy of the record the first parker keeps. Each
// record names the transfer from the source under the volume, so either one
// proves the transfer, and the landing check could pass on one parker while
// the move landed on the other. The settlement refuses, leaves the record
// unchanged, and deletes nothing.
func TestMoveSettlementRefusesWhenTwoParkersKeepRecords(t *testing.T) {
	for _, shape := range sourceVMShapes {
		t.Run(shape.name, func(t *testing.T) {
			m := shape.build(t, false)
			first := m.keepingParker(t)
			second := first + 1
			for m.client.state.configs[second] != nil {
				second++
			}
			keeper := m.client.state.configs[first]
			m.client.state.configs[second] = map[string]any{"name": fmt.Sprintf("bosh-parker-%d", second), "tags": keeper["tags"], "digest": "1", "description": keeper["description"]}
			m.requireRefusedBy(t, func() error { return m.finalizeCleanup(settleAt(pastQuietPeriod)) },
				fmt.Sprintf("parkers %d and %d each keep a record of the disk's transfer", first, second))
		})
	}
}

// TestMoveSettlementRefusesWhenVolumeNotListed removes the volume from
// storage while the source still names it. Proof 2 finds it in no listing,
// so the settlement refuses and leaves the record unchanged.
func TestMoveSettlementRefusesWhenVolumeNotListed(t *testing.T) {
	for _, shape := range unfiredMoveShapes {
		t.Run(shape.name, func(t *testing.T) {
			m := shape.build(t, false)
			volume := m.step.Target.IntendedVolume
			delete(m.client.state.volumes, volume)
			m.volumes--
			m.sourceKey(t)
			m.requireRefusedBy(t, func() error { return m.finalizeCleanup(settleAt(pastQuietPeriod)) },
				fmt.Sprintf("volume %s is not listed on node n1", volume))
		})
	}
}

// settlementFaultPVE fails one of the reads the settlement makes and passes
// every other call to the flow fake. A fault armed by armOnTaskListing waits
// until the settlement lists the active move tasks, so the reads an operation
// makes before its settlement still answer.
type settlementFaultPVE struct {
	*lifecycleFlowPVE
	armed                       *bool
	listQemuErr, listContentErr error
	contentErr                  error
	content                     *nodes.GetStorageContentResponse
}

func (c settlementFaultPVE) Nodes() nodes.Service {
	inner, _ := c.lifecycleFlowPVE.Nodes().(lifecycleFlowNodes)
	return settlementFaultNodes{lifecycleFlowNodes: inner, f: c}
}

type settlementFaultNodes struct {
	lifecycleFlowNodes
	f settlementFaultPVE
}

func (n settlementFaultNodes) live() bool { return n.f.armed == nil || *n.f.armed }

func (n settlementFaultNodes) ListQemu(ctx context.Context, node string, p *nodes.ListQemuParams) (*nodes.ListQemuResponse, error) {
	if n.f.listQemuErr != nil && n.live() {
		return nil, n.f.listQemuErr
	}
	return n.lifecycleFlowNodes.ListQemu(ctx, node, p)
}

func (n settlementFaultNodes) ListStorageContent(ctx context.Context, node, pool string, p *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
	if n.f.listContentErr != nil && n.live() {
		return nil, n.f.listContentErr
	}
	return n.lifecycleFlowNodes.ListStorageContent(ctx, node, pool, p)
}

func (n settlementFaultNodes) GetStorageContent(ctx context.Context, node, pool, volume string) (*nodes.GetStorageContentResponse, error) {
	if n.live() {
		if n.f.contentErr != nil {
			return nil, n.f.contentErr
		}
		if n.f.content != nil {
			return n.f.content, nil
		}
	}
	return n.lifecycleFlowNodes.GetStorageContent(ctx, node, pool, volume)
}

// armOnTaskListing returns a flag the settlement's task listing sets.
func (m *unfiredMove) armOnTaskListing() *bool {
	armed := new(bool)
	m.client.onActiveMoveTasks = func(string) { *armed = true }
	return armed
}

// failConfigReadAfterListing fails every config read of vmid with err once
// the settlement has listed the active move tasks.
func (m *unfiredMove) failConfigReadAfterListing(vmid int, err error) {
	armed := m.armOnTaskListing()
	m.client.onConfigRead = func(id int) error {
		if *armed && id == vmid {
			return err
		}
		return nil
	}
}

// noMoveTaskReaderPVE hides the flow fake's active move task listing.
type noMoveTaskReaderPVE struct{ pve.Client }

var (
	errSettlementRead = errors.New("connection reset by peer")
	errConfigMissing  = sdkerrors.ParseAPIError(500, []byte(`{"message":"Configuration file 'nodes/n1/qemu-server/777.conf' does not exist"}`))
	errVolumeNotFound = sdkerrors.ParseAPIError(404, []byte(`{"message":"volume not found"}`))
	errVolumeRead     = sdkerrors.ParseAPIError(500, []byte(`{"message":"storage 'a' is not online"}`))
)

// TestMoveSettlementRefusalBranches runs each refusal of the settlement that
// no other row reaches, through finalize-cleanup past the quiet period. Each
// refuses with its own gap, leaves the record unchanged, and deletes nothing.
func TestMoveSettlementRefusalBranches(t *testing.T) {
	for _, row := range []struct {
		name  string
		shape func(t *testing.T, lose bool) *unfiredMove
		setup func(t *testing.T, m *unfiredMove) string
	}{
		{"guest-listing-failed", unfiredDetach, func(_ *testing.T, m *unfiredMove) string {
			m.deps.PVE = settlementFaultPVE{lifecycleFlowPVE: m.client, listQemuErr: errSettlementRead}
			return "the cluster's guests could not be listed"
		}},
		{"source-not-listed", unfiredDetach, func(_ *testing.T, m *unfiredMove) string {
			m.client.unlisted = map[int]bool{m.source: true}
			return fmt.Sprintf("VM %d is not listed on any node", m.source)
		}},
		{"no-move-task-reader", unfiredDetach, func(_ *testing.T, m *unfiredMove) string {
			m.deps.PVE = noMoveTaskReaderPVE{Client: m.client}
			return "the PVE client cannot list active move tasks"
		}},
		{"source-config-missing", unfiredDetach, func(_ *testing.T, m *unfiredMove) string {
			m.failConfigReadAfterListing(m.source, errConfigMissing)
			return fmt.Sprintf("VM %d no longer exists on node n1", m.source)
		}},
		{"source-config-unreadable", unfiredDetach, func(_ *testing.T, m *unfiredMove) string {
			m.failConfigReadAfterListing(m.source, errSettlementRead)
			return fmt.Sprintf("the config of VM %d could not be read", m.source)
		}},
		{"two-keys-name-volume", unfiredDetach, func(t *testing.T, m *unfiredMove) string {
			m.client.state.configs[m.source][m.freeKey(t, m.source, "unused")] = m.step.Target.IntendedVolume
			return fmt.Sprintf("of VM %d all name volume %s", m.source, m.step.Target.IntendedVolume)
		}},
		{"bus-slot-without-serial", unfiredAttachFromParker, func(t *testing.T, m *unfiredMove) string {
			key := m.sourceKey(t)
			m.client.state.configs[m.source][key] = m.step.Target.IntendedVolume
			return fmt.Sprintf("%s of VM %d names volume %s without the disk's serial", key, m.source, m.step.Target.IntendedVolume)
		}},
		{"transfer-record-gone", unfiredDetach, func(t *testing.T, m *unfiredMove) string {
			(&strandedDisk{client: m.client, token: m.token}).dropRecord(t)
			return fmt.Sprintf("no parker keeps the record of the disk's transfer from VM %d", m.source)
		}},
		{"transfer-records-unreadable", unfiredDetach, func(_ *testing.T, m *unfiredMove) string {
			m.client.state.configs[888] = map[string]any{"name": "other", "digest": "1"}
			m.failConfigReadAfterListing(888, errSettlementRead)
			return "the parkers' records of the disk's transfer could not be read"
		}},
		{"storage-listing-failed", unfiredDetach, func(_ *testing.T, m *unfiredMove) string {
			m.deps.PVE = settlementFaultPVE{lifecycleFlowPVE: m.client, armed: m.armOnTaskListing(), listContentErr: errSettlementRead}
			return "the storage content on node n1 could not be listed"
		}},
		{"volume-not-on-node", unfiredDetach, func(_ *testing.T, m *unfiredMove) string {
			m.deps.PVE = settlementFaultPVE{lifecycleFlowPVE: m.client, armed: m.armOnTaskListing(), contentErr: errVolumeNotFound}
			return fmt.Sprintf("volume %s is not on node n1", m.step.Target.IntendedVolume)
		}},
		{"volume-unreadable", unfiredDetach, func(_ *testing.T, m *unfiredMove) string {
			m.deps.PVE = settlementFaultPVE{lifecycleFlowPVE: m.client, armed: m.armOnTaskListing(), contentErr: errVolumeRead}
			return fmt.Sprintf("volume %s could not be read on node n1", m.step.Target.IntendedVolume)
		}},
		{"malformed-content", unfiredDetach, func(_ *testing.T, m *unfiredMove) string {
			m.deps.PVE = settlementFaultPVE{lifecycleFlowPVE: m.client, armed: m.armOnTaskListing(), content: &nodes.GetStorageContentResponse{Format: "raw"}}
			return "PVE returned malformed content for volume " + m.step.Target.IntendedVolume
		}},
		{"holder-scan-failed", unfiredAttachFromParker, func(_ *testing.T, m *unfiredMove) string {
			m.client.state.configs[888] = map[string]any{"name": "other", "digest": "1"}
			m.failConfigReadAfterListing(888, errSettlementRead)
			return "the guests that hold the disk could not be read"
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			m := row.shape(t, false)
			want := row.setup(t, m)
			m.requireRefusedBy(t, func() error { return m.finalizeCleanup(settleAt(pastQuietPeriod)) }, want)
			m.requireNothingDeleted(t)
		})
	}
}

// TestMoveSettlementRefusedSaveLeavesRecord makes the journal refuse the
// write that settles the move step. The decision fails naming the step, and
// the record reads back unchanged.
func TestMoveSettlementRefusedSaveLeavesRecord(t *testing.T) {
	m := unfiredDetach(t, false)
	fired := false
	ctx := aj.WithSaveFaultForTest(settleAt(pastQuietPeriod), func(next aj.Record) error {
		for i := range next.Steps {
			if next.Steps[i].ID == m.step.ID && next.Steps[i].State == aj.Observed {
				fired = true
				return errors.New("no space left on device")
			}
		}
		return nil
	})
	err := m.finalizeCleanup(ctx)
	if !fired {
		t.Fatalf("the settlement wrote nothing: %v", err)
	}
	want := "the journal refused to save the move settlement of step " + m.step.ID + " (" + m.step.Kind + "): "
	if got := StorageAllocationDecisionFailure(err); !strings.Contains(got, want) {
		t.Fatalf("CLI text = %q, want it to contain %q", got, want)
	}
	m.requireRecordUnchanged(t)
	m.requireNothingDeleted(t)
}
