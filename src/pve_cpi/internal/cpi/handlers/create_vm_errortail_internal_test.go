package handlers

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/agent"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"

	sdknodes "github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/api/qemu"
)

// ---------------------------------------------------------------------------
// local fakes
//
// The shared mocks in testmocks_test.go live in package handlers_test and
// cannot reach the unexported targets here. These fakes embed the SDK service
// interfaces so only the methods each test exercises are implemented; any
// unimplemented method panics on a nil-interface call, which surfaces an
// accidental dependency immediately.
// ---------------------------------------------------------------------------

type etQEMU struct {
	qemu.Service
	resizeFn func(ctx context.Context, node string, vmid int, disk string, grow int) (string, error)
	stopFn   func(ctx context.Context, node string, vmid int) (string, error)
}

func (q *etQEMU) ResizeDisk(ctx context.Context, node string, vmid int, disk string, grow int) (string, error) {
	return q.resizeFn(ctx, node, vmid, disk, grow)
}

func (q *etQEMU) Stop(ctx context.Context, node string, vmid int) (string, error) {
	return q.stopFn(ctx, node, vmid)
}

type etNodes struct {
	sdknodes.Service
	deleteFn func(ctx context.Context, node, vmid string, params *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error)
}

func (n *etNodes) DeleteQemu(ctx context.Context, node, vmid string, params *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
	return n.deleteFn(ctx, node, vmid, params)
}

type etClient struct {
	pve.Client
	qemu  qemu.Service
	nodes sdknodes.Service
}

func (c *etClient) QEMU() qemu.Service      { return c.qemu }
func (c *etClient) Nodes() sdknodes.Service { return c.nodes }

type etAgent struct {
	configureFn func(ctx context.Context, node string, vmid int, cfg agent.AgentConfig) error
}

func (a *etAgent) Configure(ctx context.Context, node string, vmid int, cfg agent.AgentConfig) error {
	return a.configureFn(ctx, node, vmid, cfg)
}
func (a *etAgent) Remove(_ context.Context, _ string, _ int) error { return nil }
func (a *etAgent) UpdateDiskHints(_ context.Context, _ int, _ []agent.DiskHint) error {
	return nil
}

func etConfig() *config.CPIConfig {
	c := &config.CPIConfig{}
	c.ApplyDefaults()
	return c
}

// cleanupClient wires the QEMU + Nodes fakes cleanupVM touches and records
// whether the rollback delete fired. Stop returns an empty UPID (no await) and
// DeleteQemu returns (nil, nil) so cleanupVM completes its happy path.
func cleanupClient() (*etClient, *bool) {
	deleted := false
	return &etClient{
		qemu: &etQEMU{
			stopFn: func(_ context.Context, _ string, _ int) (string, error) { return "", nil },
		},
		nodes: &etNodes{
			deleteFn: func(_ context.Context, _ string, _ string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
				deleted = true
				return nil, nil
			},
		},
	}, &deleted
}

// --------------------------------------------------------------------------
// handleAwaitError
// --------------------------------------------------------------------------

// TestHandleAwaitError_VMIDConflict verifies a VMID-conflict await error is
// returned as-is WITHOUT triggering rollback (the conflicting VM belongs to a
// concurrent caller; destroying it would be wrong).
func TestHandleAwaitError_VMIDConflict(t *testing.T) {
	t.Parallel()
	client, deleted := cleanupClient()
	deps := Deps{Config: etConfig(), PVE: client, Logger: log.NewNopLogger()}

	werr := errors.New("vm 100 already exists")
	got := handleAwaitError(context.Background(), deps, log.NewNopLogger(), "node1", 100, werr)

	if !errors.Is(got, werr) {
		t.Errorf("returned error = %v, want original werr", got)
	}
	if *deleted {
		t.Error("cleanupVM ran on VMID conflict; it must NOT delete a conflicting VM")
	}
}

// TestHandleAwaitError_StorageLockTimeout verifies a storage-lock-timeout await
// error triggers rollback and is returned unchanged for the retry loop.
func TestHandleAwaitError_StorageLockTimeout(t *testing.T) {
	t.Parallel()
	client, deleted := cleanupClient()
	deps := Deps{Config: etConfig(), PVE: client, Logger: log.NewNopLogger()}

	werr := errors.New("can't lock file '/var/lock/pve-manager/pve-storage-data' - got timeout")
	got := handleAwaitError(context.Background(), deps, log.NewNopLogger(), "node1", 101, werr)

	if !errors.Is(got, werr) {
		t.Errorf("returned error = %v, want original werr", got)
	}
	if !*deleted {
		t.Error("cleanupVM did not run on storage lock timeout; partial VMID must be reclaimed")
	}
}

// TestHandleAwaitError_TransientTransport verifies a transient-transport await
// error triggers rollback and is returned unchanged.
func TestHandleAwaitError_TransientTransport(t *testing.T) {
	t.Parallel()
	client, deleted := cleanupClient()
	deps := Deps{Config: etConfig(), PVE: client, Logger: log.NewNopLogger()}

	werr := errors.New("auto-login failed: connection reset")
	got := handleAwaitError(context.Background(), deps, log.NewNopLogger(), "node1", 102, werr)

	if !errors.Is(got, werr) {
		t.Errorf("returned error = %v, want original werr", got)
	}
	if !*deleted {
		t.Error("cleanupVM did not run on transient transport fault")
	}
}

// TestHandleAwaitError_GenericFailure verifies a non-classified await error
// triggers rollback and is returned unchanged.
func TestHandleAwaitError_GenericFailure(t *testing.T) {
	t.Parallel()
	client, deleted := cleanupClient()
	deps := Deps{Config: etConfig(), PVE: client, Logger: log.NewNopLogger()}

	werr := errors.New("qmcreate failed: unexpected backend error")
	got := handleAwaitError(context.Background(), deps, log.NewNopLogger(), "node1", 103, werr)

	if !errors.Is(got, werr) {
		t.Errorf("returned error = %v, want original werr", got)
	}
	if !*deleted {
		t.Error("cleanupVM did not run on generic await failure")
	}
}

// --------------------------------------------------------------------------
// resizeRootDisk
// --------------------------------------------------------------------------

// TestResizeRootDisk_NoGrowth verifies resizeRootDisk is a no-op (no ResizeDisk
// call) when the requested root disk equals the stemcell base size.
func TestResizeRootDisk_NoGrowth(t *testing.T) {
	t.Parallel()
	called := false
	client := &etClient{qemu: &etQEMU{
		resizeFn: func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			called = true
			return "", nil
		},
	}}
	deps := Deps{Config: etConfig(), PVE: client, Logger: log.NewNopLogger()}
	shape := &createVMShape{node: "node1", rootDiskGiB: defaultStemcellDiskGiB, maxAttempts: 1}

	if err := resizeRootDisk(context.Background(), deps, log.NewNopLogger(), shape, 200); err != nil {
		t.Fatalf("resizeRootDisk returned error on no-growth path: %v", err)
	}
	if called {
		t.Error("ResizeDisk called when no growth was required")
	}
}

// TestResizeRootDisk_SubmitSuccessEmptyUPID verifies the success path where
// ResizeDisk returns an empty UPID (synchronous completion) — no task await is
// attempted and the function returns nil.
func TestResizeRootDisk_SubmitSuccessEmptyUPID(t *testing.T) {
	t.Parallel()
	client := &etClient{qemu: &etQEMU{
		resizeFn: func(_ context.Context, _ string, _ int, disk string, grow int) (string, error) {
			if disk != "virtio0" {
				t.Errorf("ResizeDisk disk = %q, want virtio0", disk)
			}
			if grow != 5 {
				t.Errorf("ResizeDisk grow = %d, want 5 (10 - 5 base)", grow)
			}
			return "", nil
		},
	}}
	deps := Deps{Config: etConfig(), PVE: client, Logger: log.NewNopLogger()}
	shape := &createVMShape{node: "node1", rootDiskGiB: 10, maxAttempts: 1}

	if err := resizeRootDisk(context.Background(), deps, log.NewNopLogger(), shape, 201); err != nil {
		t.Fatalf("resizeRootDisk returned error on submit-success path: %v", err)
	}
}

// TestResizeRootDisk_SubmitError verifies a non-transient ResizeDisk error is
// wrapped and returned after the retry budget is exhausted.
func TestResizeRootDisk_SubmitError(t *testing.T) {
	t.Parallel()
	client := &etClient{qemu: &etQEMU{
		resizeFn: func(_ context.Context, _ string, _ int, _ string, _ int) (string, error) {
			return "", errors.New("qm resize: invalid configuration")
		},
	}}
	deps := Deps{Config: etConfig(), PVE: client, Logger: log.NewNopLogger()}
	shape := &createVMShape{node: "node1", rootDiskGiB: 12, maxAttempts: 1}

	err := resizeRootDisk(context.Background(), deps, log.NewNopLogger(), shape, 202)
	if err == nil {
		t.Fatal("expected error from ResizeDisk failure, got nil")
	}
	if !strings.Contains(err.Error(), "resize virtio0") {
		t.Errorf("error missing wrap context: %v", err)
	}
}

// --------------------------------------------------------------------------
// configureAgent
// --------------------------------------------------------------------------

func configureAgentDeps(configureFn func(context.Context, string, int, agent.AgentConfig) error) Deps {
	return Deps{
		Config: etConfig(),
		PVE:    &etClient{},
		Agent:  &etAgent{configureFn: configureFn},
		Logger: log.NewNopLogger(),
	}
}

func configureAgentParsed() *createVMParsedArgs {
	return &createVMParsedArgs{
		agentID:  "agent-1",
		networks: map[string]createVMNetworkSpec{},
		env:      map[string]any{},
	}
}

// TestConfigureAgent_Success verifies the happy path forwards a populated
// AgentConfig and returns nil.
func TestConfigureAgent_Success(t *testing.T) {
	t.Parallel()
	var gotCfg agent.AgentConfig
	deps := configureAgentDeps(func(_ context.Context, _ string, _ int, cfg agent.AgentConfig) error {
		gotCfg = cfg
		return nil
	})
	shape := &createVMShape{node: "node1"}

	err := configureAgent(context.Background(), deps, log.NewNopLogger(), configureAgentParsed(), shape, 300, "vm-300")
	if err != nil {
		t.Fatalf("configureAgent returned error: %v", err)
	}
	if gotCfg.AgentID != "agent-1" {
		t.Errorf("AgentConfig.AgentID = %q, want agent-1", gotCfg.AgentID)
	}
	if gotCfg.VM.ID != "300" {
		t.Errorf("AgentConfig.VM.ID = %q, want 300", gotCfg.VM.ID)
	}
	if gotCfg.Disks.System != "/dev/sda" {
		t.Errorf("AgentConfig.Disks.System = %q, want /dev/sda", gotCfg.Disks.System)
	}
}

// TestConfigureAgent_Error verifies an agent.Configure failure is wrapped with
// create_vm context and the vmid.
func TestConfigureAgent_Error(t *testing.T) {
	t.Parallel()
	deps := configureAgentDeps(func(_ context.Context, _ string, _ int, _ agent.AgentConfig) error {
		return errors.New("settings write failed")
	})
	shape := &createVMShape{node: "node1"}

	err := configureAgent(context.Background(), deps, log.NewNopLogger(), configureAgentParsed(), shape, 301, "vm-301")
	if err == nil {
		t.Fatal("expected error from agent.Configure failure, got nil")
	}
	if !strings.Contains(err.Error(), "agent configure vmid=301") {
		t.Errorf("error missing wrap context: %v", err)
	}
}

// TestConfigureAgent_MBusAndBlobstoreFallback verifies that an empty env mbus
// falls back to Config.AgentMBus and Config.AgentBlobstore populates the
// blobstore provider + options.
func TestConfigureAgent_MBusAndBlobstoreFallback(t *testing.T) {
	t.Parallel()
	var gotCfg agent.AgentConfig
	deps := configureAgentDeps(func(_ context.Context, _ string, _ int, cfg agent.AgentConfig) error {
		gotCfg = cfg
		return nil
	})
	deps.Config.AgentMBus = "nats://mbus.example:4222"
	deps.Config.AgentBlobstore = map[string]any{
		"provider": "dav",
		"options":  map[string]any{"endpoint": "https://blobstore.example"},
	}
	shape := &createVMShape{node: "node1"}

	err := configureAgent(context.Background(), deps, log.NewNopLogger(), configureAgentParsed(), shape, 302, "vm-302")
	if err != nil {
		t.Fatalf("configureAgent returned error: %v", err)
	}
	if gotCfg.MBus != "nats://mbus.example:4222" {
		t.Errorf("AgentConfig.MBus = %q, want config fallback value", gotCfg.MBus)
	}
	if gotCfg.Blobstore.Provider != "dav" {
		t.Errorf("AgentConfig.Blobstore.Provider = %q, want dav", gotCfg.Blobstore.Provider)
	}
	if _, ok := gotCfg.Blobstore.Options["endpoint"]; !ok {
		t.Errorf("AgentConfig.Blobstore.Options missing endpoint: %#v", gotCfg.Blobstore.Options)
	}
}
