// parker_provenance_keep_internal_test.go holds white-box tests for the source
// keep rule: a stale transfer record survives collection while its source VM
// still names the record's volume, because after a failed move the record is
// the only link from the disk's stable ID to a volume that sits on an unused
// entry with no serial.
package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

const (
	keepParkerVMID = 90000
	keepSourceVMID = 777
	keepStranded   = "a:777/vm-777-disk-2.raw"
	keepKey        = "bpd-0011223344556677"
)

var keepNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// keepWorld is a two-node cluster: guest configs per node, forced read errors,
// the /cluster/resources index, and a log of every config read, so a test can
// prove which source reads ran.
type keepWorld struct {
	mu       sync.Mutex
	configs  map[string]map[int]map[string]any
	errs     map[string]error
	index    []map[string]any
	nodesErr error
	reads    []string
	written  string
}

func newKeepWorld() *keepWorld {
	return &keepWorld{configs: map[string]map[int]map[string]any{"n1": {}, "n2": {}}, errs: map[string]error{}}
}

func (w *keepWorld) client() Client {
	return &findVMTestClient{
		backendTestClient: backendTestClient{
			clusterSvc: &fakeCluster{
				listFn: func(context.Context, *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
					return clusterResp(w.index...), nil
				},
				configNodesFn: func(ctx context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
					if w.nodesErr != nil {
						return nil, w.nodesErr
					}
					return findVMConfigNodes("n1", "n2")(ctx)
				},
			},
			nodesSvc: &fakeNodesService{
				updateQemuConfigFn: func(_ context.Context, node, vmid string, params *sdknodes.UpdateQemuConfigParams) error {
					w.mu.Lock()
					defer w.mu.Unlock()
					if params != nil && params.Description != nil {
						w.written = *params.Description
						w.configs[node][keepParkerVMID]["description"] = *params.Description
					}
					return nil
				},
			},
		},
		qemuSvc: &fakeQEMUService{
			configFn: func(_ context.Context, node string, vmid int) (map[string]any, error) {
				w.mu.Lock()
				defer w.mu.Unlock()
				w.reads = append(w.reads, fmt.Sprintf("%s/%d", node, vmid))
				if err := w.errs[fmt.Sprintf("%s/%d", node, vmid)]; err != nil {
					return nil, err
				}
				cfg, ok := w.configs[node][vmid]
				if !ok {
					return nil, &sdkerrors.APIError{HTTPCode: 404, Message: "not found"}
				}
				out := make(map[string]any, len(cfg))
				for k, v := range cfg {
					out[k] = v
				}
				return out, nil
			},
		},
	}
}

// sourceReads returns the config reads that were not of the parker.
func (w *keepWorld) sourceReads() []string {
	var out []string
	for _, r := range w.reads {
		if r != fmt.Sprintf("n1/%d", keepParkerVMID) {
			out = append(out, r)
		}
	}
	return out
}

// strandedIntent is the record a detach-side transfer leaves when its move
// fails: the pre-move volid, the source VM, and a parked_at of age.
func strandedIntent(age time.Duration) parkerProvEntry {
	return parkerProvEntry{
		DiskCID:     "pvd-stranded",
		SourceVMCID: fmt.Sprint(keepSourceVMID),
		ParkedAt:    keepNow.Add(-age).Format(time.RFC3339),
		Node:        "n1",
		Volid:       keepStranded,
		Slot:        "scsi0",
	}
}

// withParker installs parker 90000 on n1 holding the given records.
func (w *keepWorld) withParker(t *testing.T, disks map[string]parkerProvEntry) {
	t.Helper()
	w.configs["n1"][keepParkerVMID] = map[string]any{"tags": ParkerTag, "description": provSentinel(t, disks)}
}

// writeFresh runs one provenance write of an unrelated disk onto the parker,
// which is the write that collects.
func (w *keepWorld) writeFresh(t *testing.T) error {
	t.Helper()
	fresh := parkerProvEntry{DiskCID: "pvd-fresh", ParkedAt: keepNow.Format(time.RFC3339), Node: "n1", Volid: "a:90000/vm-90000-disk-5.raw", Slot: "scsi5"}
	return writeParkerProvenance(context.Background(), w.client(), nil, "n1", keepParkerVMID, "bpd-fresh", fresh, provTestClock(keepNow))
}

func (w *keepWorld) survived(t *testing.T, key string) bool {
	t.Helper()
	_, ok := provRecords(t, w.written)[key]
	return ok
}

// TestKeepRule_SourceStillNamesTheVolume covers U1 and U2. Before the keep
// rule both fail: the record is an hour old and nothing on the parker names
// its volume, so the collector removed it.
func TestKeepRule_SourceStillNamesTheVolume(t *testing.T) {
	t.Parallel()
	for name, source := range map[string]map[string]any{
		"U1 active slot": {"scsi1": keepStranded + ",serial=" + keepKey + ",size=5G"},
		"U2 unused slot": {"unused0": keepStranded},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newKeepWorld()
			w.withParker(t, map[string]parkerProvEntry{keepKey: strandedIntent(2 * time.Hour)})
			w.configs["n1"][keepSourceVMID] = source
			if err := w.writeFresh(t); err != nil {
				t.Fatal(err)
			}
			if !w.survived(t, keepKey) {
				t.Fatalf("the transfer record was collected although VM %d still names %s", keepSourceVMID, keepStranded)
			}
		})
	}
}

// TestKeepRule_CollectsWhenTheSourceNoLongerHoldsTheVolume covers U3 and U4.
// The record goes, and it goes because the rule read the source and found no
// hold, not by default. Before the keep rule both fail on that second point:
// the collector removed the record without reading anything.
func TestKeepRule_CollectsWhenTheSourceNoLongerHoldsTheVolume(t *testing.T) {
	t.Parallel()
	t.Run("U3 source config names something else", func(t *testing.T) {
		t.Parallel()
		w := newKeepWorld()
		w.withParker(t, map[string]parkerProvEntry{keepKey: strandedIntent(2 * time.Hour)})
		w.configs["n1"][keepSourceVMID] = map[string]any{"unused0": "a:777/vm-777-disk-9.raw", "scsi0": "a:777/vm-777-disk-0.raw"}
		if err := w.writeFresh(t); err != nil {
			t.Fatal(err)
		}
		if w.survived(t, keepKey) {
			t.Fatal("a record whose source no longer names its volume must be collected")
		}
		if reads := w.sourceReads(); len(reads) != 1 || reads[0] != fmt.Sprintf("n1/%d", keepSourceVMID) {
			t.Fatalf("source reads = %v, want the one read that found no hold", reads)
		}
	})
	t.Run("U4 source proven absent cluster-wide", func(t *testing.T) {
		t.Parallel()
		w := newKeepWorld()
		w.withParker(t, map[string]parkerProvEntry{keepKey: strandedIntent(2 * time.Hour)})
		if err := w.writeFresh(t); err != nil {
			t.Fatal(err)
		}
		if w.survived(t, keepKey) {
			t.Fatal("a record whose source VM is proven gone must be collected")
		}
		if reads := strings.Join(w.sourceReads(), " "); !strings.Contains(reads, fmt.Sprintf("n2/%d", keepSourceVMID)) {
			t.Fatalf("source reads = %q, want the absence proven on every node before the record went", reads)
		}
	})
}

// TestKeepRule_KeepsWhenTheAnswerIsUnknown covers U5, U6, and U7. Before the
// keep rule all three fail, because the collector read no source at all.
func TestKeepRule_KeepsWhenTheAnswerIsUnknown(t *testing.T) {
	t.Parallel()
	t.Run("U5 source read fails", func(t *testing.T) {
		t.Parallel()
		w := newKeepWorld()
		w.withParker(t, map[string]parkerProvEntry{keepKey: strandedIntent(2 * time.Hour)})
		w.configs["n1"][keepSourceVMID] = map[string]any{"unused0": keepStranded}
		w.errs[fmt.Sprintf("n1/%d", keepSourceVMID)] = &sdkerrors.APIError{HTTPCode: 500, Message: "got timeout"}
		if err := w.writeFresh(t); err != nil {
			t.Fatal(err)
		}
		if !w.survived(t, keepKey) {
			t.Fatal("a source read that failed must keep the record")
		}
	})
	t.Run("U6 source moved to another node", func(t *testing.T) {
		t.Parallel()
		w := newKeepWorld()
		w.withParker(t, map[string]parkerProvEntry{keepKey: strandedIntent(2 * time.Hour)})
		w.configs["n2"][keepSourceVMID] = map[string]any{"unused0": keepStranded}
		w.index = []map[string]any{{"vmid": keepSourceVMID, "node": "n2", "type": "qemu"}}
		if err := w.writeFresh(t); err != nil {
			t.Fatal(err)
		}
		if !w.survived(t, keepKey) {
			t.Fatalf("a source gone from the recorded node but holding the volume on n2 must keep the record; reads=%v", w.reads)
		}
		if !strings.Contains(strings.Join(w.reads, " "), fmt.Sprintf("n2/%d", keepSourceVMID)) {
			t.Fatalf("the source was never read on the node it moved to; reads=%v", w.reads)
		}
	})
	t.Run("U7 absence cannot be proven", func(t *testing.T) {
		t.Parallel()
		w := newKeepWorld()
		w.withParker(t, map[string]parkerProvEntry{keepKey: strandedIntent(2 * time.Hour)})
		w.nodesErr = errors.New("corosync membership unavailable")
		if err := writeParkerProvenance(fastBackoffCtx(), w.client(), nil, "n1", keepParkerVMID, "bpd-fresh",
			parkerProvEntry{DiskCID: "pvd-fresh", ParkedAt: keepNow.Format(time.RFC3339), Node: "n1"}, provTestClock(keepNow)); err != nil {
			t.Fatal(err)
		}
		if !w.survived(t, keepKey) {
			t.Fatal("a source whose absence FindVMAuthoritative could not prove must keep the record")
		}
	})
}

// TestKeepRule_ReadsOnlyForSourceBearingCandidates covers U8 and U9. Records
// the keep rule does not concern cost no source read, and a record with no
// source VM keeps the age-and-reference rule alone, while a stranded intent
// in the same store is read and kept. Before the keep rule both fail, because
// the stranded intent was collected with the rest.
func TestKeepRule_ReadsOnlyForSourceBearingCandidates(t *testing.T) {
	t.Parallel()
	t.Run("U8 record without source_vm_cid", func(t *testing.T) {
		t.Parallel()
		w := newKeepWorld()
		noSource := strandedIntent(2 * time.Hour)
		noSource.SourceVMCID = ""
		noSource.Volid = "a:778/vm-778-disk-0.raw"
		w.withParker(t, map[string]parkerProvEntry{"bpd-nosource": noSource, keepKey: strandedIntent(2 * time.Hour)})
		w.configs["n1"][keepSourceVMID] = map[string]any{"unused0": keepStranded}
		if err := w.writeFresh(t); err != nil {
			t.Fatal(err)
		}
		if w.survived(t, "bpd-nosource") {
			t.Fatal("a stale record with no source VM must be collected by the age rule")
		}
		if !w.survived(t, keepKey) {
			t.Fatal("the stranded intent beside it must be kept")
		}
		if reads := w.sourceReads(); len(reads) != 1 || reads[0] != fmt.Sprintf("n1/%d", keepSourceVMID) {
			t.Fatalf("source reads = %v, want only the stranded intent's source", reads)
		}
	})
	t.Run("U9 young record and the key being written", func(t *testing.T) {
		t.Parallel()
		w := newKeepWorld()
		young := strandedIntent(10 * time.Minute)
		young.SourceVMCID = "778"
		rewritten := strandedIntent(2 * time.Hour)
		rewritten.SourceVMCID = "779"
		w.withParker(t, map[string]parkerProvEntry{
			"bpd-young":     young,
			"bpd-rewritten": rewritten,
			keepKey:         strandedIntent(2 * time.Hour),
		})
		w.configs["n1"][keepSourceVMID] = map[string]any{"unused0": keepStranded}
		if err := writeParkerProvenance(context.Background(), w.client(), nil, "n1", keepParkerVMID, "bpd-rewritten", rewritten, provTestClock(keepNow)); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"bpd-young", "bpd-rewritten", keepKey} {
			if !w.survived(t, key) {
				t.Fatalf("%s must survive: a young record, the record being written, and a kept stranded intent", key)
			}
		}
		if reads := w.sourceReads(); len(reads) != 1 || reads[0] != fmt.Sprintf("n1/%d", keepSourceVMID) {
			t.Fatalf("source reads = %v, want only the stranded intent's source, never 778 or 779", reads)
		}
	})
}

// TestKeepRule_CapacityProbeAndWriteAgree covers U10. A kept record that pushes
// the store past the budget must make both the capacity probe and the write
// refuse. Before the keep rule both collected the record and found room.
func TestKeepRule_CapacityProbeAndWriteAgree(t *testing.T) {
	t.Parallel()
	w := newKeepWorld()
	w.configs["n1"][keepSourceVMID] = map[string]any{"unused0": keepStranded}

	stranded := strandedIntent(2 * time.Hour)
	stranded.DiskCID = "pvd-" + strings.Repeat("S", 700)
	pctx := ParkContext{DiskCID: "pvd-" + strings.Repeat("N", 120), SourceVMCID: "701", StableID: "bpd-ffeeddccbbaa9988"}
	cfg := provTestClock(keepNow)
	widest := fmt.Sprintf("scsi%d", parkerMaxSlots-1)
	newEntry := buildParkerProvEntry(context.Background(), "n1", "a:701/vm-701-disk-0.raw", widest, cfg, pctx)

	// Fill the parker with live records until the store fits the new record
	// without the stranded intent and does not fit it with the intent.
	parker := map[string]any{"tags": ParkerTag}
	disks := map[string]parkerProvEntry{}
	size := func(withStranded bool) int {
		all := map[string]parkerProvEntry{pctx.StableID: newEntry}
		for k, v := range disks {
			all[k] = v
		}
		if withStranded {
			all[keepKey] = stranded
		}
		return len(provSentinel(t, all))
	}
	for i := 0; size(true) <= parkerDescriptionBudget; i++ {
		volid := fmt.Sprintf("a:90000/vm-90000-disk-%d.raw", i)
		parker[fmt.Sprintf("scsi%d", i)] = volid + ",size=1G"
		disks[fmt.Sprintf("bpd-%016x", i)] = parkerProvEntry{DiskCID: "pvd-" + strings.Repeat("L", 40), ParkedAt: keepNow.Format(time.RFC3339), Node: "n1", Volid: volid, Slot: fmt.Sprintf("scsi%d", i)}
	}
	if size(false) > parkerDescriptionBudget {
		t.Fatalf("fixture: the store does not fit the new record even without the stranded intent (%d bytes)", size(false))
	}
	disks[keepKey] = stranded
	parker["description"] = provSentinel(t, disks)
	w.configs["n1"][keepParkerVMID] = parker

	roomErr := parkerProvenanceRoom(context.Background(), w.client(), "n1", keepParkerVMID, "a:701/vm-701-disk-0.raw", cfg, pctx)
	writeErr := writeParkerProvenance(context.Background(), w.client(), nil, "n1", keepParkerVMID, pctx.StableID, newEntry, cfg)
	if !errors.Is(roomErr, ErrProvenanceFull) || !errors.Is(writeErr, ErrProvenanceFull) {
		t.Fatalf("probe=%v write=%v; both must refuse, because the stranded intent is kept and fills the store", roomErr, writeErr)
	}
	desc, _ := w.configs["n1"][keepParkerVMID]["description"].(string)
	if _, ok := provRecords(t, desc)[keepKey]; !ok {
		t.Fatal("the stranded intent left the store")
	}
}

// TestKeepRule_FullOfKeptIntentsRoutesElsewhere covers U11. A parker whose
// store is full of kept intents still has free slots, and an unrelated park
// and an unrelated transfer must each land on another parker without touching
// those intents. Before the keep rule both landed on the full parker, after
// collecting every intent there.
func TestKeepRule_FullOfKeptIntentsRoutesElsewhere(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"ParkDisk", "TransferDiskToParker"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			now := keepNow
			full := map[string]any{cfgKeyTags: "bosh-cpi;bosh-parker", paramProtection: true}
			configs := map[int]map[string]any{
				90000: full,
				90001: {cfgKeyTags: "bosh-cpi;bosh-parker", paramProtection: true},
			}
			if path == "TransferDiskToParker" {
				configs[700] = map[string]any{"scsi1": "data:vm-700-disk-1,serial=" + transferStableID + ",size=10G"}
			}
			intents := map[string]parkerProvEntry{}
			for i := 0; len(provSentinel(t, intents)) <= parkerDescriptionBudget; i++ {
				vmid := 800 + i
				volid := fmt.Sprintf("data:vm-%d-disk-0", vmid)
				configs[vmid] = map[string]any{"unused0": volid}
				intents[fmt.Sprintf("bpd-%016x", vmid)] = parkerProvEntry{
					DiskCID: "pvd-" + strings.Repeat("K", 300), SourceVMCID: fmt.Sprint(vmid),
					ParkedAt: now.Add(-3 * time.Hour).Format(time.RFC3339), Node: "pve1", Volid: volid, Slot: "scsi0",
				}
			}
			full["description"] = provSentinel(t, intents)
			c := newScanFakeClient(configs)
			cfg := transferTestCfg
			cfg.NowFunc = func() time.Time { return now }
			pctx := ParkContext{DiskCID: "pvd-test", SourceVMCID: "700", StableID: transferStableID}

			var err error
			if path == "ParkDisk" {
				err = ParkDisk(context.Background(), c, nil, "pve1", "data:vm-700-disk-1", cfg, pctx)
			} else {
				_, err = TransferDiskToParker(context.Background(), c, nil, "pve1", 700, "data:vm-700-disk-1", cfg, pctx)
			}
			if err != nil {
				t.Fatalf("%s failed although parker 90001 has room: %v", path, err)
			}
			if _, ok := parseSentinelDisks(t, c, 90001)[transferStableID]; !ok {
				t.Fatalf("%s did not land on parker 90001", path)
			}
			kept := parseSentinelDisks(t, c, 90000)
			for key := range intents {
				if _, ok := kept[key]; !ok {
					t.Fatalf("%s collected the kept intent %s on parker 90000", path, key)
				}
			}
		})
	}
}

// TestWithProvenanceClock stamps parked_at from the context clock and lets
// ParkerConfig.NowFunc keep precedence.
func TestWithProvenanceClock(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ctx := WithProvenanceClock(context.Background(), func() time.Time { return at })
	entry := buildParkerProvEntry(ctx, "n1", "a:1/vm-1-disk-0.raw", "scsi0", ParkerConfig{}, ParkContext{})
	if entry.ParkedAt != at.Format(time.RFC3339) {
		t.Fatalf("parked_at = %q, want the context clock %s", entry.ParkedAt, at.Format(time.RFC3339))
	}
	pinned := provTestClock(keepNow)
	if got := buildParkerProvEntry(ctx, "n1", "a:1/vm-1-disk-0.raw", "scsi0", pinned, ParkContext{}).ParkedAt; got != keepNow.Format(time.RFC3339) {
		t.Fatalf("parked_at = %q, want NowFunc to win over the context clock", got)
	}
	raw, _ := json.Marshal(entry)
	if !strings.Contains(string(raw), `"parked_at":"2026-01-02T03:04:05Z"`) {
		t.Fatalf("record = %s", raw)
	}
}
