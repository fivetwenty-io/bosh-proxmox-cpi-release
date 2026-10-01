package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"
)

// The legacy persistent disk these tests share. Its volume is named for VMID
// 9005, which is a disk-band VMID under the default bands and a VM-band VMID
// once an operator moves the VM band over it.
const (
	ownedLegacyVMID   = 9005
	ownedLegacyVolume = "data:vm-9005-disk-0"
)

// ownerSimVolumeOwner parses the owner PVE's storage plugins read from a
// volume name: the number after "vm-" or "base-" in the last path segment.
var ownerSimVolumeOwner = regexp.MustCompile(`^(?:vm|base)-(\d+)-`)

// ownerSim models the PVE rules the legacy disk paths meet, so a test can
// see whether a volume survived rather than only which calls were made.
//
//   - Deleting a bus slot registers an unusedN entry only when the VM owns
//     the volume, which is when the volume's name carries the VM's VMID.
//   - Deleting an unusedN entry frees a volume the VM owns, and only drops
//     the config line for any other volume.
//   - The SDK's DetachDisk deletes the bus slot and then deletes the first
//     unusedN entry naming the same volume, which is the sweep that frees an
//     owned volume.
//   - Destroying a VM frees every volume it owns that its config references,
//     and with destroy-unreferenced-disks every volume it owns anywhere.
type ownerSim struct {
	mu       sync.Mutex
	configs  map[int]map[string]any
	volumes  map[string]bool
	attaches int
	destroys []string
	stops    int
}

func newOwnerSim(volumes ...string) *ownerSim {
	s := &ownerSim{configs: map[int]map[string]any{}, volumes: map[string]bool{}}
	for _, v := range volumes {
		s.volumes[v] = true
	}
	return s
}

func ownerSimOwner(volid string) int {
	name := volid[strings.LastIndexAny(volid, ":/")+1:]
	m := ownerSimVolumeOwner.FindStringSubmatch(name)
	if len(m) != 2 {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func ownerSimBare(value string) string {
	bare, _, _ := strings.Cut(value, ",")
	return bare
}

func (s *ownerSim) setConfig(vmid int, cfg map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configs[vmid] = cfg
}

func (s *ownerSim) alive(volid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.volumes[volid]
}

func (s *ownerSim) slotValue(vmid int, slot string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.configs[vmid][slot].(string)
	return v, ok
}

func (s *ownerSim) destroyed(vmCID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.destroys {
		if d == vmCID {
			return true
		}
	}
	return false
}

// deleteSlotLocked is PVE's handling of PUT config delete=<slot> without force.
func (s *ownerSim) deleteSlotLocked(vmid int, slot string) {
	cfg := s.configs[vmid]
	value, ok := cfg[slot].(string)
	if !ok {
		return
	}
	delete(cfg, slot)
	volid := ownerSimBare(value)
	owned := ownerSimOwner(volid) == vmid
	if strings.HasPrefix(slot, "unused") {
		if owned {
			delete(s.volumes, volid)
		}
		return
	}
	if owned {
		for i := 0; ; i++ {
			key := fmt.Sprintf("unused%d", i)
			if _, taken := cfg[key]; !taken {
				cfg[key] = volid
				break
			}
		}
	}
}

func (s *ownerSim) qemuService() qemu.Service { return &ownerSimQEMU{sim: s} }

func (s *ownerSim) nodesService() *mockNodesService {
	return &mockNodesService{
		updateQemuConfigFn: func(_ context.Context, _ string, vmidStr string, params *nodes.UpdateQemuConfigParams) error {
			vmid, err := strconv.Atoi(vmidStr)
			if err != nil {
				return err
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			cfg, ok := s.configs[vmid]
			if !ok {
				return &sdkerrors.APIError{HTTPCode: 404}
			}
			if params.Description != nil {
				cfg["description"] = *params.Description
			}
			if params.Tags != nil {
				cfg["tags"] = *params.Tags
			}
			return nil
		},
		deleteQemuFn: func(_ context.Context, _ string, vmidStr string, params *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse, error) {
			vmid, err := strconv.Atoi(vmidStr)
			if err != nil {
				return nil, err
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			cfg, ok := s.configs[vmid]
			if !ok {
				return nil, &sdkerrors.APIError{HTTPCode: 404}
			}
			s.destroys = append(s.destroys, vmidStr)
			for key, raw := range cfg {
				value, isString := raw.(string)
				if !isString || (!strings.HasPrefix(key, "scsi") && !strings.HasPrefix(key, "unused") && !strings.HasPrefix(key, "virtio")) {
					continue
				}
				if volid := ownerSimBare(value); ownerSimOwner(volid) == vmid {
					delete(s.volumes, volid)
				}
			}
			if params != nil && params.DestroyUnreferencedDisks != nil && *params.DestroyUnreferencedDisks {
				for volid := range s.volumes {
					if ownerSimOwner(volid) == vmid {
						delete(s.volumes, volid)
					}
				}
			}
			delete(s.configs, vmid)
			resp := nodes.DeleteQemuResponse{}
			return &resp, nil
		},
	}
}

type ownerSimQEMU struct {
	qemu.Service
	sim *ownerSim
}

func (q *ownerSimQEMU) Config(_ context.Context, _ string, vmid int) (map[string]any, error) {
	q.sim.mu.Lock()
	defer q.sim.mu.Unlock()
	cfg, ok := q.sim.configs[vmid]
	if !ok {
		return nil, &sdkerrors.APIError{HTTPCode: 404}
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		out[k] = v
	}
	return out, nil
}

func (q *ownerSimQEMU) DetachDisk(_ context.Context, _ string, vmid int, slot string) error {
	q.sim.mu.Lock()
	defer q.sim.mu.Unlock()
	cfg, ok := q.sim.configs[vmid]
	if !ok {
		return &sdkerrors.APIError{HTTPCode: 404}
	}
	value, present := cfg[slot].(string)
	if !present {
		return nil
	}
	q.sim.deleteSlotLocked(vmid, slot)
	if strings.HasPrefix(slot, "unused") {
		return nil
	}
	volid := ownerSimBare(value)
	for key, raw := range cfg {
		if v, isString := raw.(string); isString && strings.HasPrefix(key, "unused") && ownerSimBare(v) == volid {
			q.sim.deleteSlotLocked(vmid, key)
			break
		}
	}
	return nil
}

// deleteConfigKey is the raw config delete the slot-delete helper sends, which
// replaced DetachDisk on the legacy detach paths. Unlike DetachDisk it doesn't
// sweep the unused entry PVE leaves for an owned volume; the helper's caller
// does that itself, the way the SDK did.
func (q *ownerSimQEMU) deleteConfigKey(_ string, vmid int, key string) error {
	q.sim.mu.Lock()
	defer q.sim.mu.Unlock()
	if _, ok := q.sim.configs[vmid]; !ok {
		return &sdkerrors.APIError{HTTPCode: 404}
	}
	q.sim.deleteSlotLocked(vmid, key)
	return nil
}

func (q *ownerSimQEMU) AttachDisk(_ context.Context, _ string, vmid int, volid, _ string, opts *qemu.AttachOpts) (string, error) {
	q.sim.mu.Lock()
	defer q.sim.mu.Unlock()
	cfg, ok := q.sim.configs[vmid]
	if !ok {
		return "", &sdkerrors.APIError{HTTPCode: 404}
	}
	q.sim.attaches++
	cfg[opts.DiskID] = volid
	return opts.DiskID, nil
}

func (q *ownerSimQEMU) ListSnapshots(context.Context, string, int) ([]map[string]any, error) {
	return []map[string]any{{"name": "current"}}, nil
}

func (q *ownerSimQEMU) Stop(_ context.Context, _ string, vmid int) (string, error) {
	q.sim.mu.Lock()
	defer q.sim.mu.Unlock()
	if _, ok := q.sim.configs[vmid]; !ok {
		return "", &sdkerrors.APIError{HTTPCode: 404}
	}
	q.sim.stops++
	return "", nil
}

func (q *ownerSimQEMU) Status(context.Context, string, int) (map[string]any, error) {
	return map[string]any{"status": "stopped"}, nil
}

// ownerSimCluster lists every VM the sim holds on one node, with its tags.
func ownerSimCluster(s *ownerSim, node string) *mockClusterSvc {
	return &mockClusterSvc{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			resp := make(sdkcluster.ListResourcesResponse, 0, len(s.configs))
			for vmid, cfg := range s.configs {
				row := map[string]any{"vmid": vmid, "node": node, "type": "qemu"}
				if tags, ok := cfg["tags"].(string); ok {
					row["tags"] = tags
				}
				raw, _ := json.Marshal(row)
				resp = append(resp, raw)
			}
			return &resp, nil
		},
	}
}

// legacyAttachedDescription renders the description sentinel the legacy
// attach writes, mapping each bare volid to the Director's CID.
func legacyAttachedDescription(t *testing.T, cids map[string]string) string {
	t.Helper()
	raw, err := json.Marshal(cids)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := pve.RenderSentinel("", map[string]json.RawMessage{"bosh_attached_disks": raw})
	if err != nil {
		t.Fatal(err)
	}
	return desc
}

// legacyDiskForm is one shape of legacy persistent disk volume: the block
// form create_disk gives a disk on LVM, ZFS, or Ceph, and the file-backed
// form it gives a disk on directory or NFS storage. vmid is the VMID the
// volume's name carries.
type legacyDiskForm struct {
	name   string
	vmid   int
	volume string
}

var legacyDiskForms = []legacyDiskForm{
	{name: "block", vmid: ownedLegacyVMID, volume: ownedLegacyVolume},
	{name: "file-backed", vmid: 9000, volume: "nfs:9000/vm-9000-disk-0.qcow2"},
}

func (f legacyDiskForm) vmCID() string      { return strconv.Itoa(f.vmid) }
func (f legacyDiskForm) systemDisk() string { return fmt.Sprintf("local-lvm:vm-%d-disk-0", f.vmid) }
func (f legacyDiskForm) storage() string    { s, _, _ := strings.Cut(f.volume, ":"); return s }

// ownedLegacyVMConfig is the VM the form's volume is named for, holding its
// own system disk, a cloud-init drive, and the legacy persistent disk on the
// slots extra names. The block form's system disk shares the persistent
// disk's volume name on another pool, which is exactly the shape that leaves
// the name unable to tell them apart.
func ownedLegacyVMConfig(t *testing.T, f legacyDiskForm, cid string, extra map[string]any) map[string]any {
	t.Helper()
	cfg := map[string]any{
		"scsi0":       f.systemDisk() + ",size=10G",
		"ide2":        fmt.Sprintf("local-lvm:vm-%d-cloudinit,media=cdrom", f.vmid),
		"description": legacyAttachedDescription(t, map[string]string{f.volume: cid}),
	}
	for k, v := range extra {
		cfg[k] = v
	}
	return cfg
}

func ownedSimDetachDeps(s *ownerSim, f legacyDiskForm) handlers.Deps {
	deps := detachDeps(s.qemuService())
	deps.Config.DiskStorage = f.storage()
	return deps
}

func assertOwnedLegacyRefusal(t *testing.T, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Error("expected a refusal, got nil")
		return
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal must name %q; got %q", want, err.Error())
		}
	}
	type retriable interface{ OkToRetry() bool }
	var r retriable
	if errors.As(err, &r) && r.OkToRetry() {
		t.Errorf("refusal must not be retriable, because a retry meets the same volume name; got %v", err)
	}
}

// ---------------------------------------------------------------------------
// attach_disk
// ---------------------------------------------------------------------------

// TestAttachDisk_LegacyRefusesVolumeNamedForTargetVM pins the earliest point
// the shape can be stopped: a legacy disk whose volume is named for the VM it
// is about to join would become that VM's own disk in PVE's eyes.
func TestAttachDisk_LegacyRefusesVolumeNamedForTargetVM(t *testing.T) {
	t.Parallel()
	for _, f := range legacyDiskForms {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			s := newOwnerSim(f.volume)
			s.setConfig(f.vmid, map[string]any{"scsi0": f.systemDisk() + ",size=10G"})
			cid := mustEncodeDiskCID(t, f.volume, nil)
			deps := handlers.Deps{
				Config: &config.CPIConfig{Node: testNode, VMDiskFormat: "qcow2", DiskStorage: f.storage()},
				PVE:    &mockPVEClient{qemuSvc: s.qemuService(), nodesSvc: s.nodesService(), clusterSvc: ownerSimCluster(s, testNode)},
				Agent:  &captureAgent{},
				Logger: log.NewNopLogger(),
			}

			_, err := handlers.HandleAttachDisk(deps).Handle(context.Background(), marshalArgs(f.vmCID(), cid), jsonrpc.Context{})

			assertOwnedLegacyRefusal(t, err, f.volume, f.vmCID(), "Let the Director recreate the VM, for example with bosh recreate.")
			if s.attaches != 0 {
				t.Errorf("AttachDisk must not run for a volume named for the target VM; got %d calls", s.attaches)
			}
			if _, ok := s.slotValue(f.vmid, "scsi1"); ok {
				t.Error("the volume must not land on the VM config")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// detach_disk
// ---------------------------------------------------------------------------

// TestDetachDisk_LegacyRefusesOwnedVolumeOnBusSlot pins the refusal before the
// SDK detach, whose unused sweep would make PVE free a volume the VM owns.
func TestDetachDisk_LegacyRefusesOwnedVolumeOnBusSlot(t *testing.T) {
	t.Parallel()
	for _, f := range legacyDiskForms {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			cid := mustEncodeDiskCID(t, f.volume, nil)
			s := newOwnerSim(f.volume, f.systemDisk())
			s.setConfig(f.vmid, ownedLegacyVMConfig(t, f, cid, map[string]any{"scsi1": f.volume + ",size=2G"}))

			_, err := handlers.HandleDetachDisk(ownedSimDetachDeps(s, f)).Handle(context.Background(), marshalArgs(f.vmCID(), cid), jsonrpc.Context{})

			assertOwnedLegacyRefusal(t, err, f.volume, f.vmCID())
			if !s.alive(f.volume) {
				t.Fatal("the persistent volume was freed")
			}
			if v, ok := s.slotValue(f.vmid, "scsi1"); !ok || ownerSimBare(v) != f.volume {
				t.Errorf("the disk must stay on scsi1; got %q (present=%t)", v, ok)
			}
		})
	}
}

// TestDetachDisk_LegacyRefusesOwnedVolumeInUnusedSweep pins the same refusal on
// the lingering-unused path: deleting an unusedN entry for an owned volume is
// itself the free.
func TestDetachDisk_LegacyRefusesOwnedVolumeInUnusedSweep(t *testing.T) {
	t.Parallel()
	for _, f := range legacyDiskForms {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			cid := mustEncodeDiskCID(t, f.volume, nil)
			s := newOwnerSim(f.volume, f.systemDisk())
			s.setConfig(f.vmid, ownedLegacyVMConfig(t, f, cid, map[string]any{"unused0": f.volume}))

			_, err := handlers.HandleDetachDisk(ownedSimDetachDeps(s, f)).Handle(context.Background(), marshalArgs(f.vmCID(), cid), jsonrpc.Context{})

			assertOwnedLegacyRefusal(t, err, f.volume, f.vmCID())
			if !s.alive(f.volume) {
				t.Fatal("the persistent volume was freed")
			}
			if _, ok := s.slotValue(f.vmid, "unused0"); !ok {
				t.Error("the unused0 entry must stay, because deleting it frees the volume")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// delete_vm
// ---------------------------------------------------------------------------

func ownedSimDeleteDeps(s *ownerSim, storage string, fastPath bool) handlers.Deps {
	deps := testDepsWithCluster(s.qemuService(), s.nodesService(), &mockTasksService{}, &mockAgentService{}, &mockStorageService{}, ownerSimCluster(s, "pve-node1"))
	deps.Config.DiskStorage = storage
	deps.Config.FastPathDelete = &fastPath
	return deps
}

// TestDeleteVM_RefusesOwnedLegacyDisk pins the refusal on the synchronous
// path and the fast path. The destroy would free a persistent volume named
// for the VM, because PVE counts it as one of the VM's own disks. On the
// synchronous path the refusal comes before the stop, and on the fast path
// before the bosh-deleting stamp and the skiplock destroy.
func TestDeleteVM_RefusesOwnedLegacyDisk(t *testing.T) {
	t.Parallel()
	for _, f := range legacyDiskForms {
		for _, fastPath := range []bool{false, true} {
			name := f.name + "/sync"
			if fastPath {
				name = f.name + "/fast"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				cid := mustEncodeDiskCID(t, f.volume, nil)
				s := newOwnerSim(f.volume, f.systemDisk())
				s.setConfig(f.vmid, ownedLegacyVMConfig(t, f, cid, map[string]any{"scsi1": f.volume + ",size=2G"}))

				_, err := handlers.HandleDeleteVM(ownedSimDeleteDeps(s, f.storage(), fastPath)).Handle(context.Background(), marshalArgs(f.vmCID()), jsonrpc.Context{})

				assertOwnedLegacyRefusal(t, err, f.volume, "scsi1", f.vmCID())
				if s.destroyed(f.vmCID()) {
					t.Error("DeleteQemu must not run")
				}
				if !s.alive(f.volume) {
					t.Fatal("the persistent volume was freed")
				}
				if s.stops != 0 {
					t.Errorf("the refusal must come before the VM is stopped; got %d stops", s.stops)
				}
				if tags, _ := s.slotValue(f.vmid, "tags"); strings.Contains(tags, "bosh-deleting") {
					t.Error("a refused VM must not be queued for the straggler sweep")
				}
			})
		}
	}
}

// TestDeleteVM_OwnedLegacyDiskOnUnusedEntryIsRefused is a control row. An
// owned legacy disk that sits on unused0 with its record in place is not read
// by the owned-disk refusal, which looks at active bus slots only. It is
// refused by guardUnusedVolumes, which every delete path runs and which
// refuses any unused volume that still exists. The row passes before and
// after the fix, and it shows the unused case needs nothing new.
func TestDeleteVM_OwnedLegacyDiskOnUnusedEntryIsRefused(t *testing.T) {
	t.Parallel()
	for _, f := range legacyDiskForms {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			cid := mustEncodeDiskCID(t, f.volume, nil)
			s := newOwnerSim(f.volume, f.systemDisk())
			s.setConfig(f.vmid, ownedLegacyVMConfig(t, f, cid, map[string]any{"unused0": f.volume}))
			storageSvc := &mockStorageService{
				existsFn: func(_ context.Context, _, storage, volume string) (bool, error) {
					return s.alive(volume) || s.alive(storage+":"+volume), nil
				},
			}
			deps := testDepsWithCluster(s.qemuService(), s.nodesService(), &mockTasksService{}, &mockAgentService{}, storageSvc, ownerSimCluster(s, "pve-node1"))
			deps.Config.DiskStorage = f.storage()
			sync := false
			deps.Config.FastPathDelete = &sync

			_, err := handlers.HandleDeleteVM(deps).Handle(context.Background(), marshalArgs(f.vmCID()), jsonrpc.Context{})

			if err == nil || !strings.Contains(err.Error(), "unused0="+f.volume) {
				t.Fatalf("delete_vm must refuse the unused entry; got %v", err)
			}
			if s.destroyed(f.vmCID()) {
				t.Error("DeleteQemu must not run")
			}
			if !s.alive(f.volume) {
				t.Fatal("the persistent volume was freed")
			}
		})
	}
}

// TestDeleteVM_RefusesOwnedDiskWithStableCIDButNoSerial pins that what the
// recorded CID claims does not matter. A drive with no bpd- serial whose
// volume is named for the VM is freed by the destroy whatever identity its
// CID carries, so the delete refuses it rather than trusting the CID.
func TestDeleteVM_RefusesOwnedDiskWithStableCIDButNoSerial(t *testing.T) {
	t.Parallel()
	f := legacyDiskForms[0]
	cid := mustEncodeDiskCID(t, f.volume, &pve.DiskCIDMeta{ID: "bpd-0123456789abcdef"})
	s := newOwnerSim(f.volume, f.systemDisk())
	s.setConfig(f.vmid, ownedLegacyVMConfig(t, f, cid, map[string]any{"scsi1": f.volume + ",size=2G"}))

	_, err := handlers.HandleDeleteVM(ownedSimDeleteDeps(s, f.storage(), false)).Handle(context.Background(), marshalArgs(f.vmCID()), jsonrpc.Context{})

	assertOwnedLegacyRefusal(t, err, f.volume, "scsi1", f.vmCID())
	if s.destroyed(f.vmCID()) {
		t.Error("DeleteQemu must not run")
	}
	if !s.alive(f.volume) {
		t.Fatal("the persistent volume was freed")
	}
}

// TestDeleteVM_StragglerSweepSkipsOwnedLegacyDisk pins the third path: a VM
// already tagged bosh-deleting that still holds an owned legacy disk is left
// for the operator, and the current delete goes ahead.
func TestDeleteVM_StragglerSweepSkipsOwnedLegacyDisk(t *testing.T) {
	t.Parallel()
	for _, f := range legacyDiskForms {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			cid := mustEncodeDiskCID(t, f.volume, nil)
			s := newOwnerSim(f.volume, f.systemDisk(), "local-lvm:vm-710-disk-0")
			s.setConfig(f.vmid, ownedLegacyVMConfig(t, f, cid, map[string]any{
				"scsi1": f.volume + ",size=2G",
				"tags":  "bosh-deleting",
			}))
			s.setConfig(710, map[string]any{"scsi0": "local-lvm:vm-710-disk-0"})

			_, err := handlers.HandleDeleteVM(ownedSimDeleteDeps(s, f.storage(), true)).Handle(context.Background(), marshalArgs("710"), jsonrpc.Context{})
			if err != nil {
				t.Fatalf("the current delete must go ahead: %v", err)
			}
			if !s.destroyed("710") {
				t.Error("the current VM must be destroyed")
			}
			if s.destroyed(f.vmCID()) {
				t.Error("the straggler holding an owned legacy disk must not be destroyed")
			}
			if !s.alive(f.volume) {
				t.Fatal("the persistent volume was freed")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// A legacy disk named for a different VMID behaves exactly as before.
// ---------------------------------------------------------------------------

// TestLegacyDisk_ForeignVMIDStillAttachesDetachesAndSurvivesDeleteVM walks one
// legacy disk through attach, detach, a second attach, and delete_vm on a VM
// whose VMID differs from the one in the volume name, in both volume forms.
func TestLegacyDisk_ForeignVMIDStillAttachesDetachesAndSurvivesDeleteVM(t *testing.T) {
	t.Parallel()
	for _, f := range legacyDiskForms {
		t.Run(f.name, func(t *testing.T) {
			t.Parallel()
			const vmid = 100
			cid := mustEncodeDiskCID(t, f.volume, nil)
			s := newOwnerSim(f.volume, "local-lvm:vm-100-disk-0")
			s.setConfig(vmid, map[string]any{"scsi0": "local-lvm:vm-100-disk-0,size=10G"})
			attachDeps := handlers.Deps{
				Config: &config.CPIConfig{Node: "pve-node1", VMDiskFormat: "qcow2", DiskStorage: f.storage(), DetachedDiskStrategy: "free"},
				PVE:    &mockPVEClient{qemuSvc: s.qemuService(), nodesSvc: s.nodesService(), clusterSvc: ownerSimCluster(s, "pve-node1")},
				Agent:  &captureAgent{},
				Logger: log.NewNopLogger(),
			}
			attach := handlers.HandleAttachDisk(attachDeps)
			detach := handlers.HandleDetachDisk(attachDeps)

			if _, err := attach.Handle(context.Background(), marshalArgs("100", cid), jsonrpc.Context{}); err != nil {
				t.Fatalf("attach: %v", err)
			}
			if v, ok := s.slotValue(vmid, "scsi1"); !ok || ownerSimBare(v) != f.volume {
				t.Fatalf("attach must land the disk on scsi1; got %q (present=%t)", v, ok)
			}
			if _, err := detach.Handle(context.Background(), marshalArgs("100", cid), jsonrpc.Context{}); err != nil {
				t.Fatalf("detach: %v", err)
			}
			if _, ok := s.slotValue(vmid, "scsi1"); ok {
				t.Fatal("detach must take the disk off scsi1")
			}
			if !s.alive(f.volume) {
				t.Fatal("detach freed a volume the VM does not own")
			}
			if _, err := attach.Handle(context.Background(), marshalArgs("100", cid), jsonrpc.Context{}); err != nil {
				t.Fatalf("second attach: %v", err)
			}

			if _, err := handlers.HandleDeleteVM(ownedSimDeleteDeps(s, f.storage(), false)).Handle(context.Background(), marshalArgs("100"), jsonrpc.Context{}); err != nil {
				t.Fatalf("delete_vm: %v", err)
			}
			if !s.destroyed("100") {
				t.Error("delete_vm must destroy the VM")
			}
			if !s.alive(f.volume) {
				t.Fatal("delete_vm freed the legacy persistent volume")
			}
			if s.alive("local-lvm:vm-100-disk-0") {
				t.Error("delete_vm must still free the VM's own system disk")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// create_vm's plain allocator
// ---------------------------------------------------------------------------

// ownedVolumeCreateVMDeps narrows the VM band to [9005, 9006], places a live
// VM on 9006, and puts a volume named for 9005 on the "data" pool, which is
// not vm_storage. The only VMID the band offers is the one that names a disk.
func ownedVolumeCreateVMDeps(q *vmMockQEMU, n *vmMockNodes) handlers.Deps {
	c := &vmMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			raw, _ := json.Marshal(map[string]any{"vmid": 9006, "node": "pve", "type": "qemu"})
			resp := sdkcluster.ListResourcesResponse{raw}
			return &resp, nil
		},
	}
	deps := buildVMDeps(q, n, c, &vmMockAgent{})
	deps.Config.VMIDRangeStart, deps.Config.VMIDRangeEnd = 9005, 9006
	deps.Config.DiskStorage = "data"
	return deps
}

func imagesStorageListing(names ...string) *nodes.ListStorageResponse {
	resp := make(nodes.ListStorageResponse, 0, len(names))
	for _, name := range names {
		raw, _ := json.Marshal(map[string]any{"storage": name, "content": "images,rootdir", "enabled": 1, "active": 1})
		resp = append(resp, raw)
	}
	return &resp
}

func isImagesListing(params *nodes.ListStorageParams) bool {
	return params != nil && params.Content != nil && *params.Content == "images"
}

func ownedVolumeContent(storage string, params *nodes.ListStorageContentParams) *nodes.ListStorageContentResponse {
	resp := nodes.ListStorageContentResponse{}
	if params != nil && params.Content != nil && *params.Content == "import" {
		raw, _ := json.Marshal(map[string]string{"volid": storage + ":import/" + testCreateVMStemcellFilename})
		return &nodes.ListStorageContentResponse{raw}
	}
	if storage == "data" {
		raw, _ := json.Marshal(map[string]string{"volid": ownedLegacyVolume})
		resp = append(resp, raw)
	}
	return &resp
}

func ownedVolumeCreateVMArgs(agentID string) []json.RawMessage {
	return mkArgs(agentID, testStemcellCID, map[string]any{"cores": 1, "memory": 512}, defaultNetMap(), []string{}, map[string]any{})
}

// TestCreateVM_SkipsVMIDNamingVolumeOnOtherImagesStorage pins the root fix on
// the plain path: a VMID that names a volume on any images storage the target
// node sees is never handed to a new VM, even when that storage is not
// vm_storage.
func TestCreateVM_SkipsVMIDNamingVolumeOnOtherImagesStorage(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{
		listStorageFn: func(_ context.Context, _ string, _ *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
			return imagesStorageListing(storageName, "data"), nil
		},
		listStorageContentFn: func(_ context.Context, _, storage string, params *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
			return ownedVolumeContent(storage, params), nil
		},
	}

	result, err := handlers.HandleCreateVM(ownedVolumeCreateVMDeps(q, n)).Handle(context.Background(), ownedVolumeCreateVMArgs("agent-owned-volume"), mkCtx("owned-volume"))
	if err == nil {
		t.Fatalf("VMID 9005 names %s and must not be allocated; create_vm returned %v", ownedLegacyVolume, result)
	}
	if !strings.Contains(err.Error(), "no free VMID") {
		t.Errorf("want the band-exhausted refusal, got %v", err)
	}
	if len(q.createCalls) != 0 {
		t.Errorf("no VM may be created; got %d QEMU.Create calls", len(q.createCalls))
	}
}

// TestCreateVM_ImagesStorageListingFailureFailsAllocation pins fail-closed on
// both reads the root fix adds: the node's storage list and each storage's
// content list.
func TestCreateVM_ImagesStorageListingFailureFailsAllocation(t *testing.T) {
	t.Parallel()
	cases := map[string]*vmMockNodes{
		"storage list": {
			listStorageFn: func(_ context.Context, _ string, params *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
				if isImagesListing(params) {
					return nil, errors.New("storage list unavailable")
				}
				return imagesStorageListing(storageName), nil
			},
		},
		"content list": {
			listStorageFn: func(_ context.Context, _ string, _ *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
				return imagesStorageListing(storageName, "data"), nil
			},
			listStorageContentFn: func(_ context.Context, _, storage string, params *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
				if storage == "data" && params == nil {
					return nil, errors.New("content list unavailable")
				}
				return ownedVolumeContent(storage, params), nil
			},
		},
	}
	for name, n := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q := &vmMockQEMU{}
			deps := buildVMDeps(q, n, &vmMockCluster{}, &vmMockAgent{})
			deps.Config.DiskStorage = "data"
			_, err := handlers.HandleCreateVM(deps).Handle(context.Background(), ownedVolumeCreateVMArgs("agent-listing-"+strings.ReplaceAll(name, " ", "-")), mkCtx("listing-failure"))
			if err == nil {
				t.Fatal("a failed listing must fail the allocation")
			}
			if len(q.createCalls) != 0 {
				t.Errorf("no VM may be created blind; got %d QEMU.Create calls", len(q.createCalls))
			}
			if cpierrors.IsType(err, cpierrors.TypeVMNotFound) {
				t.Errorf("unexpected error class: %v", err)
			}
		})
	}
}

// TestCreateVM_RecreateDrawsVMIDThatNamesNoVolume backs attach_disk's remedy.
// When the Director recreates the VM, create_vm draws a VMID from the same
// band, and with the disk's storage visible on the node it never draws the
// VMID the disk is named for. Every draw here lands on 9006.
func TestCreateVM_RecreateDrawsVMIDThatNamesNoVolume(t *testing.T) {
	t.Parallel()
	q := &vmMockQEMU{}
	n := &vmMockNodes{
		listStorageFn: func(_ context.Context, _ string, _ *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
			return imagesStorageListing(storageName, "data"), nil
		},
		listStorageContentFn: func(_ context.Context, _, storage string, params *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
			return ownedVolumeContent(storage, params), nil
		},
	}
	deps := buildVMDeps(q, n, &vmMockCluster{}, &vmMockAgent{})
	deps.Config.VMIDRangeStart, deps.Config.VMIDRangeEnd = 9005, 9006
	deps.Config.DiskStorage = "data"
	h := handlers.HandleCreateVM(deps)

	for i := range 5 {
		result, err := h.Handle(context.Background(), ownedVolumeCreateVMArgs(fmt.Sprintf("agent-recreate-%d", i)), mkCtx("recreate"))
		if err != nil {
			t.Fatalf("draw %d: %v", i, err)
		}
		if got := result.([]any)[0]; got != "9006" {
			t.Fatalf("draw %d = %v, want 9006: VMID 9005 names %s", i, got, ownedLegacyVolume)
		}
	}
}

// TestCreateVM_RefusesOwnedLegacyDiskOnInactiveStorage backs create_vm's
// remedy. The disk's storage is not active on the node, so the allocator
// cannot see the volume and draws VMID 9005, the one the disk is named for.
// The attach is refused, nothing is attached, and the new VM is rolled back.
// A retry while the storage stays inactive draws 9005 again and is refused
// again. Once the storage is active, a retry never draws 9005.
func TestCreateVM_RefusesOwnedLegacyDiskOnInactiveStorage(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	dataActive := false
	attaches := 0
	q := &vmMockQEMU{
		attachDiskFn: func(context.Context, string, int, string, string, *qemu.AttachOpts) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			attaches++
			return "scsi1", nil
		},
	}
	n := &vmMockNodes{
		listStorageFn: func(_ context.Context, _ string, _ *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			resp := make(nodes.ListStorageResponse, 0, 2)
			for name, active := range map[string]int{storageName: 1, "data": map[bool]int{false: 0, true: 1}[dataActive]} {
				raw, _ := json.Marshal(map[string]any{"storage": name, "content": "images", "enabled": 1, "active": active})
				resp = append(resp, raw)
			}
			return &resp, nil
		},
		listStorageContentFn: func(_ context.Context, _, storage string, params *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
			return ownedVolumeContent(storage, params), nil
		},
	}
	deps := ownedVolumeCreateVMDeps(q, n)
	h := handlers.HandleCreateVM(deps)
	cid := mustEncodeDiskCID(t, ownedLegacyVolume, nil)
	args := func(i int) []json.RawMessage {
		return mkArgs(fmt.Sprintf("agent-inactive-%d", i), testStemcellCID, map[string]any{"cores": 1, "memory": 512}, defaultNetMap(), []string{cid}, map[string]any{})
	}

	for i := range 2 {
		_, err := h.Handle(context.Background(), args(i), mkCtx("inactive"))
		assertOwnedLegacyRefusal(t, err, wantCreateVMRefusal(cid, ownedLegacyVolume, ownedLegacyVMID, "pve"))
		mu.Lock()
		got := attaches
		mu.Unlock()
		if got != 0 {
			t.Fatalf("attempt %d: the disk must not be attached; got %d attaches", i, got)
		}
		deleted := false
		for _, call := range n.deleteQemuCalls {
			deleted = deleted || call.vmid == "9005"
		}
		if !deleted {
			t.Fatalf("attempt %d: the new VM 9005 must be rolled back", i)
		}
	}

	mu.Lock()
	dataActive = true
	mu.Unlock()
	creates := len(q.createCalls)
	_, err := h.Handle(context.Background(), args(2), mkCtx("inactive"))
	if err == nil || !strings.Contains(err.Error(), "no free VMID") {
		t.Fatalf("with the storage active the retry must not draw 9005; got %v", err)
	}
	if len(q.createCalls) != creates {
		t.Errorf("no VM may be created once 9005 is ruled out; got %d more creates", len(q.createCalls)-creates)
	}
}

// wantCreateVMRefusal is the whole create_vm refusal text. The op prefix is
// what picks it over attach_disk's, so the tests pin every word.
func wantCreateVMRefusal(cid, volume string, vmid int, node string) string {
	return fmt.Sprintf("create_vm: refusing to attach disk %s to the new VM %d. Its volume %s is named for VMID %d, so PVE would "+
		"count it as one of the VM's own disks and free it on the next detach or delete. Nothing was "+
		"attached, and create_vm rolls the new VM back, or keeps it tagged when pve.debug.keep_failed_vms is set. "+
		"The VMID allocator could not see this volume when it drew VMID %d, so its storage was not active "+
		"on node %s or is local to another node, and a retry can draw the same VMID again. Make that "+
		"storage active on node %s, or set cloud_properties.target_node to the node whose local storage holds "+
		"the disk, and then retry", cid, vmid, volume, vmid, vmid, node, node)
}

// ownedVolumeRetryNodes serves a node storage list in which "data" is listed
// only on the nodes visible reports, and a content list that holds
// data:vm-9005-disk-0 on "data".
func ownedVolumeRetryNodes(visible func(node string) bool) *vmMockNodes {
	return &vmMockNodes{
		listStorageFn: func(_ context.Context, node string, _ *nodes.ListStorageParams) (*nodes.ListStorageResponse, error) {
			rows := map[string]int{storageName: 1}
			if visible(node) {
				rows["data"] = 1
			} else {
				rows["data"] = 0
			}
			resp := make(nodes.ListStorageResponse, 0, len(rows))
			for name, active := range rows {
				raw, _ := json.Marshal(map[string]any{"storage": name, "content": "images", "enabled": 1, "active": active})
				resp = append(resp, raw)
			}
			return &resp, nil
		},
		listStorageContentFn: func(_ context.Context, _, storage string, params *nodes.ListStorageContentParams) (*nodes.ListStorageContentResponse, error) {
			return ownedVolumeContent(storage, params), nil
		},
	}
}

// ownedVolumeRetryQEMU records which VM each attach of the legacy disk went to.
func ownedVolumeRetryQEMU(mu *sync.Mutex, attachedTo *[]int) *vmMockQEMU {
	return &vmMockQEMU{
		attachDiskFn: func(_ context.Context, _ string, vmid int, volid, _ string, _ *qemu.AttachOpts) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			if strings.HasPrefix(volid, ownedLegacyVolume) {
				*attachedTo = append(*attachedTo, vmid)
			}
			return "scsi1", nil
		},
	}
}

// TestCreateVM_RetryAfterStorageActivationDrawsAnotherVMID is the wider-band
// row for create_vm's remedy. The band is [9005, 9007] and 9006 is taken.
// While the disk's storage is inactive, the allocator cannot rule out 9005,
// so a plain retry draws it again and is refused again. Once the storage is
// active, the retry draws 9007 and the attach goes ahead.
//
// 9007 is held by a live VM during the inactive phase so that every draw
// lands on the refused VMID 9005. Without it the random start offset would
// pick 9005 or 9007, and the row could pass without ever showing a refused
// retry. Keep it held.
func TestCreateVM_RetryAfterStorageActivationDrawsAnotherVMID(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	active := false
	var attachedTo []int
	q := ownedVolumeRetryQEMU(&mu, &attachedTo)
	n := ownedVolumeRetryNodes(func(string) bool { mu.Lock(); defer mu.Unlock(); return active })
	c := &vmMockCluster{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			taken := []int{9006}
			if !active {
				taken = append(taken, 9007)
			}
			resp := make(sdkcluster.ListResourcesResponse, 0, len(taken))
			for _, vmid := range taken {
				raw, _ := json.Marshal(map[string]any{"vmid": vmid, "node": "pve", "type": "qemu"})
				resp = append(resp, raw)
			}
			return &resp, nil
		},
	}
	deps := buildVMDeps(q, n, c, &vmMockAgent{})
	deps.Config.VMIDRangeStart, deps.Config.VMIDRangeEnd = 9005, 9007
	deps.Config.DiskStorage = "data"
	h := handlers.HandleCreateVM(deps)
	cid := mustEncodeDiskCID(t, ownedLegacyVolume, nil)
	args := func(i int) []json.RawMessage {
		return mkArgs(fmt.Sprintf("agent-wide-%d", i), testStemcellCID, map[string]any{"cores": 1, "memory": 512}, defaultNetMap(), []string{cid}, map[string]any{})
	}

	// Inactive: the only free VMID is 9005, so both attempts draw it, and
	// both are refused with nothing attached and the new VM rolled back.
	for i := range 2 {
		_, err := h.Handle(context.Background(), args(i), mkCtx("wide-inactive"))
		assertOwnedLegacyRefusal(t, err, wantCreateVMRefusal(cid, ownedLegacyVolume, ownedLegacyVMID, "pve"))
		mu.Lock()
		if len(attachedTo) != 0 {
			t.Fatalf("attempt %d: the disk must not be attached; got attaches to %v", i, attachedTo)
		}
		mu.Unlock()
		deleted := false
		for _, call := range n.deleteQemuCalls {
			deleted = deleted || call.vmid == "9005"
		}
		if !deleted {
			t.Fatalf("attempt %d: the new VM 9005 must be rolled back", i)
		}
	}

	// Active: 9007 is free again and 9005 is ruled out, so the retry draws
	// 9007 exactly and the attach goes ahead there.
	mu.Lock()
	active = true
	mu.Unlock()
	result, err := h.Handle(context.Background(), args(2), mkCtx("wide-active"))
	if err != nil {
		t.Fatalf("retry after activation: %v", err)
	}
	if got := result.([]any)[0]; got != "9007" {
		t.Fatalf("retry after activation drew %v, want 9007", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(attachedTo) != 1 || attachedTo[0] != 9007 {
		t.Fatalf("the retry must attach the disk to VM 9007 once; got attaches to %v", attachedTo)
	}
}

// TestCreateVM_DiskLocalToAnotherNode is the other-node-local row. The disk
// sits on storage local to pve2, and its CID carries no node, so placement
// does not move the VM to pve2. On pve the allocator cannot see the volume,
// draws 9005, and the attach is refused. With cloud_properties.target_node
// set to pve2, the draw reads pve2's storage and skips 9005.
func TestCreateVM_DiskLocalToAnotherNode(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var attachedTo []int
	q := ownedVolumeRetryQEMU(&mu, &attachedTo)
	n := ownedVolumeRetryNodes(func(node string) bool { return node == "pve2" })
	deps := ownedVolumeCreateVMDeps(q, n)
	h := handlers.HandleCreateVM(deps)
	cid := mustEncodeDiskCID(t, ownedLegacyVolume, nil)
	cloudProps := map[string]any{"cores": 1, "memory": 512}

	_, err := h.Handle(context.Background(), mkArgs("agent-local-pve", testStemcellCID, cloudProps, defaultNetMap(), []string{cid}, map[string]any{}), mkCtx("local-pve"))
	assertOwnedLegacyRefusal(t, err, wantCreateVMRefusal(cid, ownedLegacyVolume, ownedLegacyVMID, "pve"))

	pinned := map[string]any{"cores": 1, "memory": 512, "target_node": "pve2"}
	_, err = h.Handle(context.Background(), mkArgs("agent-local-pve2", testStemcellCID, pinned, defaultNetMap(), []string{cid}, map[string]any{}), mkCtx("local-pve2"))
	if err == nil || !strings.Contains(err.Error(), "no free VMID") {
		t.Fatalf("on pve2 the draw must skip 9005, and 9006 is taken; got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(attachedTo) != 0 {
		t.Fatalf("the disk must not be attached anywhere; got attaches to %v", attachedTo)
	}
}
