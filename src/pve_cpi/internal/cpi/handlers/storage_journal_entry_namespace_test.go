package handlers_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/cpi/handlers"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/jsonrpc"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

// entryNamespaceRefusal is the start of the text the journal gates give a
// cpi-config entry that sets a namespace over a job with no journal directory.
const entryNamespaceRefusal = "pve_storage_placement_namespace \"east\" is set by the cpi-config entry, but the job " +
	"sets no pve.storage_allocation_journal_dir, which only the job can set"

// throughNamespaceEntry routes every call on deps through a cpi-config entry
// that sets pve_storage_placement_namespace, over a job that sets no journal
// directory. The job keeps the fixture's node and storage names and gains the
// connection fields config validation needs. The entry's client is the
// fixture's own client, and the returned counter says how many times the
// override built it, which proves the entry applied.
func throughNamespaceEntry(t *testing.T, deps handlers.Deps, requestID string) (
	handlers.Deps, jsonrpc.Context, *atomic.Int32,
) {
	t.Helper()
	job := *deps.Config
	base := overrideTestBaseConfig()
	job.Host, job.Port, job.User, job.Password, job.APIToken = base.Host, base.Port, base.User, base.Password, ""
	job.VerifySSL = base.VerifySSL
	fill := func(field *string, value string) {
		if *field == "" {
			*field = value
		}
	}
	fill(&job.VMStorage, base.VMStorage)
	fill(&job.DiskStorage, base.DiskStorage)
	fill(&job.NetworkBridge, base.NetworkBridge)
	fill(&job.AgentMode, base.AgentMode)
	fill(&job.VMDiskFormat, base.VMDiskFormat)
	fill(&job.LogLevel, base.LogLevel)
	fill(&job.RebootMode, base.RebootMode)
	fill(&job.NetworkMode, base.NetworkMode)
	fill(&job.SDNZoneType, base.SDNZoneType)
	if job.VMIDRangeEnd == 0 {
		job.VMIDRangeStart, job.VMIDRangeEnd = base.VMIDRangeStart, base.VMIDRangeEnd
	}
	if job.RebootTimeout == 0 {
		job.RebootTimeout = base.RebootTimeout
	}
	job.StoragePlacementNamespace, job.StorageAllocationJournalDir = "", ""
	if err := job.Validate(); err != nil {
		t.Fatalf("the job configuration is not valid: %v", err)
	}
	client := deps.PVE
	built := &atomic.Int32{}
	deps.Config = &job
	deps.Overrides = &handlers.RequestOverrideRuntime{
		Job: &job,
		ClientFactory: func(_, _ *config.CPIConfig, _ *log.Logger) (pve.Client, error) {
			built.Add(1)
			return client, nil
		},
		Logger: log.NewNopLogger(),
	}
	reqCtx := jsonrpc.Context{
		RequestID: requestID,
		Extra:     map[string]any{"pve_storage_placement_namespace": "east"},
	}
	return deps, reqCtx, built
}

// assertEntryApplied checks that the call went through the entry's override.
func assertEntryApplied(t *testing.T, built *atomic.Int32) {
	t.Helper()
	if built.Load() == 0 {
		t.Fatal("the cpi-config entry's override never applied")
	}
}

// TestNamespaceEntryWithoutJobJournalDirectory sends calls through a
// cpi-config entry that sets a namespace over a job with no journal
// directory. Config load does not check an entry's effective configuration,
// so the entry applies. has_vm, set_vm_metadata, attach_disk of a disk the
// journal never allocated, and create_stemcell answer from PVE as they would
// without the namespace, so stemcell uploads, scans, and existing VMs keep
// working. create_vm and delete_vm still refuse at the journal gates, with
// text that names the entry, and neither creates nor deletes a VM.
func TestNamespaceEntryWithoutJobJournalDirectory(t *testing.T) {
	t.Run("has_vm answers", func(t *testing.T) {
		deps, reqCtx, built := throughNamespaceEntry(t,
			testDepsFoundVM(101, nil, nil, nil, &mockAgentService{}), "entry-has-vm")
		result, err := handlers.HandleHasVM(deps).Handle(context.Background(), marshalArgs("101"), reqCtx)
		if err != nil || result != true {
			t.Fatalf("has_vm = %v, %v, want true", result, err)
		}
		assertEntryApplied(t, built)
	})

	t.Run("set_vm_metadata writes", func(t *testing.T) {
		var description string
		nodesSvc := &mockNodesService{
			updateQemuConfigFn: func(_ context.Context, _, _ string, params *nodes.UpdateQemuConfigParams) error {
				if params.Description != nil {
					description = *params.Description
				}
				return nil
			},
		}
		deps, reqCtx, built := throughNamespaceEntry(t,
			testDepsFoundVM(101, nil, nodesSvc, nil, &mockAgentService{}), "entry-set-vm-metadata")
		args := marshalArgs("101", map[string]any{"deployment": "cf", "job": "router"})
		if _, err := handlers.HandleSetVMMetadata(deps).Handle(context.Background(), args, reqCtx); err != nil {
			t.Fatalf("set_vm_metadata error = %v, want nil", err)
		}
		if !strings.Contains(description, "deployment: cf") {
			t.Fatalf("set_vm_metadata wrote description %q, want the deployment", description)
		}
		assertEntryApplied(t, built)
	})

	t.Run("set_vm_metadata read failure is retriable", func(t *testing.T) {
		var writes atomic.Int32
		qemuSvc := &mockQEMUService{
			configFn: func(context.Context, string, int) (map[string]any, error) {
				return nil, errors.New("500 internal server error")
			},
		}
		nodesSvc := &mockNodesService{
			updateQemuConfigFn: func(context.Context, string, string, *nodes.UpdateQemuConfigParams) error {
				writes.Add(1)
				return nil
			},
		}
		deps, reqCtx, built := throughNamespaceEntry(t,
			testDepsFoundVM(101, qemuSvc, nodesSvc, nil, &mockAgentService{}), "entry-set-vm-metadata-read")
		args := marshalArgs("101", map[string]any{"deployment": "cf", "job": "router"})
		_, err := handlers.HandleSetVMMetadata(deps).Handle(context.Background(), args, reqCtx)
		requireHARetriable(t, err, "read VM 101 config")
		if writes.Load() != 0 {
			t.Fatal("set_vm_metadata wrote after a failed config read")
		}
		assertEntryApplied(t, built)
	})

	t.Run("attach_disk attaches a disk the journal never allocated", func(t *testing.T) {
		qemuSvc := &attachQEMUService{
			attachReturnDiskID: diskSlot,
			configCfg:          map[string]any{"scsi0": testDiskCID, diskSlot: "vm-9001-disk-0"},
		}
		fixture := attachDeps(qemuSvc, &captureAgent{})
		// The entry's override builds its own backend resolver, which reads
		// the storage's type from PVE, so the cluster reports the disk's
		// storage as shared.
		fixture.PVE.(*mockPVEClient).clusterStorageSvc = &mockClusterStorage{
			storageName: "local-lvm", storageType: "lvm", shared: true,
		}
		deps, reqCtx, built := throughNamespaceEntry(t, fixture, "entry-attach")
		result, err := handlers.HandleAttachDisk(deps).Handle(context.Background(), attachArgs(t, "100", diskCID),
			reqCtx)
		if err != nil {
			t.Fatalf("attach_disk error = %v, want nil", err)
		}
		if path := extractPath(t, result); path != "/dev/disk/by-id/scsi-0QEMU_QEMU_HARDDISK_drive-"+diskSlot {
			t.Fatalf("attach_disk disk_hints path = %q", path)
		}
		assertEntryApplied(t, built)
	})

	t.Run("create_stemcell uploads", func(t *testing.T) {
		var uploads atomic.Int32
		client := buildStemcellClient(&stemcellMockQEMU{}, &stemcellMockNodes{}, &stemcellMockTasks{},
			&stemcellMockCluster{})
		client.storageSvc = &stemcellMockStorage{
			uploadFn: func(_ context.Context, _, _, _, _ string, body io.Reader) (string, error) {
				_, _ = io.Copy(io.Discard, body)
				uploads.Add(1)
				return "", nil
			},
		}
		deps, reqCtx, built := throughNamespaceEntry(t, makeDeps(client), "entry-create-stemcell")
		cp := map[string]any{"name": "ubuntu-jammy", "version": "1.234", "disk_format": "qcow2"}
		args := []json.RawMessage{marshalArg(t, tempImageFile(t)), marshalArg(t, cp)}
		result, err := handlers.HandleCreateStemcell(deps).Handle(context.Background(), args, reqCtx)
		if err != nil {
			t.Fatalf("create_stemcell error = %v, want nil", err)
		}
		if cid, ok := result.(string); !ok || cid == "" || uploads.Load() != 1 {
			t.Fatalf("create_stemcell = %v after %d uploads, want a CID after one", result, uploads.Load())
		}
		assertEntryApplied(t, built)
	})

	t.Run("create_vm refuses", func(t *testing.T) {
		q := &vmMockQEMU{}
		deps, reqCtx, built := throughNamespaceEntry(t, journalSetupVMDeps(q), "entry-create-vm")
		args := mkArgs("agent-entry", testStemcellCID, map[string]any{"cores": 1, "memory": 512},
			defaultNetMap(), []string{}, map[string]any{})
		_, err := handlers.HandleCreateVM(deps).Handle(context.Background(), args, reqCtx)
		assertCreateRefusal(t, err, q, []string{entryNamespaceRefusal,
			"or remove pve_storage_placement_namespace from the cpi-config entry"})
		assertEntryApplied(t, built)
	})

	t.Run("delete_vm refuses", func(t *testing.T) {
		var deletes atomic.Int32
		nodesSvc := &mockNodesService{
			deleteQemuFn: func(context.Context, string, string, *nodes.DeleteQemuParams) (*nodes.DeleteQemuResponse,
				error,
			) {
				deletes.Add(1)
				return nil, nil
			},
		}
		deps, reqCtx, built := throughNamespaceEntry(t,
			testDepsFoundVM(101, nil, nodesSvc, nil, &mockAgentService{}), "entry-delete-vm")
		_, err := handlers.HandleDeleteVM(deps).Handle(context.Background(), marshalArgs("101"), reqCtx)
		if err == nil || !strings.Contains(err.Error(), entryNamespaceRefusal) ||
			!strings.Contains(err.Error(), "or remove pve_storage_placement_namespace from the cpi-config entry") {
			t.Fatalf("delete_vm error = %v, want the entry refusal", err)
		}
		if deletes.Load() != 0 {
			t.Fatal("delete_vm deleted the VM")
		}
		assertEntryApplied(t, built)
	})
}
