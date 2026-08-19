package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
)

// fakeClusterStorageEntry describes one /storage index entry used by
// fakeClusterStorageSvc.
type fakeClusterStorageEntry struct {
	storage string
	stype   string
	shared  bool
	content string // CSV, e.g. "images,iso,vztmpl"
}

// fakeClusterStorageSvc implements sdkclusterstorage.Service (ListStorage
// only; the remaining methods panic) so ResolveISOStorage can be tested
// against a controlled /storage index. listCalls counts ListStorage
// invocations so tests asserting a resolution short-circuit (no PVE call at
// all) can verify that directly rather than inferring it from the fallback
// return value.
type fakeClusterStorageSvc struct {
	entries   []fakeClusterStorageEntry
	listErr   error
	listCalls int
}

var _ sdkclusterstorage.Service = (*fakeClusterStorageSvc)(nil)

func (f *fakeClusterStorageSvc) ListStorage(context.Context, *sdkclusterstorage.ListStorageParams) (*sdkclusterstorage.ListStorageResponse, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	resp := make(sdkclusterstorage.ListStorageResponse, 0, len(f.entries))
	for _, e := range f.entries {
		sharedInt := 0
		if e.shared {
			sharedInt = 1
		}
		raw, _ := json.Marshal(map[string]any{
			"storage": e.storage,
			"type":    e.stype,
			"shared":  sharedInt,
			"content": e.content,
		})
		resp = append(resp, json.RawMessage(raw))
	}
	return &resp, nil
}

func (f *fakeClusterStorageSvc) CreateStorage(context.Context, *sdkclusterstorage.CreateStorageParams) (*sdkclusterstorage.CreateStorageResponse, error) {
	panic("fakeClusterStorageSvc.CreateStorage: not expected")
}

func (f *fakeClusterStorageSvc) DeleteStorage(context.Context, string) error {
	panic("fakeClusterStorageSvc.DeleteStorage: not expected")
}

func (f *fakeClusterStorageSvc) GetStorage(context.Context, string) (*sdkclusterstorage.GetStorageResponse, error) {
	panic("fakeClusterStorageSvc.GetStorage: not expected")
}

func (f *fakeClusterStorageSvc) UpdateStorage(context.Context, string, *sdkclusterstorage.UpdateStorageParams) (*sdkclusterstorage.UpdateStorageResponse, error) {
	panic("fakeClusterStorageSvc.UpdateStorage: not expected")
}

// resolveISOTestCfg returns a minimal cloudinit-mode CPIConfig with
// iso_storage at its spec default ("local") and vm_storage set to
// vmStorageName, ready for follow-vm-storage resolution tests.
func resolveISOTestCfg(vmStorageName string, followVMStorage bool) *config.CPIConfig {
	return &config.CPIConfig{
		Host:                      "pve.example.com",
		Port:                      8006,
		User:                      "root",
		Password:                  "secret",
		Realm:                     "pam",
		Node:                      "pve",
		VMStorage:                 vmStorageName,
		DiskStorage:               vmStorageName,
		ISOStorage:                "local", // spec default sentinel
		NetworkBridge:             "vmbr0",
		AgentMode:                 "cloudinit",
		VMDiskFormat:              "qcow2",
		LogLevel:                  "info",
		VMIDRangeStart:            100,
		ISOStorageFollowVMStorage: boolPtr(followVMStorage),
	}
}

func TestResolveISOStorage_FlagDisabled_ReturnsISOStorageUnchanged_NoPVECalls(t *testing.T) {
	t.Parallel()
	cfg := resolveISOTestCfg("vm-storage", false)
	storageSvc := &fakeClusterStorageSvc{}
	pveClient := &fakePVEClient{clusterStorageSvc: storageSvc}

	got := ResolveISOStorage(context.Background(), cfg, pveClient, log.NewNopLogger())
	if got != "local" {
		t.Fatalf("flag disabled: want %q, got %q", "local", got)
	}
	if storageSvc.listCalls != 0 {
		t.Errorf("flag disabled: expected zero ListStorage calls, got %d", storageSvc.listCalls)
	}
}

func TestResolveISOStorage_ExplicitISOStorage_Untouched(t *testing.T) {
	t.Parallel()
	cfg := resolveISOTestCfg("vm-storage", true)
	cfg.ISOStorage = "bosh-isos" // operator explicitly pinned a non-default pool
	storageSvc := &fakeClusterStorageSvc{}
	pveClient := &fakePVEClient{clusterStorageSvc: storageSvc}

	got := ResolveISOStorage(context.Background(), cfg, pveClient, log.NewNopLogger())
	if got != "bosh-isos" {
		t.Fatalf("explicit iso_storage: want %q (untouched), got %q", "bosh-isos", got)
	}
	if storageSvc.listCalls != 0 {
		t.Errorf("explicit iso_storage: expected zero ListStorage calls, got %d", storageSvc.listCalls)
	}
}

func TestResolveISOStorage_SharedWithISOContent_Follows(t *testing.T) {
	t.Parallel()
	cfg := resolveISOTestCfg("vm-storage", true)
	pveClient := &fakePVEClient{clusterStorageSvc: &fakeClusterStorageSvc{entries: []fakeClusterStorageEntry{
		{storage: "vm-storage", stype: "nfs", shared: true, content: "images,iso,vztmpl"},
	}}}

	got := ResolveISOStorage(context.Background(), cfg, pveClient, log.NewNopLogger())
	if got != "vm-storage" {
		t.Fatalf("shared+iso content: want to follow vm_storage %q, got %q", "vm-storage", got)
	}
}

func TestResolveISOStorage_SharedWithoutISOContent_FallsBack(t *testing.T) {
	t.Parallel()
	cfg := resolveISOTestCfg("vm-storage", true)
	pveClient := &fakePVEClient{clusterStorageSvc: &fakeClusterStorageSvc{entries: []fakeClusterStorageEntry{
		{storage: "vm-storage", stype: "nfs", shared: true, content: "images,vztmpl"},
	}}}

	got := ResolveISOStorage(context.Background(), cfg, pveClient, log.NewNopLogger())
	if got != "local" {
		t.Fatalf("shared without iso content: want fallback %q, got %q", "local", got)
	}
}

func TestResolveISOStorage_NonShared_FallsBack(t *testing.T) {
	t.Parallel()
	cfg := resolveISOTestCfg("vm-storage", true)
	pveClient := &fakePVEClient{clusterStorageSvc: &fakeClusterStorageSvc{entries: []fakeClusterStorageEntry{
		{storage: "vm-storage", stype: "dir", shared: false, content: "images,iso,vztmpl"},
	}}}

	got := ResolveISOStorage(context.Background(), cfg, pveClient, log.NewNopLogger())
	if got != "local" {
		t.Fatalf("non-shared: want fallback %q, got %q", "local", got)
	}
}

func TestResolveISOStorage_VMStorageNotFound_FallsBackFailOpen(t *testing.T) {
	t.Parallel()
	cfg := resolveISOTestCfg("vm-storage", true)
	pveClient := &fakePVEClient{clusterStorageSvc: &fakeClusterStorageSvc{entries: nil}} // empty index

	got := ResolveISOStorage(context.Background(), cfg, pveClient, log.NewNopLogger())
	if got != "local" {
		t.Fatalf("vm_storage not found: want fallback %q, got %q", "local", got)
	}
}

func TestResolveISOStorage_ListStorageError_FallsBackFailOpen(t *testing.T) {
	t.Parallel()
	cfg := resolveISOTestCfg("vm-storage", true)
	storageSvc := &fakeClusterStorageSvc{listErr: errors.New("simulated PVE API failure")}
	pveClient := &fakePVEClient{clusterStorageSvc: storageSvc}

	got := ResolveISOStorage(context.Background(), cfg, pveClient, log.NewNopLogger())
	if got != "local" {
		t.Fatalf("ListStorage error: want fail-open fallback %q, got %q", "local", got)
	}
	if storageSvc.listCalls != 1 {
		t.Errorf("ListStorage error: expected exactly 1 ListStorage call, got %d", storageSvc.listCalls)
	}
}

func TestResolveISOStorage_EmptyVMStorage_FallsBack(t *testing.T) {
	t.Parallel()
	cfg := resolveISOTestCfg("", true)
	storageSvc := &fakeClusterStorageSvc{}
	pveClient := &fakePVEClient{clusterStorageSvc: storageSvc}

	got := ResolveISOStorage(context.Background(), cfg, pveClient, log.NewNopLogger())
	if got != "local" {
		t.Fatalf("empty vm_storage: want fallback %q, got %q", "local", got)
	}
	if storageSvc.listCalls != 0 {
		t.Errorf("empty vm_storage: expected zero ListStorage calls, got %d", storageSvc.listCalls)
	}
}

func TestResolveISOStorage_NoAgentMode_NoOp(t *testing.T) {
	t.Parallel()
	cfg := resolveISOTestCfg("vm-storage", true)
	cfg.AgentMode = "noagent"
	storageSvc := &fakeClusterStorageSvc{}
	pveClient := &fakePVEClient{clusterStorageSvc: storageSvc}

	got := ResolveISOStorage(context.Background(), cfg, pveClient, log.NewNopLogger())
	if got != "local" {
		t.Fatalf("noagent mode: want iso_storage unchanged %q, got %q", "local", got)
	}
	if storageSvc.listCalls != 0 {
		t.Errorf("noagent mode: expected zero ListStorage calls, got %d", storageSvc.listCalls)
	}
}
