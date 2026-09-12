package handlers

import (
	"context"
	"fmt"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
)

type namedEphemeralClient struct {
	*ephemeralClient
	backend string
}

func (c *namedEphemeralClient) ClusterStorage() clusterstorage.Service {
	backend := c.backend
	if backend == "" {
		backend = "nfs"
	}
	return &shapeTestClusterStorage{entries: []map[string]any{{"storage": "e", "type": backend}}}
}

type namedEphemeralStorage struct {
	storage.Service
	replay bool
	block  bool
	calls  int
	probes []string
}

func (s *namedEphemeralStorage) CreateVolume(_ context.Context, _, pool string, _ int, format string, vmid int, name string) (string, error) {
	s.calls++
	expectedFormat, expectedName := "qcow2", "vm-123-ephemeral-0.qcow2"
	if s.block {
		expectedFormat, expectedName = "raw", "vm-123-ephemeral-0"
	}
	if pool != "e" || vmid != 123 || format != expectedFormat || name != expectedName {
		return "", fmt.Errorf("file plugin filename or format rejected")
	}
	if s.replay {
		if s.calls == 1 {
			return "", fmt.Errorf("cfs-lock 'storage-e' error: got lock request timeout")
		}
		return "", fmt.Errorf("volume already exists")
	}
	if s.block {
		return "e:" + name, nil
	}
	return "e:123/" + name, nil
}
func (s *namedEphemeralStorage) Exists(_ context.Context, _, _, volume string) (bool, error) {
	s.probes = append(s.probes, volume)
	return volume == "e:123/vm-123-ephemeral-0.qcow2", nil
}

func TestLegacyEphemeralFileNameAndReplayUseOwnerDirectory(t *testing.T) {
	for _, replay := range []bool{false, true} {
		t.Run(fmt.Sprintf("replay=%t", replay), func(t *testing.T) {
			store := &namedEphemeralStorage{replay: replay}
			guest := &ephemeralQEMU{configFn: func() (map[string]any, error) { return map[string]any{}, nil }, attachDiskFn: func(volume, _ string, _ *qemu.AttachOpts) (string, error) {
				if volume != "e:123/vm-123-ephemeral-0.qcow2" {
					t.Fatalf("attach changed canonical file identity: %q", volume)
				}
				return "scsi1", nil
			}}
			deps := Deps{PVE: &namedEphemeralClient{ephemeralClient: &ephemeralClient{qemu: guest, storage: store}}, Logger: log.NewNopLogger()}
			shape := &createVMShape{node: "n1", ephemeralStorage: "e", ephemeralDiskGiB: 1, vmDiskFormat: "qcow2"}
			if _, err := attachEphemeralDisk(t.Context(), deps, deps.Logger, shape, 123); err != nil {
				t.Fatal(err)
			}
			if replay && (store.calls != 2 || len(store.probes) != 1 || store.probes[0] != "e:123/vm-123-ephemeral-0.qcow2") {
				t.Fatalf("replay did not probe exact file identity: calls=%d probes=%v", store.calls, store.probes)
			}
		})
	}
}

func TestManagedEphemeralExistingBirthNameRefusesSubmission(t *testing.T) {
	for _, size := range []uint64{2 << 30, 9 << 30} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			m, fixture, _, _ := newManagedVMGuardCase(t, managedVMGuardCase{ephemeral: true})
			target, _ := managedVMRoleTarget(m.prepared.plan, storageRoleEphemeral)
			_, suffix, err := pve.ManagedEphemeralVolumeName(m.prepared.plan.Definitions[target.StorageID].Type, m.shape.vmDiskFormat, 101, m.handle.Record().Namespace, m.handle.Record().ID)
			if err != nil {
				t.Fatal(err)
			}
			foreign := target.StorageID + ":" + suffix
			fixture.volumes = map[string]uint64{foreign: size}
			guarded := m.deps
			guarded.PVE = m.guard.Client()
			if err := createManagedVMRoot(t.Context(), guarded, m.parsed, m.shape, m.prepared.plan.Targets[0], 101, m.marker); err != nil {
				t.Fatal(err)
			}
			if _, err := attachCreatedVMEphemeral(t.Context(), guarded, m.deps.Logger, m.parsed, m.shape, 101); err == nil {
				t.Fatal("preexisting same-name foreign volume was accepted")
			}
			record := m.handle.Record()
			for _, step := range record.Steps {
				if step.Kind == managedVMStepCreateVolume {
					t.Fatal("collision was journaled as a submitted birth")
				}
			}
			if fixture.volumes[foreign] != size || fixture.allocations != 0 {
				t.Fatal("preexisting collision was submitted or changed")
			}

		})
	}
}

func TestLegacyEphemeralBlockCompanionDoesNotInheritRootQCOW2(t *testing.T) {
	store := &namedEphemeralStorage{block: true}
	guest := &ephemeralQEMU{configFn: func() (map[string]any, error) { return map[string]any{}, nil }, attachDiskFn: func(volume, _ string, _ *qemu.AttachOpts) (string, error) {
		if volume != "e:vm-123-ephemeral-0" {
			t.Fatalf("block companion acquired a file identity: %q", volume)
		}
		return "scsi1", nil
	}}
	deps := Deps{PVE: &namedEphemeralClient{ephemeralClient: &ephemeralClient{qemu: guest, storage: store}, backend: "lvmthin"}, Logger: log.NewNopLogger()}
	shape := &createVMShape{node: "n1", vmStorage: "root-nfs", vmStorageType: "nfs", ephemeralStorage: "e", ephemeralDiskGiB: 1, vmDiskFormat: "qcow2"}
	if _, err := attachEphemeralDisk(t.Context(), deps, deps.Logger, shape, 123); err != nil {
		t.Fatal(err)
	}
	if shape.vmDiskFormat != "qcow2" || store.calls != 1 {
		t.Fatal("ephemeral format resolution changed root policy or retried")
	}
}

func TestLegacyEphemeralFailedCreateSweepsCanonicalFileVolume(t *testing.T) {
	const canonical = "e:123/vm-123-ephemeral-0.qcow2"
	store := &ephemeralStorageSvc{
		createVolumeFn: func(int) (string, error) { return "", fmt.Errorf("allocation request failed") },
		existsFn:       func(volume string) (bool, error) { return volume == canonical, nil },
	}
	deps := Deps{PVE: &namedEphemeralClient{ephemeralClient: &ephemeralClient{storage: store}}, Logger: log.NewNopLogger()}
	shape := &createVMShape{node: "n1", ephemeralStorage: "e", ephemeralDiskGiB: 1, vmDiskFormat: "qcow2"}
	if _, err := attachEphemeralDisk(t.Context(), deps, deps.Logger, shape, 123); err == nil {
		t.Fatal("failed allocation reported success")
	}
	if store.createVolumeCalls != 1 || len(store.existsCalls) != 1 || store.existsCalls[0] != canonical || len(store.deleteAsyncCalls) != 1 || store.deleteAsyncCalls[0].volume != canonical {
		t.Fatalf("file cleanup used an unqualified filename: creates=%d probes=%v deletes=%v", store.createVolumeCalls, store.existsCalls, store.deleteAsyncCalls)
	}
}
