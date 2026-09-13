package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkcloudinit "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cloudinit"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	sdktasks "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
)

// ---------------------------------------------------------------------------
// The parker config builders and the three park funnels that were folded onto
// them. Two things are under test here. The first is that the configured
// parker prefix and pool reach every funnel that can create a parker, which is
// the whole point of the fold: a funnel that kept its own literal would
// compile, pass, and name a live cluster's parkers "bosh-parker-..." whatever
// the operator asked for. The second is that the write builder leaves
// FallbackNode empty, because pve.ParkDisk and pve.TransferDiskToParker fill
// that field with the disk's own node only while it is empty, and the
// job-level node is the wrong answer on a multi-node cluster.
//
// The funnel tests swap the parkDisk seam (park_disk_seam.go) and must not run
// in parallel.
// ---------------------------------------------------------------------------

const (
	parkerCfgPrefix  = "acme"
	parkerCfgPool    = "acme-parker"
	parkerCfgJobNode = "pve1"
	parkerCfgStorage = "local-lvm"
	parkerCfgVolid   = parkerCfgStorage + ":vm-9001-disk-0"
)

// parkerCfgTestConfig is a parked-strategy config with a non-default parker
// prefix and the spec's default pool template, so the rendered pool differs
// from the built-in "bosh-parker" and a funnel that dropped either field shows
// up as a plain string mismatch.
func parkerCfgTestConfig() *config.CPIConfig {
	return &config.CPIConfig{
		Node:                 parkerCfgJobNode,
		DiskStorage:          parkerCfgStorage,
		DetachedDiskStrategy: "parked",
		ParkerPrefix:         parkerCfgPrefix,
		ParkerPool:           "{prefix}-parker",
	}
}

// parkerCfgCluster is a cluster service fake reporting a single-member
// cluster, which is what the authoritative guest enumeration reads before it
// fans out per node.
type parkerCfgCluster struct {
	sdkcluster.Service
}

func (c *parkerCfgCluster) ListConfigNodes(_ context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
	raw, err := json.Marshal(map[string]any{"name": parkerCfgJobNode})
	if err != nil {
		return nil, err
	}
	resp := sdkcluster.ListConfigNodesResponse{raw}
	return &resp, nil
}

// ListStatus answers with no rows, so the enumeration's offline-member
// tolerance finds no quorate cluster to take authority from and treats every
// member as online. That is the strict behavior, which is what we want here.
func (c *parkerCfgCluster) ListStatus(_ context.Context) (*sdkcluster.ListStatusResponse, error) {
	empty := sdkcluster.ListStatusResponse{}
	return &empty, nil
}

// parkerCfgNodes is a nodes service fake whose per-node guest listing comes
// back empty, so the holder scan concludes the volume is free-floating and the
// funnel goes on to park it.
type parkerCfgNodes struct {
	sdknodes.Service
	listQemuCalls int
}

func (n *parkerCfgNodes) ListQemu(
	_ context.Context, _ string, _ *sdknodes.ListQemuParams,
) (*sdknodes.ListQemuResponse, error) {
	n.listQemuCalls++
	empty := sdknodes.ListQemuResponse{}
	return &empty, nil
}

// parkerCfgClient implements pve.Client with only Cluster() and Nodes() wired,
// which is all the holder scan needs once the park itself runs through the
// seam. Every other service is nil so an unintended call panics loudly.
type parkerCfgClient struct {
	cluster sdkcluster.Service
	nodes   sdknodes.Service
}

func (c *parkerCfgClient) QEMU() sdkqemu.Service                     { return nil }
func (c *parkerCfgClient) Nodes() sdknodes.Service                   { return c.nodes }
func (c *parkerCfgClient) Storage() sdkstorage.Service               { return nil }
func (c *parkerCfgClient) CloudInit() sdkcloudinit.Service           { return nil }
func (c *parkerCfgClient) Tasks() sdktasks.Service                   { return nil }
func (c *parkerCfgClient) Cluster() sdkcluster.Service               { return c.cluster }
func (c *parkerCfgClient) ClusterStorage() sdkclusterstorage.Service { return nil }
func (c *parkerCfgClient) Pools() pve.PoolService                    { return nil }

// parkerCfgBackend is a pve.Backend that reports one fixed node for an
// existing volume, standing in for the storage lookup
// resolveNodeForDetachedDisk makes.
type parkerCfgBackend struct{ node string }

func (b parkerCfgBackend) Kind() pve.BackendKind { return pve.BackendShared }

func (b parkerCfgBackend) NodeForCreate(_ context.Context, _, _ string) (string, error) {
	return b.node, nil
}

func (b parkerCfgBackend) NodeForExisting(_ context.Context, _ string) (string, error) {
	return b.node, nil
}

// parkerCfgResolver hands the same backend back for every storage.
type parkerCfgResolver struct{ backend parkerCfgBackend }

func (r parkerCfgResolver) Resolve(_ context.Context, _ string) (pve.Backend, error) {
	return r.backend, nil
}

// captureParkDisk swaps the parkDisk seam for one that records the
// ParkerConfig it was handed and returns parkErr, restoring the seam when the
// test ends. The caller must not run in parallel: the seam is process-wide.
func captureParkDisk(t *testing.T, parkErr error) *pve.ParkerConfig {
	t.Helper()
	got := &pve.ParkerConfig{}
	called := false
	previous := parkDisk
	parkDisk = func(
		_ context.Context, _ pve.Client, _ *log.Logger,
		_, _ string, cfg pve.ParkerConfig, _ pve.ParkContext,
	) error {
		called = true
		*got = cfg
		return parkErr
	}
	t.Cleanup(func() {
		parkDisk = previous
		if !called {
			t.Errorf("the park funnel never reached parkDisk; nothing was asserted")
		}
	})
	return got
}

// assertParkerCfgPrefixAndPool checks the two fields the fold exists to carry.
func assertParkerCfgPrefixAndPool(t *testing.T, got pve.ParkerConfig, where string) {
	t.Helper()
	if got.Prefix != parkerCfgPrefix {
		t.Errorf("%s: ParkerConfig.Prefix = %q; want %q", where, got.Prefix, parkerCfgPrefix)
	}
	if got.Pool != parkerCfgPool {
		t.Errorf("%s: ParkerConfig.Pool = %q; want %q", where, got.Pool, parkerCfgPool)
	}
}

// ---------------------------------------------------------------------------
// The builders
// ---------------------------------------------------------------------------

func TestParkerReadConfigFor_CarriesPrefixAndPool(t *testing.T) {
	t.Parallel()

	deps := Deps{Config: parkerCfgTestConfig(), Logger: log.NewNopLogger()}
	got := parkerReadConfigFor(deps)

	assertParkerCfgPrefixAndPool(t, got, "parkerReadConfigFor")
	// The read builder keeps the job-level node: its callers scan for a holder
	// without having resolved a node of their own.
	if got.FallbackNode != parkerCfgJobNode {
		t.Errorf("parkerReadConfigFor: FallbackNode = %q; want the job-level node %q",
			got.FallbackNode, parkerCfgJobNode)
	}
}

func TestParkerReadConfigFor_DefaultsToTheHistoricalPrefixAndNoPool(t *testing.T) {
	t.Parallel()

	// An operator who sets neither field gets what every prior release
	// produced: parkers named "bosh-parker-<vmid>" and no pool assignment at
	// all, since an empty pool is the documented opt-out.
	deps := Deps{Config: &config.CPIConfig{DetachedDiskStrategy: "parked"}, Logger: log.NewNopLogger()}
	got := parkerReadConfigFor(deps)

	if got.Prefix != "bosh" {
		t.Errorf("ParkerConfig.Prefix = %q; want the historical default %q", got.Prefix, "bosh")
	}
	if got.Pool != "" {
		t.Errorf("ParkerConfig.Pool = %q; want empty (the opt-out) when pve.parker_pool is unset", got.Pool)
	}
}

func TestParkerWriteConfigFor_LeavesFallbackNodeEmpty(t *testing.T) {
	t.Parallel()

	// This is the invariant the fold rests on. pve.ParkDisk and
	// pve.TransferDiskToParker each default FallbackNode to the disk's own
	// node, and only while the field is empty. A builder that carried the
	// read builder's job-level node across would silence that default and hand
	// a multi-node cluster the wrong node for every park, with no test failing.
	deps := Deps{Config: parkerCfgTestConfig(), Logger: log.NewNopLogger()}
	got := parkerWriteConfigFor(deps)

	if got.FallbackNode != "" {
		t.Fatalf("parkerWriteConfigFor: FallbackNode = %q; want empty so ParkDisk and "+
			"TransferDiskToParker can fill it with the disk's own node", got.FallbackNode)
	}
	assertParkerCfgPrefixAndPool(t, got, "parkerWriteConfigFor")
	if got.DiskStorage != parkerCfgStorage {
		t.Errorf("parkerWriteConfigFor: DiskStorage = %q; want %q", got.DiskStorage, parkerCfgStorage)
	}
}

// ---------------------------------------------------------------------------
// parkFreshDisk (create_disk)
// ---------------------------------------------------------------------------

func TestParkFreshDisk_HandsDownPrefixPoolAndEmptyFallbackNode(t *testing.T) {
	got := captureParkDisk(t, nil)

	deps := Deps{Config: parkerCfgTestConfig(), Logger: log.NewNopLogger()}
	if err := parkFreshDisk(context.Background(), deps, parkerCfgJobNode,
		"pvd-abc", parkerCfgVolid, "stable-id"); err != nil {
		t.Fatalf("parkFreshDisk: unexpected error: %v", err)
	}

	assertParkerCfgPrefixAndPool(t, *got, "parkFreshDisk")
	if got.FallbackNode != "" {
		t.Errorf("parkFreshDisk: FallbackNode = %q; want empty so ParkDisk uses the disk's own node",
			got.FallbackNode)
	}
	if got.DiskStorage != parkerCfgStorage {
		t.Errorf("parkFreshDisk: DiskStorage = %q; want %q", got.DiskStorage, parkerCfgStorage)
	}
}

func TestParkFreshDisk_SkipsTheParkEntirelyUnderTheFreeStrategy(t *testing.T) {
	previous := parkDisk
	parkDisk = func(
		_ context.Context, _ pve.Client, _ *log.Logger,
		_, _ string, _ pve.ParkerConfig, _ pve.ParkContext,
	) error {
		t.Error("parkFreshDisk called ParkDisk under detached_disk_strategy=free")
		return nil
	}
	t.Cleanup(func() { parkDisk = previous })

	cfg := parkerCfgTestConfig()
	cfg.DetachedDiskStrategy = "free"
	deps := Deps{Config: cfg, Logger: log.NewNopLogger()}
	if err := parkFreshDisk(context.Background(), deps, parkerCfgJobNode,
		"pvd-abc", parkerCfgVolid, "stable-id"); err != nil {
		t.Fatalf("parkFreshDisk: unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// parkAfterDetach (detach_disk)
// ---------------------------------------------------------------------------

func TestParkAfterDetach_HandsDownPrefixPoolAndEmptyFallbackNode(t *testing.T) {
	got := captureParkDisk(t, nil)

	deps := Deps{Config: parkerCfgTestConfig(), Logger: log.NewNopLogger()}
	if err := parkAfterDetach(context.Background(), deps, "100", "pvd-abc",
		parkerCfgVolid, parkerCfgJobNode, nil); err != nil {
		t.Fatalf("parkAfterDetach: unexpected error: %v", err)
	}

	assertParkerCfgPrefixAndPool(t, *got, "parkAfterDetach")
	if got.FallbackNode != "" {
		t.Errorf("parkAfterDetach: FallbackNode = %q; want empty so ParkDisk uses the disk's own node",
			got.FallbackNode)
	}
	if got.DiskStorage != parkerCfgStorage {
		t.Errorf("parkAfterDetach: DiskStorage = %q; want %q", got.DiskStorage, parkerCfgStorage)
	}
}

func TestParkAfterDetach_KeepsThePermanentClassTheParkChose(t *testing.T) {
	// The fold must not change how a failed park is reported: a park that
	// refuses permanently stays permanent, so the Director stops instead of
	// retrying a grant it cannot make on its own.
	permanent := errors.New("simulated park failure")
	_ = captureParkDisk(t, permanent)

	deps := Deps{Config: parkerCfgTestConfig(), Logger: log.NewNopLogger()}
	err := parkAfterDetach(context.Background(), deps, "100", "pvd-abc",
		parkerCfgVolid, parkerCfgJobNode, nil)
	if err == nil {
		t.Fatal("parkAfterDetach: want an error when the park fails, got nil")
	}
	if !errors.Is(err, permanent) {
		t.Errorf("parkAfterDetach: want the park's own error wrapped, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// handleAlreadyDetachedParked (detach_disk retry)
// ---------------------------------------------------------------------------

func TestHandleAlreadyDetachedParked_HandsDownPrefixPoolAndTheJobLevelFallbackNode(t *testing.T) {
	got := captureParkDisk(t, nil)

	// An empty cluster means the holder scan finds nobody, so the disk reads
	// as free-floating and the funnel goes on to park it.
	nodesSvc := &parkerCfgNodes{}
	deps := Deps{
		Config:   parkerCfgTestConfig(),
		PVE:      &parkerCfgClient{cluster: &parkerCfgCluster{}, nodes: nodesSvc},
		Resolver: parkerCfgResolver{backend: parkerCfgBackend{node: "pve2"}},
		Logger:   log.NewNopLogger(),
	}

	if err := handleAlreadyDetachedParked(context.Background(), deps, "pvd-abc", parkerCfgVolid); err != nil {
		t.Fatalf("handleAlreadyDetachedParked: unexpected error: %v", err)
	}

	assertParkerCfgPrefixAndPool(t, *got, "handleAlreadyDetachedParked")
	if got.DiskStorage != parkerCfgStorage {
		t.Errorf("handleAlreadyDetachedParked: DiskStorage = %q; want %q",
			got.DiskStorage, parkerCfgStorage)
	}
	// This funnel is the one exception to the empty-FallbackNode rule, and the
	// order it writes the field in is what makes the exception work: the write
	// builder clears the field, and the funnel sets the job-level node back on
	// the result afterwards. Its holder scan runs before any node is resolved,
	// and a cluster-resources row without a node is dropped unless the scan has
	// a fallback to attribute it to.
	if got.FallbackNode != parkerCfgJobNode {
		t.Errorf("handleAlreadyDetachedParked: FallbackNode = %q; want the job-level node %q",
			got.FallbackNode, parkerCfgJobNode)
	}
	if nodesSvc.listQemuCalls == 0 {
		t.Error("handleAlreadyDetachedParked: the holder scan never ran")
	}
}
