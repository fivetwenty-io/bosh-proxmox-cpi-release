package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

// ---------------------------------------------------------------------------
// pve.root_disk_bus: import path (createParams disk key + boot order)
// ---------------------------------------------------------------------------

func TestCreateVM_ImportPath_RootDiskBus_Unset_UsesVirtio0(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	h := handlers.HandleCreateVM(buildVMDeps(q, n, c, a))

	args := mkArgs("agent-rootbus-unset", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("rootbus-unset")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(q.createCalls))
	}
	p := q.createCalls[0].params
	if _, present := p["virtio0"]; !present {
		t.Error("createParams must carry a \"virtio0\" key when root_disk_bus is unset")
	}
	if _, present := p["scsi0"]; present {
		t.Error("createParams must not carry a \"scsi0\" key when root_disk_bus is unset")
	}
	if boot, _ := p["boot"].(string); boot != "order=virtio0" {
		t.Errorf("createParams[\"boot\"] = %q; want \"order=virtio0\"", boot)
	}
}

func TestCreateVM_ImportPath_RootDiskBus_SCSI_UsesScsi0(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	deps := buildVMDeps(q, n, c, a)
	deps.Config.RootDiskBus = "scsi"
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-rootbus-scsi", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("rootbus-scsi")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(q.createCalls))
	}
	p := q.createCalls[0].params
	if _, present := p["scsi0"]; !present {
		t.Error("createParams must carry a \"scsi0\" key when root_disk_bus=scsi")
	}
	if _, present := p["virtio0"]; present {
		t.Error("createParams must not carry a \"virtio0\" key when root_disk_bus=scsi")
	}
	if boot, _ := p["boot"].(string); boot != "order=scsi0" {
		t.Errorf("createParams[\"boot\"] = %q; want \"order=scsi0\"", boot)
	}
}

// TestCreateVM_ImportPath_RootDiskBus_SCSI_TrimCapableStorage_KeepsSSD verifies
// that a scsi0 root disk composes correctly with the item 2.3 discard/ssd
// TRIM-capability auto-resolution: unlike the default virtio0 root (whose
// virtio-blk bus filter always drops "ssd"), a scsi0 root disk lives on the
// same virtio-scsi controller persistent disks use, so "ssd" is retained.
func TestCreateVM_ImportPath_RootDiskBus_SCSI_TrimCapableStorage_KeepsSSD(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{}
	c := &vmMockCluster{}
	a := &vmMockAgent{}
	deps := buildVMDeps(q, n, c, a)
	deps.Config.RootDiskBus = "scsi"
	deps.PVE.(*mockPVEClient).clusterStorageSvc = &mockClusterStorage{
		storageName: storageName,
		storageType: "zfspool", // TRIM-capable regardless of format
	}
	h := handlers.HandleCreateVM(deps)

	args := mkArgs("agent-rootbus-scsi-trim", testStemcellCID,
		map[string]any{"cores": 1, "memory": 512},
		defaultNetMap(), []string{}, map[string]any{})

	if _, err := h.Handle(context.Background(), args, mkCtx("rootbus-scsi-trim")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.createCalls) != 1 {
		t.Fatalf("expected 1 Create call, got %d", len(q.createCalls))
	}
	scsi0Val, _ := q.createCalls[0].params["scsi0"].(string)
	if !strings.Contains(scsi0Val, "discard=on") {
		t.Errorf("scsi0 value %q missing discard=on on a TRIM-capable pool", scsi0Val)
	}
	if !strings.Contains(scsi0Val, "ssd=1") {
		t.Errorf("scsi0 value %q missing ssd=1 — a scsi0 root disk must not be bus-filtered like virtio-blk", scsi0Val)
	}
}

// ---------------------------------------------------------------------------
// pve.root_disk_bus: clone path (dominant "template:<vmid>" CID path)
// ---------------------------------------------------------------------------

func rootDiskBusCloneArgs(agentID string) []json.RawMessage {
	return mkArgs(agentID, testTemplateCID,
		map[string]any{"cores": 1, "memory": 512},
		map[string]any{"default": map[string]any{"type": "dynamic", "cloud_properties": map[string]any{}}},
		[]string{}, map[string]any{})
}

// configFnWithDisk returns a vmMockQEMU.configFn that reports diskKey as the
// VM's root disk on every Config() call — both the pre-clone template read
// (resolveTemplateDiskStorage) and the post-clone cloned-config read used to
// patch root-disk perf options.
func configFnWithDisk(diskKey string) func(context.Context, string, int) (map[string]any, error) {
	return func(_ context.Context, _ string, vmid int) (map[string]any, error) {
		// PVE names a VM's disks after their owner, so the config read for the
		// template and for the cloned VM each see their own vmid embedded in
		// the volume name (a mismatched vmid would read as a foreign
		// persistent disk to the rollback guard).
		return map[string]any{
			diskKey: fmt.Sprintf("%s:vm-%d-disk-0,size=5G", storageName, vmid),
			"net0":  "virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0",
		}, nil
	}
}

func TestCreateVM_ClonePath_RootDiskBus_Unset_VirtioTemplate_Succeeds(t *testing.T) {
	t.Parallel()
	n := &vmMockNodes{
		createQemuCloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			raw := sdknodes.CreateQemuCloneResponse{}
			_ = json.Unmarshal([]byte(`"UPID:pve:0000C001:00000001:clone:ok"`), &raw)
			return &raw, nil
		},
	}
	q := &vmMockQEMU{configFn: configFnWithDisk("virtio0")}
	a := &vmMockAgent{}
	deps := buildVMDepsForTemplate(q, n, &vmMockCluster{}, a)
	h := handlers.HandleCreateVM(deps)

	if _, err := h.Handle(context.Background(), rootDiskBusCloneArgs("agent-clone-rootbus-unset"), mkCtx("clone-rootbus-unset")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(n.createQemuCloneCalls) != 1 {
		t.Fatalf("expected 1 clone call, got %d", len(n.createQemuCloneCalls))
	}
}

func TestCreateVM_ClonePath_RootDiskBus_SCSI_MatchingTemplate_Succeeds(t *testing.T) {
	t.Parallel()
	n := &vmMockNodes{
		createQemuCloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			raw := sdknodes.CreateQemuCloneResponse{}
			_ = json.Unmarshal([]byte(`"UPID:pve:0000C002:00000001:clone:ok"`), &raw)
			return &raw, nil
		},
	}
	q := &vmMockQEMU{configFn: configFnWithDisk("scsi0")}
	a := &vmMockAgent{}
	deps := buildVMDepsForTemplate(q, n, &vmMockCluster{}, a)
	deps.Config.RootDiskBus = "scsi"
	h := handlers.HandleCreateVM(deps)

	if _, err := h.Handle(context.Background(), rootDiskBusCloneArgs("agent-clone-rootbus-scsi-match"), mkCtx("clone-rootbus-scsi-match")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(n.createQemuCloneCalls) != 1 {
		t.Fatalf("expected 1 clone call, got %d", len(n.createQemuCloneCalls))
	}
	// Root-disk perf options (iothread defaults true as of item 2.2) are
	// patched onto the Scsi map, not Virtio, when the root disk is scsi0.
	var sawScsi0Patch bool
	for _, c := range n.updateConfigCalls {
		if c.params.Scsi != nil {
			if _, ok := c.params.Scsi[0]; ok {
				sawScsi0Patch = true
			}
		}
	}
	if !sawScsi0Patch {
		t.Error("expected an UpdateQemuConfig call patching Scsi[0] for the scsi0 root disk's perf options")
	}
}

func TestCreateVM_ClonePath_RootDiskBus_SCSI_MismatchedVirtioTemplate_FailsFast(t *testing.T) {
	t.Parallel()
	// createQemuCloneFn intentionally left nil: CreateQemuClone panics if
	// called, proving the bus-mismatch guard rejects before the clone API
	// call. (The rejection still triggers the clone-error rollback, which
	// reads the candidate VM's config, finds no foreign disks to protect,
	// and cleans up the never-cloned candidate idempotently.)
	n := &vmMockNodes{}
	q := &vmMockQEMU{configFn: configFnWithDisk("virtio0")}
	a := &vmMockAgent{}
	deps := buildVMDepsForTemplate(q, n, &vmMockCluster{}, a)
	deps.Config.RootDiskBus = "scsi"
	h := handlers.HandleCreateVM(deps)

	_, err := h.Handle(context.Background(), rootDiskBusCloneArgs("agent-clone-rootbus-scsi-mismatch"), mkCtx("clone-rootbus-scsi-mismatch"))
	if err == nil {
		t.Fatal("expected error for scsi/virtio0 template mismatch, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Errorf("expected non-retriable Cloud error; got: %v", err)
	}
	if !strings.Contains(err.Error(), "root_disk_bus=scsi requires stemcell template") {
		t.Errorf("error missing expected conflict message: %v", err)
	}
	if len(n.createQemuCloneCalls) != 0 {
		t.Errorf("expected 0 clone calls (fail-fast before mutation), got %d", len(n.createQemuCloneCalls))
	}
}

func TestCreateVM_ClonePath_RootDiskBus_SCSI_UndeterminableTemplate_FailsFast(t *testing.T) {
	t.Parallel()
	n := &vmMockNodes{}
	q := &vmMockQEMU{configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
		return nil, cpierrors.Retriable("PVE API unreachable")
	}}
	a := &vmMockAgent{}
	deps := buildVMDepsForTemplate(q, n, &vmMockCluster{}, a)
	deps.Config.RootDiskBus = "scsi"
	h := handlers.HandleCreateVM(deps)

	_, err := h.Handle(context.Background(), rootDiskBusCloneArgs("agent-clone-rootbus-scsi-unknown"), mkCtx("clone-rootbus-scsi-unknown"))
	if err == nil {
		t.Fatal("expected error when the template's root disk bus cannot be verified, got nil")
	}
	if !cpierrors.IsType(err, cpierrors.TypeCloud) {
		t.Errorf("expected non-retriable Cloud error; got: %v", err)
	}
	if !strings.Contains(err.Error(), "root_disk_bus=scsi requires verifying") {
		t.Errorf("error missing expected conflict message: %v", err)
	}
	if len(n.createQemuCloneCalls) != 0 {
		t.Errorf("expected 0 clone calls (fail-fast before mutation), got %d", len(n.createQemuCloneCalls))
	}
}

// TestCreateVM_ClonePath_RootDiskBus_Unset_UndeterminableTemplate_StillClones
// verifies the default (virtio) path stays fail-OPEN on an undeterminable
// template — byte-identical to pre-2.5 behavior — unlike the scsi path above.
func TestCreateVM_ClonePath_RootDiskBus_Unset_UndeterminableTemplate_StillClones(t *testing.T) {
	t.Parallel()
	n := &vmMockNodes{
		createQemuCloneFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuCloneParams) (*sdknodes.CreateQemuCloneResponse, error) {
			raw := sdknodes.CreateQemuCloneResponse{}
			_ = json.Unmarshal([]byte(`"UPID:pve:0000C003:00000001:clone:ok"`), &raw)
			return &raw, nil
		},
	}
	// Only the template's own Config() read (vmid 6042, from testTemplateCID)
	// fails; the candidate VM's later Config() reads (resize, perf-opts patch)
	// must still succeed so this test isolates resolveTemplateDiskStorage's
	// failure specifically, not an unrelated downstream read.
	q := &vmMockQEMU{configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
		if vmid == 6042 {
			return nil, cpierrors.Retriable("PVE API unreachable")
		}
		return map[string]any{
			"virtio0": storageName + ":vm-" + strconv.Itoa(vmid) + "-disk-0,size=5G",
			"net0":    "virtio=aa:bb:cc:dd:ee:ff,bridge=vmbr0",
		}, nil
	}}
	a := &vmMockAgent{}
	deps := buildVMDepsForTemplate(q, n, &vmMockCluster{}, a)
	h := handlers.HandleCreateVM(deps)

	if _, err := h.Handle(context.Background(), rootDiskBusCloneArgs("agent-clone-rootbus-unset-unknown"), mkCtx("clone-rootbus-unset-unknown")); err != nil {
		t.Fatalf("unexpected error (default path must stay fail-open): %v", err)
	}
	if len(n.createQemuCloneCalls) != 1 {
		t.Fatalf("expected 1 clone call (fail-open default), got %d", len(n.createQemuCloneCalls))
	}
}
