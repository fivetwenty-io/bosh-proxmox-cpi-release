package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"sync"
	"testing"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	aj "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/allocationjournal"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

const (
	parkSettleNode   = "pve1"
	parkSettleParker = 90000
)

// parkSettlePVE is one parker whose config writes are checked against the
// digest the way qemu-server's update_vm_api checks them. A write with a
// digest other than the current one is refused with PVE's checksum-mismatch
// answer and changes nothing; every accepted write moves the digest on.
type parkSettlePVE struct {
	pve.Client

	mu         sync.Mutex
	cfg        map[string]any
	generation int
	// beforeWrite, when set, runs before each description write is checked,
	// with the 1-based number of that write. It stands for another request
	// whose write lands between our read and our write.
	beforeWrite func(p *parkSettlePVE, write int)
	// refuseKeepingDigest makes every write fail with the checksum-mismatch
	// answer while the digest stays where it is, which no real PVE does.
	refuseKeepingDigest bool
	writes              int
}

func newParkSettlePVE(t *testing.T, entries map[string]any) *parkSettlePVE {
	t.Helper()
	p := &parkSettlePVE{cfg: map[string]any{"description": parkSettleDescription(t, entries)}}
	p.cfg["digest"] = p.digestLocked()
	return p
}

func parkSettleDescription(t *testing.T, entries map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal entries: %v", err)
	}
	desc, err := pve.RenderSentinel("", map[string]json.RawMessage{"bosh_parked_disks": encoded})
	if err != nil {
		t.Fatalf("render sentinel: %v", err)
	}
	return desc
}

func (p *parkSettlePVE) digestLocked() string { return fmt.Sprintf("digest-%04d", p.generation) }

// otherRequestAdds lands another request's record for key; p.mu must be held.
func (p *parkSettlePVE) otherRequestAdds(t *testing.T, key, volid string) {
	t.Helper()
	desc, _ := p.cfg["description"].(string)
	_, raw := pve.ParseSentinel(desc)
	entries := map[string]any{}
	if encoded, ok := raw["bosh_parked_disks"]; ok {
		if err := json.Unmarshal(encoded, &entries); err != nil {
			t.Fatalf("decode entries: %v", err)
		}
	}
	entries[key] = map[string]any{"disk_cid": volid, "volid": volid, "node": parkSettleNode, "parked_at": "2026-10-05T12:00:00Z"}
	p.cfg["description"] = parkSettleDescription(t, entries)
	p.generation++
	p.cfg["digest"] = p.digestLocked()
}

func (p *parkSettlePVE) entries(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	desc, _ := p.cfg["description"].(string)
	_, raw := pve.ParseSentinel(desc)
	out := map[string]json.RawMessage{}
	if encoded, ok := raw["bosh_parked_disks"]; ok {
		if err := json.Unmarshal(encoded, &out); err != nil {
			t.Fatalf("decode entries: %v", err)
		}
	}
	return out
}

func (p *parkSettlePVE) QEMU() qemu.Service { return parkSettleQEMU{p: p} }
func (p *parkSettlePVE) Nodes() sdknodes.Service {
	return parkSettleNodes{p: p}
}

type parkSettleQEMU struct {
	qemu.Service
	p *parkSettlePVE
}

func (q parkSettleQEMU) Config(_ context.Context, node string, vmid int) (map[string]any, error) {
	if node != parkSettleNode || vmid != parkSettleParker {
		return nil, fmt.Errorf("unexpected config read of %s/%d", node, vmid)
	}
	q.p.mu.Lock()
	defer q.p.mu.Unlock()
	return maps.Clone(q.p.cfg), nil
}

type parkSettleNodes struct {
	sdknodes.Service
	p *parkSettlePVE
}

var parkSettleChecksumMismatch = sdkerrors.ParseAPIError(500, []byte(`{"message":"checksum mismatch (file change by other user?)\n"}`))

func (n parkSettleNodes) UpdateQemuConfig(_ context.Context, _ string, _ string, params *sdknodes.UpdateQemuConfigParams) error {
	n.p.mu.Lock()
	defer n.p.mu.Unlock()
	n.p.writes++
	if n.p.beforeWrite != nil {
		n.p.beforeWrite(n.p, n.p.writes)
	}
	if n.p.refuseKeepingDigest {
		return parkSettleChecksumMismatch
	}
	if params.Digest != nil && *params.Digest != n.p.cfg["digest"] {
		return parkSettleChecksumMismatch
	}
	if params.Description != nil {
		n.p.cfg["description"] = *params.Description
	}
	n.p.generation++
	n.p.cfg["digest"] = n.p.digestLocked()
	return nil
}

// newParkSettleHandle enrolls a journal and starts one disk allocation in it.
func newParkSettleHandle(t *testing.T) *aj.Handle {
	t.Helper()
	directory := t.TempDir()
	// The journal refuses a directory that grants group or other access.
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("restrict journal directory: %v", err)
	}
	journal, err := aj.Initialize(t.Context(), directory, "park-settle", aj.Enrollment{
		ClusterID:               "cluster",
		AuthorityID:             "authority",
		AuditID:                 "park-settle-fixture-audit",
		CompleteHistoricalAudit: true,
		PreviousWriterFenced:    true,
	})
	if err != nil {
		t.Fatalf("open allocation journal: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := journal.Close(); closeErr != nil {
			t.Errorf("close allocation journal: %v", closeErr)
		}
	})
	fingerprint, err := aj.Fingerprint(map[string]string{"fixture": "park-settle"})
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	id, err := aj.NewAllocationID()
	if err != nil {
		t.Fatalf("allocation id: %v", err)
	}
	handle, err := journal.CreateDisk(t.Context(), id, aj.Intent{
		IntentFingerprint: fingerprint,
		PolicyFingerprint: fingerprint,
		PlanVersion:       1,
		Plan:              json.RawMessage(`{"version":1}`),
	})
	if err != nil {
		t.Fatalf("create disk allocation: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := handle.Close(); closeErr != nil {
			t.Errorf("close allocation handle: %v", closeErr)
		}
	})
	return handle
}

// newParkSettleGuard builds the guard the managed park uses, with the same
// settle hook, journaling every write's intent to handle. Its After hook
// stands in for the park's readback and marks the step observed.
func newParkSettleGuard(t *testing.T, p *parkSettlePVE, handle *aj.Handle) *ManagedAllocationGuard {
	t.Helper()
	guard, err := NewManagedAllocationGuard(p, ManagedAllocationHooks{
		Before: func(_ context.Context, call ManagedAllocationMutation) (string, error) {
			return storageMutationIntent(handle, "park_"+call.Service+"_"+call.Method,
				aj.Target{Node: parkSettleNode, VMID: parkSettleParker, Storage: "local-lvm", IntendedVolume: "local-lvm:vm-90000-disk-5"}, nil)
		},
		After: func(_ context.Context, _ ManagedAllocationMutation, step string, _ any) error {
			return storageMutationObserved(handle, step, nil, false)
		},
		Failed: func(_ context.Context, call ManagedAllocationMutation, _ string, _ error) error {
			return storageAllocationUncertain(handle, "parker "+call.Service+"."+call.Method)
		},
		SettleFailedWrite: func(ctx context.Context, call ManagedAllocationMutation, step string, writeErr error) bool {
			return settleRefusedParkerDescription(ctx, p, handle, call, step, writeErr, parkSettleNode)
		},
	})
	if err != nil {
		t.Fatalf("new guard: %v", err)
	}
	return guard
}

// TestManagedParkGuard_SettlesAStaleDigestRefusalSoTheWriterCanRetry runs a
// guarded parker description write through the managed park's guard while
// another request writes the parker between our read and our write. PVE
// refuses our write for its stale digest, and the guard settles the refusal
// instead of poisoning the allocation, so the writer reads again and its second
// write lands with the other request's record intact.
func TestManagedParkGuard_SettlesAStaleDigestRefusalSoTheWriterCanRetry(t *testing.T) {
	t.Parallel()

	p := newParkSettlePVE(t, map[string]any{
		"bpd-ours": map[string]any{"disk_cid": "local-lvm:vm-90000-disk-5", "volid": "local-lvm:vm-90000-disk-5", "node": parkSettleNode, "parked_at": "2026-10-05T12:00:00Z"},
	})
	p.beforeWrite = func(p *parkSettlePVE, write int) {
		if write == 1 {
			p.otherRequestAdds(t, "bpd-theirs", "local-lvm:vm-90000-disk-7")
		}
	}
	handle := newParkSettleHandle(t)
	guard := newParkSettleGuard(t, p, handle)

	// ApplyParkerDiskOverlay runs the same guarded read-modify-write as the
	// park's provenance write, and it is reachable from this package.
	merged, err := pve.ApplyParkerDiskOverlay(t.Context(), guard.Client(), parkSettleNode, parkSettleParker,
		"local-lvm:vm-90000-disk-5", "bpd-ours", "local-lvm:vm-90000-disk-5", map[string]string{"cache": "writeback"}, pve.ParkerConfig{})
	if err != nil {
		t.Fatalf("guarded write through the park guard: %v", err)
	}
	if merged["cache"] != "writeback" {
		t.Errorf("merged = %v, want cache=writeback", merged)
	}
	if guardErr := guard.Err(); guardErr != nil {
		t.Fatalf("the guard was poisoned by a refusal that changed nothing: %v", guardErr)
	}
	entries := p.entries(t)
	if _, ok := entries["bpd-theirs"]; !ok {
		t.Errorf("the other request's record was overwritten; entries now %v", entries)
	}
	if p.writes != 2 {
		t.Errorf("writes=%d, want the refused write and the one that landed", p.writes)
	}
	record := handle.Record()
	if len(record.Steps) != 2 {
		t.Fatalf("journal holds %d steps, want one per write", len(record.Steps))
	}
	for _, step := range record.Steps {
		if step.State != aj.Observed {
			t.Errorf("step %s is %s, want observed", step.ID, step.State)
		}
	}
	if record.State == aj.ReconciliationRequired {
		t.Errorf("allocation state = %s after a settled refusal", record.State)
	}
}

// TestManagedParkGuard_LeavesARefusalItCannotConfirmUncertain covers a
// checksum-mismatch answer whose readback shows the digest the write carried,
// which contradicts the refusal. The settle hook declines, so the guard treats
// the failure as uncertain as it did before.
func TestManagedParkGuard_LeavesARefusalItCannotConfirmUncertain(t *testing.T) {
	t.Parallel()

	p := newParkSettlePVE(t, map[string]any{
		"bpd-ours": map[string]any{"disk_cid": "local-lvm:vm-90000-disk-5", "volid": "local-lvm:vm-90000-disk-5", "node": parkSettleNode, "parked_at": "2026-10-05T12:00:00Z"},
	})
	p.refuseKeepingDigest = true
	handle := newParkSettleHandle(t)
	guard := newParkSettleGuard(t, p, handle)

	_, err := pve.ApplyParkerDiskOverlay(t.Context(), guard.Client(), parkSettleNode, parkSettleParker,
		"local-lvm:vm-90000-disk-5", "bpd-ours", "local-lvm:vm-90000-disk-5", map[string]string{"cache": "writeback"}, pve.ParkerConfig{})
	if err == nil {
		t.Fatal("want an error for an unconfirmed refusal")
	}
	if guard.Err() == nil {
		t.Error("the guard stayed usable after a refusal its readback contradicts")
	}
	if p.writes != 1 {
		t.Errorf("writes=%d, want one write and no retry once the guard is poisoned", p.writes)
	}
	if got := handle.Record().State; got != aj.ReconciliationRequired {
		t.Errorf("allocation state = %s, want %s", got, aj.ReconciliationRequired)
	}
}

// TestSettleRefusedParkerDescription_DeclinesOtherFailures covers the writes
// the settle hook leaves to the guard: a refusal that isn't a stale digest, a
// write that carried no digest, and a write with fields beyond the
// description and digest.
func TestSettleRefusedParkerDescription_DeclinesOtherFailures(t *testing.T) {
	t.Parallel()

	p := newParkSettlePVE(t, map[string]any{})
	handle := newParkSettleHandle(t)
	desc, stale := "x", "digest-old"
	protection := true
	call := func(params *sdknodes.UpdateQemuConfigParams) ManagedAllocationMutation {
		return ManagedAllocationMutation{Service: managedServiceNodes, Method: "UpdateQemuConfig",
			Args: map[string]any{resourceTypeNode: parkSettleNode, metadataKeyVMID: fmt.Sprint(parkSettleParker), managedArgumentParams: params}}
	}
	cases := []struct {
		name string
		call ManagedAllocationMutation
		err  error
	}{
		{"permission refusal", call(&sdknodes.UpdateQemuConfigParams{Description: &desc, Digest: &stale}),
			sdkerrors.ParseAPIError(403, []byte(`{"message":"Permission check failed\n"}`))},
		{"no digest sent", call(&sdknodes.UpdateQemuConfigParams{Description: &desc}), parkSettleChecksumMismatch},
		{"more than a description", call(&sdknodes.UpdateQemuConfigParams{Description: &desc, Digest: &stale, Protection: &protection}),
			parkSettleChecksumMismatch},
		{"transport fault", call(&sdknodes.UpdateQemuConfigParams{Description: &desc, Digest: &stale}),
			errors.New("checksum mismatch (file change by other user?)")},
	}
	for _, tc := range cases {
		if settleRefusedParkerDescription(t.Context(), p, handle, tc.call, "step", tc.err, parkSettleNode) {
			t.Errorf("%s: settled, want it left to the guard", tc.name)
		}
	}
}

// TestManagedDescriptionDigestStale_ReadsAsAStaleDigestToPVEWriters covers the
// lifecycle guard's own stale-digest refusal. A pve-package writer has to see
// it as the same refusal PVE gives, so it reads again instead of failing.
func TestManagedDescriptionDigestStale_ReadsAsAStaleDigestToPVEWriters(t *testing.T) {
	t.Parallel()

	refusal := &managedMutationNotAttempted{err: errManagedDescriptionDigestStale}
	if !pve.IsConfigDigestStale(refusal) {
		t.Errorf("pve.IsConfigDigestStale(%v) = false, want true", refusal)
	}
	if !errors.Is(refusal, errManagedDescriptionDigestStale) || !errors.Is(refusal, pve.ErrMutationNotAttempted) {
		t.Errorf("the refusal lost its own identity: %v", refusal)
	}
}
