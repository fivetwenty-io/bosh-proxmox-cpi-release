package handlers

// A transfer to a parker can stop after its move renamed the volume and
// before the parker's slot got the disk's serial. The transfer record still
// names the volume by its name before the move, and that name is gone. These
// rows check that the managed identity check and attach_disk's node lookup
// finish the transfer before they judge the disk, and that update_disk's own
// resume runs inside the disk's lifecycle.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	nodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// missingVolumeRefusal opens the identity check's permanent refusal of a disk
// whose recorded volume is gone, which asks for an audit.
const missingVolumeRefusal = "managed disk ownership provenance references a missing volume; audit required"

// landedBeforeSerial parks the managed disk off 777 with move_disk, which
// renames it for the parker, and then puts the parker back in the state a
// crash leaves between the move and the serial write. The landed slot loses
// the disk's serial, the parker's record names the volume by its name before
// the move, and 777 carries the allocation entry again because the detach
// never reached its tail. It returns the parker, the disk's token, and the
// landed volid.
func landedBeforeSerial(t *testing.T, f digestManaged) (int, string, string) {
	t.Helper()
	_, key, entry := tailEntry(t, f)
	parkRenamed(t, f, key, entry)
	landed := renamedLanding(t, f)
	parker, token := 0, ""
	for vmid, cfg := range f.client.state.configs {
		for slot, value := range cfg {
			text, ok := value.(string)
			if !ok || !strings.HasPrefix(text, landed+",") {
				continue
			}
			parts := strings.Split(text, ",")
			kept := parts[:0]
			for _, part := range parts {
				if serial, found := strings.CutPrefix(part, "serial="); found {
					token = serial
					continue
				}
				kept = append(kept, part)
			}
			cfg[slot] = strings.Join(kept, ",")
			parker = vmid
		}
	}
	if parker == 0 || token == "" {
		t.Fatalf("no slot carries %s with a serial", landed)
	}
	cfg := f.client.state.configs[parker]
	nonBOSH, raw := pve.ParseSentinel(pve.DescriptionFromConfig(cfg))
	var disks map[string]map[string]any
	if err := json.Unmarshal(raw["bosh_parked_disks"], &disks); err != nil {
		t.Fatal(err)
	}
	if disks[token] == nil {
		t.Fatalf("parker %d keeps no record for %s: %v", parker, token, disks)
	}
	disks[token]["volid"] = f.volume
	encoded, err := json.Marshal(disks)
	if err != nil {
		t.Fatal(err)
	}
	raw["bosh_parked_disks"] = encoded
	description, err := pve.RenderSentinel(nonBOSH, raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg["description"] = description
	f.client.moveCalls = 0
	return parker, token, landed
}

// serialSlots lists the slots, as vmid.slot, whose value carries token as its
// serial.
func serialSlots(configs map[int]map[string]any, token string) []string {
	var out []string
	for vmid, cfg := range configs {
		for slot, value := range cfg {
			if text, ok := value.(string); ok && strings.Contains(text, ",serial="+token) {
				out = append(out, strconv.Itoa(vmid)+"."+slot)
			}
		}
	}
	return out
}

// TestManagedIdentityResumesLandedTransfer calls each handler with the CID of
// a journal-managed disk whose transfer stopped between its move and its
// serial write. The identity check used to refuse it for audit because the
// record names a volume that's gone, and every retry got the same refusal.
// Now the check finishes the transfer first, so each handler acts on the disk
// where it landed.
func TestManagedIdentityResumesLandedTransfer(t *testing.T) {
	captureParkerPoolSweep(t)
	for _, tc := range []struct {
		name  string
		call  func(t *testing.T, f digestManaged) error
		after func(t *testing.T, f digestManaged, parker int, token, landed string)
	}{
		{"attach_disk", func(t *testing.T, f digestManaged) error {
			return digestAttach(t, f.deps, f.cid)
		}, func(t *testing.T, f digestManaged, _ int, token, _ string) {
			if slots := serialSlots(f.client.state.configs, token); len(slots) != 1 || !strings.HasPrefix(slots[0], "777.") {
				t.Fatalf("the disk's serial sits on %v, want one slot on 777", slots)
			}
		}},
		{"detach_disk", func(t *testing.T, f digestManaged) error {
			return digestDetach(t, f.deps, f.cid)
		}, func(t *testing.T, f digestManaged, parker int, token, _ string) {
			if slots := serialSlots(f.client.state.configs, token); len(slots) != 1 || !strings.HasPrefix(slots[0], strconv.Itoa(parker)+".") {
				t.Fatalf("the disk's serial sits on %v, want one slot on parker %d", slots, parker)
			}
		}},
		{"delete_disk", func(t *testing.T, f digestManaged) error {
			_, err := HandleDeleteDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
			return err
		}, func(t *testing.T, f digestManaged, _ int, _, landed string) {
			if f.client.state.volumes[landed] != nil {
				t.Fatalf("%s survived delete_disk", landed)
			}
			record, err := f.journal.Inspect(f.id)
			if err != nil {
				t.Fatal(err)
			}
			if record.State != aj.Deleted {
				t.Fatalf("record state = %s (%s), want deleted", record.State, record.Reason)
			}
		}},
		{"has_disk", func(t *testing.T, f digestManaged) error {
			result, err := HandleHasDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
			if err == nil && result != true {
				t.Fatalf("has_disk = %v, want true for the landed disk", result)
			}
			return err
		}, func(t *testing.T, f digestManaged, parker int, token, _ string) {
			if slots := serialSlots(f.client.state.configs, token); len(slots) != 1 || !strings.HasPrefix(slots[0], strconv.Itoa(parker)+".") {
				t.Fatalf("the disk's serial sits on %v, want one slot on parker %d", slots, parker)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := digestManagedFixture(t)
			parker, token, landed := landedBeforeSerial(t, f)
			if err := tc.call(t, f); err != nil {
				t.Fatalf("%s of the disk that landed before its serial: %v", tc.name, err)
			}
			tc.after(t, f, parker, token, landed)
		})
	}
}

// TestManagedIdentityRefusesConflictingTransferRecord is the same landed
// transfer with a record that names another allocation. The check refuses it
// for audit before it resumes anything, so the parker's slot stays without a
// serial.
func TestManagedIdentityRefusesConflictingTransferRecord(t *testing.T) {
	captureParkerPoolSweep(t)
	f := digestManagedFixture(t)
	parker, token, _ := landedBeforeSerial(t, f)
	cfg := f.client.state.configs[parker]
	nonBOSH, raw := pve.ParseSentinel(pve.DescriptionFromConfig(cfg))
	var disks map[string]map[string]any
	if err := json.Unmarshal(raw["bosh_parked_disks"], &disks); err != nil {
		t.Fatal(err)
	}
	if _, ok := disks[token]["allocation_id"]; !ok {
		t.Fatalf("parker %d's record for %s carries no allocation_id: %v", parker, token, disks[token])
	}
	disks[token]["allocation_id"] = "00000000-0000-4000-8000-000000000000"
	encoded, err := json.Marshal(disks)
	if err != nil {
		t.Fatal(err)
	}
	raw["bosh_parked_disks"] = encoded
	description, err := pve.RenderSentinel(nonBOSH, raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg["description"] = description
	err = digestDetach(t, f.deps, f.cid)
	requirePermanent(t, err, "detach_disk with a conflicting transfer record")
	requireText(t, err, "detach_disk with a conflicting transfer record", []string{"transfer provenance conflicts", "audit required"})
	if slots := serialSlots(f.client.state.configs, token); len(slots) != 0 {
		t.Fatalf("the refused disk's serial was written to %v", slots)
	}
}

// legacyLandedBeforeSerial parks a legacy stable-ID disk on node-local storage
// off 777 with move_disk, which renames it for the parker, and then puts the
// parker back in the state a crash leaves between the move and the serial
// write, the way buildBirthNameUnfinishedTransfer does. Nothing takes the
// birth name afterwards, so the volume the record names is simply gone, and a
// node lookup on node-local storage can't find it.
func legacyLandedBeforeSerial(t *testing.T) (*strandedDisk, string) {
	t.Helper()
	s := legacyBirthNameFixture(t)
	birth := "a:777/vm-777-disk-0.raw"
	s.client.localStorage = true
	s.client.volumeNodes = map[string]string{birth: "n1"}
	s.deps.Resolver = pve.NewBackendResolver(s.deps.PVE, nil, "n1")
	s.mintStableDisk(t, birth)
	s.client.state.volumes[birth] = &nodes.GetStorageContentResponse{Size: 5 << 30, Format: "raw"}
	ctx := context.Background()
	if err := attachDiskAt(t, ctx, s.deps, "777", s.cid); err != nil {
		t.Fatalf("attach_disk(777): %v", err)
	}
	if err := detachDiskAt(t, ctx, s.deps, "777", s.cid); err != nil {
		t.Fatalf("detach_disk(777): %v", err)
	}
	parker := s.requireSingleHolder(t, true)
	cfg := s.client.state.configs[parker]
	for slot := range s.serialHolders() {
		key := strings.TrimPrefix(slot, strconv.Itoa(parker)+".")
		cfg[key] = strings.Replace(cfg[key].(string), ",serial="+s.token, "", 1)
	}
	nonBOSH, raw := pve.ParseSentinel(pve.DescriptionFromConfig(cfg))
	var disks map[string]map[string]any
	if err := json.Unmarshal(raw["bosh_parked_disks"], &disks); err != nil {
		t.Fatal(err)
	}
	if disks[s.token] == nil {
		t.Fatalf("the park left no record for the disk: %v", disks)
	}
	disks[s.token]["volid"] = birth
	encoded, err := json.Marshal(disks)
	if err != nil {
		t.Fatal(err)
	}
	raw["bosh_parked_disks"] = encoded
	description, err := pve.RenderSentinel(nonBOSH, raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg["description"] = description
	if len(s.serialHolders()) != 0 || s.intentParker() != parker {
		t.Fatalf("the crash state still has serial holders %v or lost the record", s.serialHolders())
	}
	if s.client.state.volumes[birth] != nil {
		t.Fatalf("%s kept its name, want the move to rename it", birth)
	}
	return s, birth
}

// TestAttachDiskResumesBeforeNodeLookup calls attach_disk for a legacy
// stable-ID disk on node-local storage whose transfer stopped between its
// move and its serial write. The node lookup used to look for the record's
// volume name, find nothing, and report the disk missing for good. Now attach_disk finishes the
// transfer first, so the lookup finds the landed volume and the disk lands on
// 777.
func TestAttachDiskResumesBeforeNodeLookup(t *testing.T) {
	s, birth := legacyLandedBeforeSerial(t)
	if err := attachDiskAt(t, context.Background(), s.deps, "777", s.cid); err != nil {
		t.Fatalf("attach_disk of the disk that landed before its serial: %v", err)
	}
	if vmid := s.requireSingleHolder(t, false); vmid != 777 {
		t.Fatalf("the disk landed on VM %d, want 777", vmid)
	}
	if s.intentParker() != 0 {
		t.Fatal("the transfer record survived the attach")
	}
	if refs := s.references(birth); len(refs) != 0 {
		t.Fatalf("%s is referenced by %v, want the old name gone", birth, refs)
	}
}

// resumeFails swaps the resume for one that returns err, and counts its calls.
// The swap is process-wide, so its rows don't run in parallel.
func resumeFails(t *testing.T, err error) *int {
	t.Helper()
	calls := 0
	t.Cleanup(setResumeDiskTransferToParkerForTest(func(
		context.Context, pve.Client, *log.Logger, pve.DiskTransferIntent, string, pve.ParkerConfig, pve.ParkContext,
	) (string, error) {
		calls++
		return "", err
	}))
	return &calls
}

// TestUnfinishedTransferResumeErrorClass covers a resume that fails in each
// place that now runs it first. A resume that can't run, such as one whose
// parker node is offline, comes back retriable, so the Director tries again
// once the node is back. A resume that refuses permanently keeps its class and
// its text, so an audit refusal is never relabelled retriable.
func TestUnfinishedTransferResumeErrorClass(t *testing.T) {
	captureParkerPoolSweep(t)
	offline := errors.New("parker node n1 is offline")
	refused := cpierrors.Cloud("the parker's record conflicts with the landed slot; audit required")
	for _, tc := range []struct {
		name      string
		build     func(t *testing.T) func() error
		resumeErr error
		permanent bool
	}{
		{"managed identity check, resume can't run", managedIdentityCall, offline, false},
		{"managed identity check, resume refuses", managedIdentityCall, refused, true},
		{"attach_disk node lookup, resume can't run", legacyAttachCall, offline, false},
		{"attach_disk node lookup, resume refuses", legacyAttachCall, refused, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := tc.build(t)
			calls := resumeFails(t, tc.resumeErr)
			err := call()
			if *calls == 0 {
				t.Fatalf("the resume never ran (err = %v)", err)
			}
			if tc.permanent {
				requirePermanent(t, err, tc.name)
				requireText(t, err, tc.name, []string{"conflicts with the landed slot", "audit required"})
				return
			}
			requireRetriable(t, err, tc.name)
			requireText(t, err, tc.name, []string{"parker node n1 is offline"})
		})
	}
}

// managedIdentityCall builds the managed landed transfer and returns a
// detach_disk call for it, which reaches the identity check first.
func managedIdentityCall(t *testing.T) func() error {
	f := digestManagedFixture(t)
	landedBeforeSerial(t, f)
	return func() error { return digestDetach(t, f.deps, f.cid) }
}

// legacyAttachCall builds the legacy landed transfer and returns an
// attach_disk call for it, which reaches the node lookup first.
func legacyAttachCall(t *testing.T) func() error {
	s, _ := legacyLandedBeforeSerial(t)
	return func() error { return attachDiskAt(t, context.Background(), s.deps, "777", s.cid) }
}

// TestUpdateDiskJournalsItsResume covers a journal-managed disk whose park a
// snapshot deferred, so its transfer to a parker is still unfinished. update_disk
// resumes the transfer inside the disk's lifecycle, so the landing leaves its
// steps in the record and the record goes back ready to return. update_disk
// still leaves the source VM's tail to the calls that move or delete the disk,
// so 777 keeps its entry.
func TestUpdateDiskJournalsItsResume(t *testing.T) {
	captureParkerPoolSweep(t)
	s, id, w := buildTailDisk(t)
	s.deferBySnapshot(t, id)
	parker := s.intentParker()
	if parker == 0 {
		t.Fatal("the deferred park left no transfer record")
	}
	before, err := s.journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := HandleUpdateDisk(s.deps).Handle(context.Background(), overlayArgs(t, s.cid, map[string]any{"cache": "writeback"}), jsonrpc.Context{}); err != nil {
		t.Fatalf("update_disk: %v", err)
	}
	s.requireParkedUnderItsOwnName(t)
	after, err := s.journal.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	landing := 0
	for _, step := range after.Steps[len(before.Steps):] {
		if step.State != aj.Observed {
			t.Fatalf("update_disk left step %s (%s) %s, want every step observed", step.ID, step.Kind, step.State)
		}
		if step.Target.VMID == parker {
			landing++
		}
	}
	if landing == 0 {
		t.Fatalf("update_disk's resume left no step on parker %d in the record", parker)
	}
	s.requireRecord(t, id, aj.ReadyToReturn)
	if !s.hasEntry(777) || w.removals != 0 {
		t.Fatalf("update_disk ran the tail: 777 entry=%v removals=%d", s.hasEntry(777), w.removals)
	}
}

// resumeThen swaps the resume for the real one, and runs before ahead of
// each call so a row can change PVE between the identity check and the
// resume's reads. The swap is process-wide, so its rows don't run in
// parallel.
func resumeThen(t *testing.T, before func()) {
	t.Helper()
	t.Cleanup(setResumeDiskTransferToParkerForTest(func(
		ctx context.Context, c pve.Client, logger *log.Logger, intent pve.DiskTransferIntent, stableID string, cfg pve.ParkerConfig, pctx pve.ParkContext,
	) (string, error) {
		before()
		return pve.ResumeDiskTransferToParker(ctx, c, logger, intent, stableID, cfg, pctx)
	}))
}

// requireTransferUnfinished fails unless the landed slot still has no serial
// and the parker's record still names the volume by its name before the move,
// so nothing the identity check's resume could write was written.
func requireTransferUnfinished(t *testing.T, f digestManaged, parker int, token string) {
	t.Helper()
	if slots := serialSlots(f.client.state.configs, token); len(slots) != 0 {
		t.Fatalf("the disk's serial was written to %v", slots)
	}
	_, raw := pve.ParseSentinel(pve.DescriptionFromConfig(f.client.state.configs[parker]))
	var disks map[string]map[string]any
	if err := json.Unmarshal(raw["bosh_parked_disks"], &disks); err != nil {
		t.Fatal(err)
	}
	if volid, _ := disks[token]["volid"].(string); volid != f.volume {
		t.Fatalf("parker %d's record for %s names %q, want the unfinished record naming %s", parker, token, volid, f.volume)
	}
}

// resumeRun is one call through the resume seam, with the mode it ran in
// and the answer it gave.
type resumeRun struct {
	claimOnly bool
	err       error
}

// recordResumes swaps the resume for the real one, runs before ahead of each
// call, and records each call's mode and answer. The swap is process-wide, so
// its rows don't run in parallel.
func recordResumes(t *testing.T, before func(run int)) *[]resumeRun {
	t.Helper()
	runs := &[]resumeRun{}
	t.Cleanup(setResumeDiskTransferToParkerForTest(func(
		ctx context.Context, c pve.Client, logger *log.Logger, intent pve.DiskTransferIntent, stableID string, cfg pve.ParkerConfig, pctx pve.ParkContext,
	) (string, error) {
		before(len(*runs))
		landed, err := pve.ResumeDiskTransferToParker(ctx, c, logger, intent, stableID, cfg, pctx)
		*runs = append(*runs, resumeRun{claimOnly: pctx.ClaimOnly, err: err})
		return landed, err
	}))
	return runs
}

// requireClaimOnlyMove fails unless run is a claim-only resume that stopped
// at the move.
func requireClaimOnlyMove(t *testing.T, run resumeRun) {
	t.Helper()
	refusal, ok := pve.AsClaimOnlyRefusal(run.err)
	if !run.claimOnly || !ok || refusal.Window != pve.ClaimOnlyMove {
		t.Fatalf("the identity check's resume = claim-only %v, %v, want a claim-only refusal at the move", run.claimOnly, run.err)
	}
}

// TestManagedIdentityResumeIsClaimOnly covers a volume that takes the
// record's old name on 777 after the identity check saw the name gone and
// before the resume reads the source. The full resume would move that volume
// onto the parker, unjournaled, from a read such as has_disk. The identity
// check's resume is claim-only, so it stops at the move window, sends no
// move, and writes no serial. The check then resolves the disk once more,
// finds the old name, and has_disk answers from there, leaving the transfer
// for the next call that changes the disk.
func TestManagedIdentityResumeIsClaimOnly(t *testing.T) {
	captureParkerPoolSweep(t)
	f := digestManagedFixture(t)
	parker, token, _ := landedBeforeSerial(t, f)
	runs := recordResumes(t, func(int) {
		f.client.state.configs[777]["unused7"] = f.volume
		f.client.state.volumes[f.volume] = &nodes.GetStorageContentResponse{Size: 5 << 30, Format: "raw"}
	})
	result, err := HandleHasDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
	if len(*runs) != 1 {
		t.Fatalf("the resume ran %d times, want once (err = %v)", len(*runs), err)
	}
	requireClaimOnlyMove(t, (*runs)[0])
	if err != nil || result != true {
		t.Fatalf("has_disk once the old name is back = %v, %v, want true", result, err)
	}
	if f.client.moveCalls != 0 {
		t.Fatalf("has_disk sent %d moves, want none", f.client.moveCalls)
	}
	if f.client.state.configs[777]["unused7"] != f.volume {
		t.Fatalf("777's unused7 = %v, want the volume left where it is", f.client.state.configs[777]["unused7"])
	}
	requireTransferUnfinished(t, f, parker, token)
}

// nodeUnreachable is the answer pveproxy relays when the node a request is
// for doesn't answer.
func nodeUnreachable(t *testing.T) error {
	t.Helper()
	body, err := json.Marshal(map[string]any{"data": nil, "message": "No route to host\n"})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Errorf("API request failed: %w", sdkerrors.ParseAPIError(595, body))
}

// TestManagedIdentityResumeParkerReadFails covers an identity check whose
// resume can't read the parker, the way it can't while the parker's node is
// unreachable. The check comes back retriable and writes nothing, so the
// Director tries again once the node answers.
func TestManagedIdentityResumeParkerReadFails(t *testing.T) {
	captureParkerPoolSweep(t)
	f := digestManagedFixture(t)
	parker, token, _ := landedBeforeSerial(t, f)
	resumeThen(t, func() {
		f.client.onConfigRead = func(vmid int) error {
			if vmid == parker {
				return nodeUnreachable(t)
			}
			return nil
		}
	})
	err := digestDetach(t, f.deps, f.cid)
	f.client.onConfigRead = nil
	requireRetriable(t, err, "detach_disk while the parker can't be read")
	requireText(t, err, "detach_disk while the parker can't be read", []string{"No route to host"}, "audit required")
	if f.client.moveCalls != 0 {
		t.Fatalf("detach_disk sent %d moves, want none", f.client.moveCalls)
	}
	requireTransferUnfinished(t, f, parker, token)
}

// parkerRaceClient is the flow fake with a parker lock that refuses a second
// holder the way PVE refuses a pool that already exists. While counting is
// set, it counts the serial writes and the record writes that reach the
// parker, and it runs beforeSerial as each serial write arrives. It runs
// lockCreated after it creates a lock's pool and lockRefused before it refuses
// one, each with the pool's name.
type parkerRaceClient struct {
	*lifecycleFlowPVE
	t            *testing.T
	parker       int
	token        string
	beforeSerial func()
	lockCreated  func(id string)
	lockRefused  func(id string)

	mu                 sync.Mutex
	started, counting  bool
	serials, finalizes int
	lockRefusals       int
}

type parkerRacePools struct {
	pve.PoolService
	c *parkerRaceClient
}

func (c *parkerRaceClient) Pools() pve.PoolService {
	return parkerRacePools{PoolService: c.lifecycleFlowPVE.Pools(), c: c}
}

func (p parkerRacePools) CreatePool(ctx context.Context, id, comment string) error {
	if !strings.HasPrefix(id, reservedPoolLockPrefix) {
		return p.PoolService.CreatePool(ctx, id, comment)
	}
	if _, held, err := p.GetPoolComment(ctx, id); err != nil || held {
		p.c.mu.Lock()
		p.c.lockRefusals++
		p.c.mu.Unlock()
		if p.c.lockRefused != nil {
			p.c.lockRefused(id)
		}
		return livePoolVerdict(p.c.t, "create pool failed: pool '"+id+"' already exists")
	}
	if err := p.PoolService.CreatePool(ctx, id, comment); err != nil {
		return err
	}
	if p.c.lockCreated != nil {
		p.c.lockCreated(id)
	}
	return nil
}

type parkerRaceQEMU struct {
	qemu.Service
	c *parkerRaceClient
}

func (c *parkerRaceClient) QEMU() qemu.Service {
	return parkerRaceQEMU{Service: c.lifecycleFlowPVE.QEMU(), c: c}
}

func (q parkerRaceQEMU) AttachDisk(ctx context.Context, node string, vmid int, volume, bus string, opts *qemu.AttachOpts) (string, error) {
	q.c.mu.Lock()
	serial := q.c.counting && vmid == q.c.parker && strings.Contains(volume, "serial="+q.c.token)
	if serial {
		q.c.serials++
	}
	q.c.mu.Unlock()
	if serial && q.c.beforeSerial != nil {
		q.c.beforeSerial()
	}
	return q.Service.AttachDisk(ctx, node, vmid, volume, bus, opts)
}

type parkerRaceNodes struct {
	nodes.Service
	c *parkerRaceClient
}

func (c *parkerRaceClient) Nodes() nodes.Service {
	return parkerRaceNodes{Service: c.lifecycleFlowPVE.Nodes(), c: c}
}

func (n parkerRaceNodes) UpdateQemuConfig(ctx context.Context, node, vmid string, p *nodes.UpdateQemuConfigParams) error {
	n.c.mu.Lock()
	if n.c.counting && vmid == strconv.Itoa(n.c.parker) && p.Description != nil {
		n.c.finalizes++
	}
	n.c.mu.Unlock()
	return n.Service.UpdateQemuConfig(ctx, node, vmid, p)
}

// resumeOutcome is one resume call the race row saw.
type resumeOutcome struct {
	lifecycle bool
	err       error
}

// TestIdentityResumeYieldsToLifecycleResume overlaps has_disk with a resume
// inside the disk's lifecycle. attach_disk resumes the park a snapshot-free
// digest refusal left on 777's unused entry, and the journal records that move
// as observed. While that resume still holds the parker's lock before its
// serial write, has_disk resolves the same disk. Its identity check sees the
// old name gone, and it finds the serial write planned and not yet settled in
// the disk's record. It refuses for audit the way an overlapping call does
// while a move is unsettled, names that step, and writes nothing, so it never
// reaches the parker's lock. The lifecycle's resume then writes the one serial
// and the one record, and has_disk finds the disk once attach_disk is done.
func TestIdentityResumeYieldsToLifecycleResume(t *testing.T) {
	captureParkerPoolSweep(t)
	f := digestManagedFixture(t)
	f.client.changeSourceBeforeMove = true
	if err := digestDetach(t, f.deps, f.cid); !errors.Is(err, pve.ErrMoveDiskDigestRefused) {
		t.Fatalf("want the detach's digest refusal, got %v", err)
	}
	record, err := f.journal.Inspect(f.id)
	if err != nil {
		t.Fatal(err)
	}
	race := &parkerRaceClient{lifecycleFlowPVE: f.client, t: t, token: record.DiskToken}
	for vmid := range f.client.state.configs {
		if isParkerVM(f.client, vmid) {
			race.parker = vmid
		}
	}
	if race.parker == 0 || unusedHolding(f.client, f.volume) == "" {
		t.Fatalf("the refused detach left no parker or no unused entry: %v", f.client.state.configs)
	}
	f.deps.PVE = race

	var outcomes []resumeOutcome
	t.Cleanup(setResumeDiskTransferToParkerForTest(func(
		ctx context.Context, c pve.Client, logger *log.Logger, intent pve.DiskTransferIntent, stableID string, cfg pve.ParkerConfig, pctx pve.ParkContext,
	) (string, error) {
		race.mu.Lock()
		first := !race.started
		race.started, race.counting = true, race.counting || first
		race.mu.Unlock()
		landed, err := pve.ResumeDiskTransferToParker(ctx, c, logger, intent, stableID, cfg, pctx)
		race.mu.Lock()
		outcomes = append(outcomes, resumeOutcome{lifecycle: first, err: err})
		if first {
			race.counting = false
		}
		race.mu.Unlock()
		return landed, err
	}))

	var hasDiskErr error
	var fakeBefore, fakeAfter string
	raced := false
	race.beforeSerial = func() {
		if raced {
			return
		}
		raced = true
		fakeBefore = flowFakeWrites(t, f.client)
		done := make(chan error, 1)
		go func() {
			// The short lock wait bounds the row if has_disk ever reaches the
			// lock the lifecycle's resume holds.
			ctx := pve.WithParkerLockWait(digestCtx(), 50*time.Millisecond)
			_, err := HandleHasDisk(f.deps).Handle(ctx, []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
			done <- err
		}()
		hasDiskErr = <-done
		fakeAfter = flowFakeWrites(t, f.client)
	}
	if err := digestAttach(t, f.deps, f.cid); err != nil {
		t.Fatalf("attach_disk: %v", err)
	}
	if !raced {
		t.Fatal("attach_disk's resume wrote no serial, so has_disk never overlapped it")
	}
	if hasDiskErr == nil || !isTypedCPIError(hasDiskErr) || okToRetryCPIError(hasDiskErr) || !strings.HasPrefix(hasDiskErr.Error(), missingVolumeRefusal) {
		t.Fatalf("has_disk during the lifecycle's resume: error %v, want the permanent missing-volume refusal for audit", hasDiskErr)
	}
	requireText(t, hasDiskErr, "has_disk during the lifecycle's resume", []string{"QEMU_AttachDisk) is planned in the disk's record"})
	if fakeAfter != fakeBefore {
		t.Errorf("the overlapping has_disk wrote to PVE\nbefore %s\nafter  %s", fakeBefore, fakeAfter)
	}
	race.mu.Lock()
	if len(outcomes) != 1 || !outcomes[0].lifecycle || outcomes[0].err != nil {
		t.Errorf("resume outcomes = %+v, want only the lifecycle's resume, landing", outcomes)
	}
	if race.lockRefusals != 0 {
		t.Errorf("has_disk met the parker's lock %d times, want it to refuse before the lock", race.lockRefusals)
	}
	if race.serials != 1 || race.finalizes != 1 {
		t.Errorf("the resume wrote %d serials and %d parker records, want one of each", race.serials, race.finalizes)
	}
	race.mu.Unlock()
	result, err := HandleHasDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
	if err != nil || result != true {
		t.Errorf("has_disk after attach_disk = %v, %v, want true", result, err)
	}
}

// TestIdentityResumeWaiterFindsTheTransferFinished races two has_disk calls
// over one transfer that stopped between its move and its serial write. The
// first call's claim-only resume takes the parker's lock. The second call
// reads the same transfer record and then waits for that lock while the first
// writes the serial, finalizes the record, and lets the lock go. Under the
// lock, the second call reads the parker's record again, finds that the
// transfer moved underneath it, and resolves the disk again. It finds the disk
// on its parker with no second serial write and no second finalize.
func TestIdentityResumeWaiterFindsTheTransferFinished(t *testing.T) {
	captureParkerPoolSweep(t)
	f := digestManagedFixture(t)
	parker, token, _ := landedBeforeSerial(t, f)
	race := &parkerRaceClient{lifecycleFlowPVE: f.client, t: t, parker: parker, token: token, counting: true}
	f.deps.PVE = race
	var resumeErrs []error
	t.Cleanup(setResumeDiskTransferToParkerForTest(func(
		ctx context.Context, c pve.Client, logger *log.Logger, intent pve.DiskTransferIntent, stableID string, cfg pve.ParkerConfig, pctx pve.ParkContext,
	) (string, error) {
		landed, err := pve.ResumeDiskTransferToParker(ctx, c, logger, intent, stableID, cfg, pctx)
		race.mu.Lock()
		resumeErrs = append(resumeErrs, err)
		race.mu.Unlock()
		return landed, err
	}))

	// The first call's lock create starts the second call and waits until
	// the second call's create meets the held lock. The second call then
	// waits inside that refusal until the first call has returned, so the
	// two calls never touch the fake at the same time.
	lock := reservedPoolLockPrefix + "vm-" + strconv.Itoa(parker)
	waiting, release := make(chan struct{}), make(chan struct{})
	type answer struct {
		result any
		err    error
	}
	waiter := make(chan answer, 1)
	launched, refused := false, false
	race.lockCreated = func(id string) {
		if id != lock || launched {
			return
		}
		launched = true
		go func() {
			ctx := pve.WithClusterLockPollForTest(pve.WithParkerLockWait(digestCtx(), 5*time.Second), time.Millisecond)
			result, err := HandleHasDisk(f.deps).Handle(ctx, []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
			waiter <- answer{result, err}
		}()
		<-waiting
	}
	race.lockRefused = func(id string) {
		if id != lock || refused {
			return
		}
		refused = true
		close(waiting)
		<-release
	}
	result, err := HandleHasDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
	if !launched {
		t.Fatalf("the first has_disk took no parker lock (result %v, err %v), so nothing waited on it", result, err)
	}
	close(release)
	second := <-waiter
	if err != nil || result != true {
		t.Errorf("the first has_disk = %v, %v, want true", result, err)
	}
	if second.err != nil || second.result != true {
		t.Errorf("the has_disk that waited for the lock = %v, %v, want true", second.result, second.err)
	}
	race.mu.Lock()
	defer race.mu.Unlock()
	if race.lockRefusals == 0 {
		t.Error("the second has_disk never met the first one's parker lock")
	}
	if race.serials != 1 || race.finalizes != 1 {
		t.Errorf("the two calls wrote %d serials and %d parker records, want one of each", race.serials, race.finalizes)
	}
	if len(resumeErrs) != 2 || resumeErrs[0] != nil || resumeErrs[1] == nil || !strings.Contains(resumeErrs[1].Error(), "moved underneath this call") {
		t.Errorf("resume results = %v, want the first to land and the second to find the transfer moved underneath it", resumeErrs)
	}
	if slots := serialSlots(f.client.state.configs, token); len(slots) != 1 || !strings.HasPrefix(slots[0], strconv.Itoa(parker)+".") {
		t.Errorf("the disk's serial sits on %v, want one slot on parker %d", slots, parker)
	}
}

// landedSlot names the parker slot whose volume is landed.
func landedSlot(t *testing.T, f digestManaged, parker int, landed string) string {
	t.Helper()
	for slot, value := range f.client.state.configs[parker] {
		if text, ok := value.(string); ok && (text == landed || strings.HasPrefix(text, landed+",")) {
			return slot
		}
	}
	t.Fatalf("no slot on parker %d holds %s", parker, landed)
	return ""
}

// TestIdentityCheckRefusesTransfersOnlyAFullResumeFinishes covers transfers
// that a claim-only resume can't finish and that no later call finishes
// either, because every managed call runs the identity check before its
// lifecycle opens. Each one refuses for audit, permanently, with the reason,
// and writes nothing. In the first, the parker's slot lost the landed volume,
// which leaves only the config-edit attach. In the second, the slot holds
// another parker-named volume with no serial, which no move in the disk's
// record landed, so the identity check leaves it untouched.
func TestIdentityCheckRefusesTransfersOnlyAFullResumeFinishes(t *testing.T) {
	captureParkerPoolSweep(t)
	for _, tc := range []struct {
		name   string
		reason string
		shape  func(t *testing.T, f digestManaged, parker int, landed string)
	}{
		{"released volume", "attach the released volume by config edit", func(t *testing.T, f digestManaged, parker int, landed string) {
			delete(f.client.state.configs[parker], landedSlot(t, f, parker, landed))
		}},
		{"another disk's landing", "no move the disk's record observed", func(t *testing.T, f digestManaged, parker int, landed string) {
			storage, _, _ := strings.Cut(landed, ":")
			other := fmt.Sprintf("%s:vm-%d-disk-99", storage, parker)
			slot := landedSlot(t, f, parker, landed)
			text, _ := f.client.state.configs[parker][slot].(string)
			f.client.state.configs[parker][slot] = other + strings.TrimPrefix(text, landed)
			f.client.state.volumes[other] = &nodes.GetStorageContentResponse{Size: 1 << 30, Format: "raw"}
		}},
	} {
		for _, op := range []string{"has_disk", "attach_disk"} {
			t.Run(tc.name+"/"+op, func(t *testing.T) {
				f := digestManagedFixture(t)
				parker, token, landed := landedBeforeSerial(t, f)
				tc.shape(t, f, parker, landed)
				fakeBefore := flowFakeWrites(t, f.client)
				journalBefore := journalFiles(t, f.deps.Config.StorageAllocationJournalDir)
				var err error
				if op == "has_disk" {
					_, err = HandleHasDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
				} else {
					err = digestAttach(t, f.deps, f.cid)
				}
				if err == nil || !isTypedCPIError(err) || okToRetryCPIError(err) || !strings.HasPrefix(err.Error(), missingVolumeRefusal) {
					t.Fatalf("%s: error %v, want the permanent missing-volume refusal for audit", op, err)
				}
				requireText(t, err, op, []string{tc.reason})
				if after := flowFakeWrites(t, f.client); after != fakeBefore {
					t.Errorf("%s wrote to PVE\nbefore %s\nafter  %s", op, fakeBefore, after)
				}
				if after := journalFiles(t, f.deps.Config.StorageAllocationJournalDir); !reflect.DeepEqual(after, journalBefore) {
					t.Errorf("%s changed the journal", op)
				}
				requireTransferUnfinished(t, f, parker, token)
			})
		}
	}
}

// editParkedRecord rewrites the parker's transfer record for token with edit.
func editParkedRecord(t *testing.T, f digestManaged, parker int, token string, edit func(entry map[string]any)) {
	t.Helper()
	cfg := f.client.state.configs[parker]
	nonBOSH, raw := pve.ParseSentinel(pve.DescriptionFromConfig(cfg))
	var disks map[string]map[string]any
	if err := json.Unmarshal(raw["bosh_parked_disks"], &disks); err != nil {
		t.Fatal(err)
	}
	edit(disks[token])
	encoded, err := json.Marshal(disks)
	if err != nil {
		t.Fatal(err)
	}
	raw["bosh_parked_disks"] = encoded
	description, err := pve.RenderSentinel(nonBOSH, raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg["description"] = description
}

// TestIdentityCheckKeepsTheAnswerWhenNothingMoved covers a transfer record
// that no window of the resume converges, here one that names no source VM
// after the parker's landed slot is gone. The identity check's claim-only
// resume returns the transfer's permanent error, and the check resolves the
// disk once more in case another call changed the transfer in the meantime.
// Nothing changed, so the check gives the resume's permanent answer without
// resuming again and without writing anything.
func TestIdentityCheckKeepsTheAnswerWhenNothingMoved(t *testing.T) {
	captureParkerPoolSweep(t)
	f := digestManagedFixture(t)
	parker, token, landed := landedBeforeSerial(t, f)
	delete(f.client.state.configs[parker], landedSlot(t, f, parker, landed))
	editParkedRecord(t, f, parker, token, func(entry map[string]any) { delete(entry, "source_vm_cid") })
	calls := 0
	resumeThen(t, func() { calls++ })
	fakeBefore := flowFakeWrites(t, f.client)
	_, err := HandleHasDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
	if calls != 1 {
		t.Fatalf("the resume ran %d times, want once (err = %v)", calls, err)
	}
	if err == nil || !isTypedCPIError(err) || okToRetryCPIError(err) || !strings.Contains(err.Error(), "neither the parker nor the source VM holds a state") {
		t.Fatalf("has_disk: error %v, want the transfer's permanent error for a state no window converges", err)
	}
	if after := flowFakeWrites(t, f.client); after != fakeBefore {
		t.Errorf("has_disk wrote to PVE\nbefore %s\nafter  %s", fakeBefore, after)
	}
	requireTransferUnfinished(t, f, parker, token)
}

// TestIdentityCheckRefusesARecordNeedingReconciliation covers a disk whose
// transfer stopped between its move and its serial write and whose record
// then went to reconciliation_required. The identity check doesn't resume the
// transfer on such a record. It refuses for audit, permanently, says the
// record needs reconciliation, and writes nothing.
func TestIdentityCheckRefusesARecordNeedingReconciliation(t *testing.T) {
	captureParkerPoolSweep(t)
	f := digestManagedFixture(t)
	parker, token, _ := landedBeforeSerial(t, f)
	handle, err := f.journal.Acquire(t.Context(), f.id)
	if err != nil {
		t.Fatal(err)
	}
	record := handle.Record()
	record.State = aj.ReconciliationRequired
	record.Reason = "operator audit pending"
	if err := handle.Save(record); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	fakeBefore := flowFakeWrites(t, f.client)
	_, err = HandleHasDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
	if err == nil || !isTypedCPIError(err) || okToRetryCPIError(err) || !strings.HasPrefix(err.Error(), missingVolumeRefusal) {
		t.Fatalf("has_disk: error %v, want the permanent missing-volume refusal for audit", err)
	}
	requireText(t, err, "has_disk on a record needing reconciliation", []string{"needs reconciliation"})
	if after := flowFakeWrites(t, f.client); after != fakeBefore {
		t.Errorf("has_disk wrote to PVE\nbefore %s\nafter  %s", fakeBefore, after)
	}
	requireTransferUnfinished(t, f, parker, token)
}

// TestIdentityCheckRefusesADanglingUnusedEntry covers a source VM whose
// unused0 still names the transfer's old name after storage stopped listing
// it, the way an entry dangles once someone removes the volume by hand. The
// identity check's claim-only resume stops at the move, and the check
// resolves the disk once more. The old name is still gone and nothing
// changed, so the check refuses for audit, permanently, with the move's
// reason. A second call gets the same answer, and neither call writes
// anything.
func TestIdentityCheckRefusesADanglingUnusedEntry(t *testing.T) {
	captureParkerPoolSweep(t)
	f := digestManagedFixture(t)
	parker, token, _ := landedBeforeSerial(t, f)
	if taken, ok := f.client.state.configs[777]["unused0"]; ok {
		t.Fatalf("777's unused0 already holds %v", taken)
	}
	if f.client.state.volumes[f.volume] != nil {
		t.Fatalf("storage still lists %s", f.volume)
	}
	f.client.state.configs[777]["unused0"] = f.volume
	runs := recordResumes(t, func(int) {})
	fakeBefore := flowFakeWrites(t, f.client)
	journalBefore := journalFiles(t, f.deps.Config.StorageAllocationJournalDir)
	for call := 1; call <= 2; call++ {
		before := len(*runs)
		_, err := HandleHasDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
		if len(*runs) != before+1 {
			t.Fatalf("has_disk call %d ran the resume %d times, want once (err = %v)", call, len(*runs)-before, err)
		}
		requireClaimOnlyMove(t, (*runs)[before])
		if err == nil || !isTypedCPIError(err) || okToRetryCPIError(err) || !strings.HasPrefix(err.Error(), missingVolumeRefusal) {
			t.Fatalf("has_disk call %d: error %v, want the permanent missing-volume refusal for audit", call, err)
		}
		requireText(t, err, fmt.Sprintf("has_disk call %d", call), []string{
			"move the volume off source vm 777", "storage doesn't list " + f.volume + ", so source vm 777's entry for it may be dangling",
		})
	}
	if after := flowFakeWrites(t, f.client); after != fakeBefore {
		t.Errorf("has_disk wrote to PVE\nbefore %s\nafter  %s", fakeBefore, after)
	}
	if after := journalFiles(t, f.deps.Config.StorageAllocationJournalDir); !reflect.DeepEqual(after, journalBefore) {
		t.Error("has_disk changed the journal")
	}
	requireTransferUnfinished(t, f, parker, token)
}

// TestIdentityCheckFinishesTheMoveWhenTheOldNameIsBack covers a case where
// storage doesn't list the transfer's old name when the identity check looks,
// but lists it again by the time the claim-only resume reads the source VM,
// which names it on unused7. The resume stops at the move, the check resolves
// the disk once more and finds the old name, and detach_disk goes on from
// there. detach_disk's own resume runs inside the disk's lifecycle, moves the
// volume onto the parker, and finishes the transfer.
func TestIdentityCheckFinishesTheMoveWhenTheOldNameIsBack(t *testing.T) {
	captureParkerPoolSweep(t)
	f := digestManagedFixture(t)
	parker, token, landed := landedBeforeSerial(t, f)
	runs := recordResumes(t, func(run int) {
		if run == 0 {
			f.client.state.configs[777]["unused7"] = f.volume
			f.client.state.volumes[f.volume] = &nodes.GetStorageContentResponse{Size: 5 << 30, Format: "raw"}
		}
	})
	if err := digestDetach(t, f.deps, f.cid); err != nil {
		t.Fatalf("detach_disk once the old name is back: %v", err)
	}
	if len(*runs) != 2 {
		t.Fatalf("the resume ran %d times, want the identity check's and then detach_disk's", len(*runs))
	}
	requireClaimOnlyMove(t, (*runs)[0])
	if (*runs)[1].claimOnly || (*runs)[1].err != nil {
		t.Fatalf("detach_disk's resume = claim-only %v, %v, want the full resume landing the disk", (*runs)[1].claimOnly, (*runs)[1].err)
	}
	if f.client.moveCalls != 1 {
		t.Fatalf("detach_disk sent %d moves, want the resume's one", f.client.moveCalls)
	}
	if value, kept := f.client.state.configs[777]["unused7"]; kept {
		t.Fatalf("777's unused7 still holds %v", value)
	}
	slots := serialSlots(f.client.state.configs, token)
	if len(slots) != 1 || !strings.HasPrefix(slots[0], strconv.Itoa(parker)+".") {
		t.Fatalf("the disk's serial sits on %v, want one slot on parker %d", slots, parker)
	}
	moved, _, _ := strings.Cut(f.client.state.configs[parker][strings.TrimPrefix(slots[0], strconv.Itoa(parker)+".")].(string), ",")
	if moved == landed || moved == f.volume {
		t.Fatalf("the disk's serial sits on %s, want the volume the resume moved", moved)
	}
	_, raw := pve.ParseSentinel(pve.DescriptionFromConfig(f.client.state.configs[parker]))
	var disks map[string]map[string]any
	if err := json.Unmarshal(raw["bosh_parked_disks"], &disks); err != nil {
		t.Fatal(err)
	}
	if volid, _ := disks[token]["volid"].(string); volid != moved {
		t.Fatalf("parker %d's record for %s names %q, want the finished transfer naming %s", parker, token, volid, moved)
	}
	record, err := f.journal.Inspect(f.id)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(record.Steps, func(step aj.Step) bool {
		return step.State == aj.Observed && strings.HasSuffix(step.Kind, "_Nodes_CreateQemuMoveDisk") &&
			len(step.VolIDs) == 2 && step.VolIDs[0] == f.volume && step.VolIDs[1] == moved
	}) {
		t.Fatalf("the record observed no move of %s to %s: %+v", f.volume, moved, record.Steps)
	}
}

// appendObservedMove adds an observed move to the disk's record. The move
// takes the volume named from off the VM numbered vmid, lands it as to, and
// carries the target of the record's last move. A handle that Acquire opens
// can't add a step to a record that went through a crash, so the row rewrites
// the record file through rewriteRecord, which gives the same result as
// reading back a record that a release left behind. A to equal to from
// records a move that PVE refused, which holds only the name it started from.
func appendObservedMove(t *testing.T, f digestManaged, vmid int, from, to string) {
	t.Helper()
	record, err := f.journal.Inspect(f.id)
	if err != nil {
		t.Fatal(err)
	}
	var target aj.Target
	for i := range record.Steps {
		if strings.HasSuffix(record.Steps[i].Kind, "_Nodes_CreateQemuMoveDisk") {
			target = record.Steps[i].Target
		}
	}
	if target.Node == "" {
		t.Fatal("the disk's record holds no move to copy a target from")
	}
	target.VMID, target.IntendedVolume = vmid, from
	volids := []string{from, to}
	if to == from {
		volids = volids[:1]
	}
	record.Steps = append(record.Steps, aj.Step{
		ID:      fmt.Sprintf("attempt-%d-step-%d", record.ActiveAttempt(), len(record.Steps)),
		Attempt: record.ActiveAttempt(), Kind: "lifecycle_attach_disk_Nodes_CreateQemuMoveDisk",
		Target: target, State: aj.Observed, VolIDs: volids,
	})
	(&unfiredMove{digestManaged: f}).rewriteRecord(t, record)
}

// TestIdentityCheckLeavesALandingAMoveTookBack covers a disk that moved onto
// the parker, moved back to 777 under its old name, and started a second
// transfer to the parker. Meanwhile another volume with no serial took the
// first landing's name on the slot that the second transfer's record names.
// The disk's record observed both moves, so the first landing proves nothing
// about that name anymore. The identity check's claim-only resume leaves the
// volume untouched and refuses for audit, permanently.
func TestIdentityCheckLeavesALandingAMoveTookBack(t *testing.T) {
	captureParkerPoolSweep(t)
	f := digestManagedFixture(t)
	parker, token, landed := landedBeforeSerial(t, f)
	appendObservedMove(t, f, parker, landed, f.volume)
	f.client.state.volumes[landed] = &nodes.GetStorageContentResponse{Size: 1 << 30, Format: "raw"}
	slot := landedSlot(t, f, parker, landed)
	slotBefore := f.client.state.configs[parker][slot]
	fakeBefore := flowFakeWrites(t, f.client)
	journalBefore := journalFiles(t, f.deps.Config.StorageAllocationJournalDir)
	_, err := HandleHasDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
	if err == nil || !isTypedCPIError(err) || okToRetryCPIError(err) || !strings.HasPrefix(err.Error(), missingVolumeRefusal) {
		t.Fatalf("has_disk: error %v, want the permanent missing-volume refusal for audit", err)
	}
	requireText(t, err, "has_disk over a landing a move took back", []string{"no move the disk's record observed"})
	if got := f.client.state.configs[parker][slot]; got != slotBefore {
		t.Fatalf("parker slot %s = %v, want %v left untouched", slot, got, slotBefore)
	}
	if after := flowFakeWrites(t, f.client); after != fakeBefore {
		t.Errorf("has_disk wrote to PVE\nbefore %s\nafter  %s", fakeBefore, after)
	}
	if after := journalFiles(t, f.deps.Config.StorageAllocationJournalDir); !reflect.DeepEqual(after, journalBefore) {
		t.Error("has_disk changed the journal")
	}
	requireTransferUnfinished(t, f, parker, token)
}

// TestIdentityCheckKeepsALandingARefusedMoveLeft covers a disk whose record
// observed a move from the landed name that PVE refused, the way the digest
// guard records a refusal with only the name the move started from. Nothing
// moved, so the landing still proves the parker's volume, and has_disk's
// claim-only resume writes the disk's serial on the landed slot.
func TestIdentityCheckKeepsALandingARefusedMoveLeft(t *testing.T) {
	captureParkerPoolSweep(t)
	f := digestManagedFixture(t)
	parker, token, landed := landedBeforeSerial(t, f)
	appendObservedMove(t, f, parker, landed, landed)
	result, err := HandleHasDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
	if err != nil || result != true {
		t.Fatalf("has_disk after a refused move from the landed name = %v, %v, want true", result, err)
	}
	slots := serialSlots(f.client.state.configs, token)
	if want := strconv.Itoa(parker) + "." + landedSlot(t, f, parker, landed); len(slots) != 1 || slots[0] != want {
		t.Fatalf("the disk's serial sits on %v, want %s", slots, want)
	}
}

// TestIdentityCheckDropsALandingAnotherVMMovedOff covers a landing that a
// later move took away from a VM whose number the landed name doesn't embed.
// After the disk landed on the parker, attach_disk put the volume on 778 by a
// config edit under the parker's name, and a detach from 778 then moved it
// off under another name. Meanwhile another volume with no serial took the
// landed name on the slot that the transfer's record names. The move ran on
// the landing's node, so the landing proves nothing about that name anymore,
// and the identity check refuses for audit, permanently, and leaves the
// volume untouched.
func TestIdentityCheckDropsALandingAnotherVMMovedOff(t *testing.T) {
	captureParkerPoolSweep(t)
	f := digestManagedFixture(t)
	parker, token, landed := landedBeforeSerial(t, f)
	addVM778(f)
	storage, _, _ := strings.Cut(landed, ":")
	appendObservedMove(t, f, 778, landed, fmt.Sprintf("%s:vm-%d-disk-98", storage, parker))
	slot := landedSlot(t, f, parker, landed)
	slotBefore := f.client.state.configs[parker][slot]
	fakeBefore := flowFakeWrites(t, f.client)
	journalBefore := journalFiles(t, f.deps.Config.StorageAllocationJournalDir)
	_, err := HandleHasDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
	if err == nil || !isTypedCPIError(err) || okToRetryCPIError(err) || !strings.HasPrefix(err.Error(), missingVolumeRefusal) {
		t.Fatalf("has_disk: error %v, want the permanent missing-volume refusal for audit", err)
	}
	requireText(t, err, "has_disk over a landing 778 moved off", []string{"no move the disk's record observed"})
	if got := f.client.state.configs[parker][slot]; got != slotBefore {
		t.Fatalf("parker slot %s = %v, want %v left untouched", slot, got, slotBefore)
	}
	if after := flowFakeWrites(t, f.client); after != fakeBefore {
		t.Errorf("has_disk wrote to PVE\nbefore %s\nafter  %s", fakeBefore, after)
	}
	if after := journalFiles(t, f.deps.Config.StorageAllocationJournalDir); !reflect.DeepEqual(after, journalBefore) {
		t.Error("has_disk changed the journal")
	}
	requireTransferUnfinished(t, f, parker, token)
}

// lockForbidden is the flow fake with a pool service that refuses every
// parker lock's create with a 403, the way PVE answers a token that lacks
// Pool.Allocate on bosh-lock-*.
type lockForbidden struct {
	*lifecycleFlowPVE
	t       *testing.T
	refused *int
}

type lockForbiddenPools struct {
	pve.PoolService
	c lockForbidden
}

func (c lockForbidden) Pools() pve.PoolService {
	return lockForbiddenPools{PoolService: c.lifecycleFlowPVE.Pools(), c: c}
}

func (p lockForbiddenPools) CreatePool(ctx context.Context, id, comment string) error {
	if !strings.HasPrefix(id, reservedPoolLockPrefix) {
		return p.PoolService.CreatePool(ctx, id, comment)
	}
	*p.c.refused++
	body, err := json.Marshal(map[string]any{"data": nil, "message": "Permission check failed (/pool/" + id + ", Pool.Allocate)\n"})
	if err != nil {
		p.c.t.Fatal(err)
	}
	return fmt.Errorf("API request failed: %w", sdkerrors.ParseAPIError(403, body))
}

// TestIdentityCheckRefusesWhenPVERefusesTheParkerLock covers a token that
// lacks Pool.Allocate on bosh-lock-*, so PVE refuses the parker's lock. The
// identity check's claim-only resume never claims without that lock, and no
// retry grants it, so has_disk refuses for audit, permanently, and names the
// lock. A second call gets the same answer, and neither call writes anything
// or changes the journal.
func TestIdentityCheckRefusesWhenPVERefusesTheParkerLock(t *testing.T) {
	captureParkerPoolSweep(t)
	f := digestManagedFixture(t)
	parker, token, _ := landedBeforeSerial(t, f)
	refused := 0
	f.deps.PVE = lockForbidden{lifecycleFlowPVE: f.client, t: t, refused: &refused}
	runs := recordResumes(t, func(int) {})
	fakeBefore := flowFakeWrites(t, f.client)
	journalBefore := journalFiles(t, f.deps.Config.StorageAllocationJournalDir)
	for call := 1; call <= 2; call++ {
		before, refusedBefore := len(*runs), refused
		_, err := HandleHasDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
		if len(*runs) != before+1 {
			t.Fatalf("has_disk call %d ran the resume %d times, want once (err = %v)", call, len(*runs)-before, err)
		}
		run := (*runs)[before]
		if refusal, ok := pve.AsClaimOnlyRefusal(run.err); !run.claimOnly || !ok || refusal.Window != pve.ClaimOnlyUnserialized {
			t.Fatalf("has_disk call %d's resume = claim-only %v, %v, want a claim-only refusal for running unserialized", call, run.claimOnly, run.err)
		}
		if refused == refusedBefore {
			t.Fatalf("has_disk call %d never asked PVE for the parker's lock", call)
		}
		if err == nil || !isTypedCPIError(err) || okToRetryCPIError(err) || !strings.HasPrefix(err.Error(), missingVolumeRefusal) {
			t.Fatalf("has_disk call %d: error %v, want the permanent missing-volume refusal for audit", call, err)
		}
		requireText(t, err, fmt.Sprintf("has_disk call %d", call), []string{"PVE refused the parker's lock"})
	}
	if after := flowFakeWrites(t, f.client); after != fakeBefore {
		t.Errorf("has_disk wrote to PVE\nbefore %s\nafter  %s", fakeBefore, after)
	}
	if after := journalFiles(t, f.deps.Config.StorageAllocationJournalDir); !reflect.DeepEqual(after, journalBefore) {
		t.Error("has_disk changed the journal")
	}
	requireTransferUnfinished(t, f, parker, token)
}

// poolWriteCounter is the flow fake with a count of the pool writes it
// receives. A parker lock that's taken and then released leaves the fake's
// pools as they were, so only the count shows it.
type poolWriteCounter struct {
	*lifecycleFlowPVE
	writes *int
}

type countedPools struct {
	pve.PoolService
	writes *int
}

func (c poolWriteCounter) Pools() pve.PoolService {
	return countedPools{PoolService: c.lifecycleFlowPVE.Pools(), writes: c.writes}
}

func (p countedPools) CreatePool(ctx context.Context, id, comment string) error {
	*p.writes++
	return p.PoolService.CreatePool(ctx, id, comment)
}

func (p countedPools) DeletePool(ctx context.Context, id string) error {
	*p.writes++
	return p.PoolService.DeletePool(ctx, id)
}

func (p countedPools) AddVM(ctx context.Context, id string, vmid int64) error {
	*p.writes++
	return p.PoolService.AddVM(ctx, id, vmid)
}

// flowFakeWrites renders everything the flow fake changes when it takes a
// write, so two renders differ whenever a write reached it in between.
func flowFakeWrites(t *testing.T, c *lifecycleFlowPVE) string {
	t.Helper()
	out, err := json.Marshal(map[string]any{
		"configs": c.state.configs, "volumes": c.state.volumes, "pools": c.state.pools, "members": c.state.poolMembers,
		"created": c.state.created, "parks": c.state.parkMutations, "generation": c.generation, "moves": c.moveCalls,
		"deletes": c.deletes, "migrations": c.migrations, "resizes": c.resizeCalls, "snapshots": c.snapshots,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// journalFiles reads every file under the journal's directory.
func journalFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := os.ReadFile(path)
		files[path] = body
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestIdentityResumeRefusesAnUnsettledMove covers a detach whose move answer
// was lost after PVE forked the task, so the record keeps the move step
// planned, and the move then lands on the parker without the disk's serial.
// The landed name is in no step of the record, so the identity check would
// refuse the disk after any claim. has_disk and then detach_disk refuse for
// audit the way they did before the identity check could resume, and neither
// writes to PVE or the journal.
func TestIdentityResumeRefusesAnUnsettledMove(t *testing.T) {
	captureParkerPoolSweep(t)
	m := unfiredDetach(t, true)
	if outcome := m.client.runLostMove(0); !strings.HasPrefix(outcome, "moved ") {
		t.Fatalf("the lost move didn't land: %s", outcome)
	}
	if m.step.State != aj.Planned {
		t.Fatalf("the lost move's step is %s, want it planned", m.step.State)
	}
	poolWrites := 0
	m.deps.PVE = poolWriteCounter{lifecycleFlowPVE: m.client, writes: &poolWrites}
	fakeBefore := flowFakeWrites(t, m.client)
	journalBefore := journalFiles(t, m.deps.Config.StorageAllocationJournalDir)

	_, hasErr := HandleHasDisk(m.deps).Handle(settleAt(pastQuietPeriod), []json.RawMessage{planJSON(t, m.cid)}, jsonrpc.Context{})
	detachErr := detachDiskAt(t, settleAt(pastQuietPeriod), m.deps, "777", m.cid)
	if after := flowFakeWrites(t, m.client); after != fakeBefore || poolWrites != 0 {
		t.Errorf("PVE took writes (%d pool writes)\nbefore %s\nafter  %s", poolWrites, fakeBefore, after)
	}
	if after := journalFiles(t, m.deps.Config.StorageAllocationJournalDir); !reflect.DeepEqual(after, journalBefore) {
		t.Error("the journal changed")
	}
	for _, call := range []struct {
		name string
		err  error
	}{{"has_disk", hasErr}, {"detach_disk", detachErr}} {
		where := call.name + " after the lost move landed"
		if call.err == nil || !isTypedCPIError(call.err) || okToRetryCPIError(call.err) || !strings.HasPrefix(call.err.Error(), missingVolumeRefusal) {
			t.Errorf("%s: error %v, want the permanent missing-volume refusal for audit", where, call.err)
		}
	}
}

// TestOverlappingMoveRefusalClearsOnRerun covers a call that overlaps a
// detach whose move has landed and whose step isn't observed yet. has_disk
// then sees the parker's transfer record name a volume that's gone, and it
// refuses for audit the way it did before the identity check could resume,
// without writing anything. Once the detach has finished, the same has_disk
// finds the disk on its parker, with no change made in between.
func TestOverlappingMoveRefusalClearsOnRerun(t *testing.T) {
	captureParkerPoolSweep(t)
	f := digestManagedFixture(t)
	var overlapErr error
	overlapped := false
	f.client.afterMove = func() {
		if overlapped {
			return
		}
		overlapped = true
		fakeBefore := flowFakeWrites(t, f.client)
		journalBefore := journalFiles(t, f.deps.Config.StorageAllocationJournalDir)
		_, overlapErr = HandleHasDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
		if after := flowFakeWrites(t, f.client); after != fakeBefore {
			t.Errorf("the overlapping has_disk wrote to PVE\nbefore %s\nafter  %s", fakeBefore, after)
		}
		if after := journalFiles(t, f.deps.Config.StorageAllocationJournalDir); !reflect.DeepEqual(after, journalBefore) {
			t.Error("the overlapping has_disk changed the journal")
		}
	}
	if err := digestDetach(t, f.deps, f.cid); err != nil {
		t.Fatalf("detach_disk: %v", err)
	}
	if !overlapped {
		t.Fatal("detach_disk sent no move, so has_disk never overlapped it")
	}
	if overlapErr == nil || !isTypedCPIError(overlapErr) || okToRetryCPIError(overlapErr) || !strings.HasPrefix(overlapErr.Error(), missingVolumeRefusal) {
		t.Errorf("has_disk during the detach's move: error %v, want the permanent missing-volume refusal for audit", overlapErr)
	}
	result, err := HandleHasDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
	if err != nil || result != true {
		t.Errorf("has_disk after the detach finished = %v, %v, want true", result, err)
	}
}

// TestUnsettledMoveRefusalNamesItsRunbook checks that the unsettled-move
// refusal points at a heading docs/troubleshooting.md has, so a rename of the
// section can't leave the pointer dangling.
func TestUnsettledMoveRefusalNamesItsRunbook(t *testing.T) {
	const heading = "A disk call refuses while a move to a parker is unsettled"
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "docs", "troubleshooting.md"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for line := range strings.Lines(string(doc)) {
		found = found || strings.HasPrefix(line, "#") && strings.TrimSpace(strings.TrimLeft(line, "#")) == heading
	}
	if !found {
		t.Fatalf("docs/troubleshooting.md has no heading %q", heading)
	}
	captureParkerPoolSweep(t)
	m := unfiredDetach(t, true)
	if outcome := m.client.runLostMove(0); !strings.HasPrefix(outcome, "moved ") {
		t.Fatalf("the lost move didn't land: %s", outcome)
	}
	_, err = HandleHasDisk(m.deps).Handle(settleAt(pastQuietPeriod), []json.RawMessage{planJSON(t, m.cid)}, jsonrpc.Context{})
	requireText(t, err, "has_disk after the lost move landed", []string{`see "` + heading + `" in docs/troubleshooting.md of bosh-proxmox-cpi-release`})
}

// TestClaimLeavesTheSweepAndTailToLaterCalls covers the work a claim-only
// resume leaves undone. A full resume also sweeps the parker's node into the
// parker pool and removes the disk's entries from the source VM, and has_disk's
// claim does neither. The row checks that the entries the claim leaves on 777
// don't make the completion audit refuse in the meantime. It then checks that
// the next detach_disk, delete_disk, attach_disk back to 777, or attach_disk to
// another VM leaves 777's notes and the record just as the same call leaves
// them after a park that finished in one go, and that the next park on the
// parker's node sweeps the node that park swept. The attach to another VM is
// the one call that can't overwrite 777's entry, so it shows the heal comes
// from the parker's landed entry, and the row then detaches the disk from that
// VM and deletes it.
func TestClaimLeavesTheSweepAndTailToLaterCalls(t *testing.T) {
	for _, tc := range []struct {
		name string
		next func(t *testing.T, f digestManaged) error
		// then runs after the comparison, with the fixture parked in one go,
		// the claimed fixture, and the nodes the park in one go swept.
		then func(t *testing.T, whole, f digestManaged, sweeps *[]sweepCall, wholeSwept []string)
	}{
		{"detach_disk", func(t *testing.T, f digestManaged) error {
			return digestDetach(t, f.deps, f.cid)
		}, requireNextParkSweeps},
		{"delete_disk", func(t *testing.T, f digestManaged) error {
			_, err := HandleDeleteDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
			return err
		}, nil},
		{"attach_disk", func(t *testing.T, f digestManaged) error {
			return digestAttach(t, f.deps, f.cid)
		}, nil},
		{"attach_disk to 778", func(t *testing.T, f digestManaged) error {
			addVM778(f)
			_, err := HandleAttachDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, "778"), planJSON(t, f.cid)}, jsonrpc.Context{})
			return err
		}, requireAttachElsewhereHealed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sweeps := captureParkerPoolSweep(t)
			whole, wholeSwept := parkInOneGo(t, sweeps)
			if err := tc.next(t, whole); err != nil {
				t.Fatalf("%s after the park in one go: %v", tc.name, err)
			}
			want777 := vm777Notes(t, whole)
			wantState := recordState(t, whole)

			f := claimLandedDisk(t, sweeps)
			if err := tc.next(t, f); err != nil {
				t.Fatalf("%s after has_disk's claim: %v", tc.name, err)
			}
			if got := vm777Notes(t, f); !reflect.DeepEqual(got, want777) {
				t.Errorf("777's notes after %s are %v, want %v as after a park in one go", tc.name, got, want777)
			}
			if got := recordState(t, f); got != wantState {
				t.Errorf("the record is %s after %s, want %s as after a park in one go", got, tc.name, wantState)
			}
			if tc.then != nil {
				tc.then(t, whole, f, sweeps, wholeSwept)
			}
		})
	}
}

// parkInOneGo builds the managed disk and parks it with a detach_disk that
// finishes in one go. It returns the fixture and the nodes that park swept.
func parkInOneGo(t *testing.T, sweeps *[]sweepCall) (digestManaged, []string) {
	t.Helper()
	whole := digestManagedFixture(t)
	*sweeps = nil
	if err := digestDetach(t, whole.deps, whole.cid); err != nil {
		t.Fatalf("the park in one go: %v", err)
	}
	wholeSwept := sweptNodes(*sweeps)
	if len(wholeSwept) == 0 {
		t.Fatal("the park in one go swept no node")
	}
	return whole, wholeSwept
}

// claimLandedDisk builds the landed shape and runs has_disk, and checks that
// the claim put the serial on the parker, swept nothing, and left 777's
// allocation entry. It also checks that the completion audit sees that entry
// and that its gate still admits the report.
func claimLandedDisk(t *testing.T, sweeps *[]sweepCall) digestManaged {
	t.Helper()
	f := digestManagedFixture(t)
	parker, token, _ := landedBeforeSerial(t, f)
	*sweeps = nil
	result, err := HandleHasDisk(f.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, f.cid)}, jsonrpc.Context{})
	if err != nil || result != true {
		t.Fatalf("has_disk = %v, %v, want true for the landed disk", result, err)
	}
	if slots := serialSlots(f.client.state.configs, token); len(slots) != 1 || !strings.HasPrefix(slots[0], strconv.Itoa(parker)+".") {
		t.Fatalf("has_disk left the disk's serial on %v, want one slot on parker %d", slots, parker)
	}
	if len(*sweeps) != 0 {
		t.Fatalf("has_disk's claim swept %v", sweptNodes(*sweeps))
	}
	entries, err := pve.ParseDiskAllocationProvenance(pve.DescriptionFromConfig(f.client.state.configs[777]))
	if err != nil || len(entries) != 1 {
		t.Fatalf("777 carries entries %v (%v) after the claim, want the disk's entry left for a later call", entries, err)
	}
	report, gateErr := completionAuditGate(t, f)
	if !slices.ContainsFunc(report.Evidence, func(e StorageAllocationEvidence) bool {
		return e.AllocationID == f.id && e.VMID == 777
	}) {
		t.Fatalf("the audit after the claim doesn't see 777's entry for the disk: %+v", report.Evidence)
	}
	if gateErr != nil {
		t.Fatalf("the completion audit refuses while 777 keeps the claimed disk's entry: %v", gateErr)
	}
	return f
}

// requireNextParkSweeps attaches the claimed disk after its healed detach and
// parks it again, and checks that the new park sweeps the nodes the park in
// one go swept.
func requireNextParkSweeps(t *testing.T, _, f digestManaged, sweeps *[]sweepCall, wholeSwept []string) {
	t.Helper()
	*sweeps = nil
	if err := digestAttach(t, f.deps, f.cid); err != nil {
		t.Fatalf("attach_disk after the healed detach: %v", err)
	}
	if err := digestDetach(t, f.deps, f.cid); err != nil {
		t.Fatalf("the next park: %v", err)
	}
	if got := sweptNodes(*sweeps); !reflect.DeepEqual(got, wholeSwept) {
		t.Errorf("the next park swept %v, want %v as the park in one go did", got, wholeSwept)
	}
}

// requireAttachElsewhereHealed checks that the attach to 778 left no entry for
// the claimed disk on 777, which only the tail run from the parker's landed
// entry removes. It then detaches the disk from 778 and deletes it in both
// fixtures, and the record has to end Deleted in both.
func requireAttachElsewhereHealed(t *testing.T, whole, f digestManaged, _ *[]sweepCall, _ []string) {
	t.Helper()
	entries, err := pve.ParseDiskAllocationProvenance(pve.DescriptionFromConfig(f.client.state.configs[777]))
	if err != nil || len(entries) != 0 {
		t.Fatalf("777 still carries entries %v (%v) after the attach to 778, want them healed from the parker's landed entry", entries, err)
	}
	for _, g := range []*digestManaged{&whole, &f} {
		if _, err := HandleDetachDisk(g.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, "778"), planJSON(t, g.cid)}, jsonrpc.Context{}); err != nil {
			t.Fatalf("detach_disk from 778: %v", err)
		}
		if _, err := HandleDeleteDisk(g.deps).Handle(digestCtx(), []json.RawMessage{planJSON(t, g.cid)}, jsonrpc.Context{}); err != nil {
			t.Fatalf("delete_disk after the detach from 778: %v", err)
		}
	}
	if got, want := recordState(t, f), recordState(t, whole); got != want || got != aj.Deleted {
		t.Errorf("the record is %s after the detach from 778 and the delete, want %s as after a park in one go", got, want)
	}
}

// addVM778 gives the fixture a second VM, 778, with 777's settings and no
// disks or notes, so a disk can attach to a VM other than its source.
func addVM778(f digestManaged) {
	cfg := maps.Clone(f.client.state.configs[777])
	for key := range cfg {
		if key == "description" || strings.HasPrefix(key, "scsi") || strings.HasPrefix(key, "virtio") || strings.HasPrefix(key, "sata") || strings.HasPrefix(key, "unused") {
			delete(cfg, key)
		}
	}
	f.client.state.configs[778] = cfg
}

// completionAuditGate runs the audit and the gate that delete_disk's
// completion proof runs, on the nodes f's record names, and returns the
// audit's report and the gate's refusal, if any.
func completionAuditGate(t *testing.T, f digestManaged) (StorageAllocationAudit, error) {
	t.Helper()
	record, err := f.journal.Inspect(f.id)
	if err != nil {
		t.Fatal(err)
	}
	var auditNodes []string
	for i := range record.Steps {
		if node := record.Steps[i].Target.Node; node != "" && !slices.Contains(auditNodes, node) {
			auditNodes = append(auditNodes, node)
		}
	}
	slices.Sort(auditNodes)
	report, err := AuditStorageAllocations(digestCtx(), f.deps, f.journal, auditNodes)
	if err != nil {
		t.Fatalf("the audit after the claim: %v", err)
	}
	return report, storageAuditGateError(digestCtx(), f.deps, "delete_disk", report, storageAuditGateAll)
}

// sweptNodes lists the nodes the captured sweeps ran on, each once, in order.
func sweptNodes(calls []sweepCall) []string {
	var nodes []string
	for i := range calls {
		if !slices.Contains(nodes, calls[i].node) {
			nodes = append(nodes, calls[i].node)
		}
	}
	return nodes
}

// vm777Notes returns descriptionNotes for 777 with f's disk CID, disk token,
// allocation ID, and storage replaced by placeholders, so two fixtures compare
// equal when they file the same notes for their own disks. The fixture picks
// its storage afresh each time, so the storage differs between two of them.
func vm777Notes(t *testing.T, f digestManaged) map[string]string {
	t.Helper()
	record, err := f.journal.Inspect(f.id)
	if err != nil {
		t.Fatal(err)
	}
	storage, _, _ := strings.Cut(f.volume, ":")
	replacer := strings.NewReplacer(f.cid, "<cid>", record.DiskToken, "<token>", f.id, "<allocation>",
		`"`+storage+":", `"<storage>:`, "nfs://nas/"+storage+`"`, `nfs://nas/<storage>"`)
	notes := map[string]string{}
	for key, note := range descriptionNotes(t, f.client.state.configs[777]) {
		notes[replacer.Replace(key)] = replacer.Replace(note)
	}
	return notes
}

// recordState returns the state of f's allocation record.
func recordState(t *testing.T, f digestManaged) aj.State {
	t.Helper()
	record, err := f.journal.Inspect(f.id)
	if err != nil {
		t.Fatal(err)
	}
	return record.State
}
