// Package handlers internal tests for the ZFS thick-provisioning diagnostic
// (warnIfZFSThickProvisioned): once-per-pool-per-process dedup, sparse=1
// silence, the knownStorageType=="zfspool" gate (no independent lookup, ever),
// and fail-open on a ListStorage error.
package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cloudinit"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdkclusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/tasks"
)

// ztpEntry is one synthetic /storage list row for ztpClusterStorage.
type ztpEntry struct {
	storage string
	typ     string
	sparse  *int // nil = key absent, matching an unset PVE config line
}

// ztpClusterStorage implements clusterstorage.Service with a fixed set of
// entries and a call counter, so tests can assert both the log outcome and
// how many times ListStorage was actually invoked.
type ztpClusterStorage struct {
	sdkclusterstorage.Service
	entries   []ztpEntry
	err       error
	callCount int
}

func (m *ztpClusterStorage) ListStorage(_ context.Context, _ *sdkclusterstorage.ListStorageParams) (*sdkclusterstorage.ListStorageResponse, error) {
	m.callCount++
	if m.err != nil {
		return nil, m.err
	}
	resp := make(sdkclusterstorage.ListStorageResponse, 0, len(m.entries))
	for _, e := range m.entries {
		v := map[string]any{"storage": e.storage, "type": e.typ}
		if e.sparse != nil {
			v["sparse"] = *e.sparse
		}
		raw, _ := json.Marshal(v)
		resp = append(resp, raw)
	}
	return &resp, nil
}

// ztpPVEClient is a minimal pve.Client stub exposing only ClusterStorage()
// meaningfully; every other method is unused by warnIfZFSThickProvisioned
// and returns nil (typed nil interface values, as none of these are ever
// called by the function under test).
type ztpPVEClient struct {
	clusterStorageSvc sdkclusterstorage.Service
}

var _ pve.Client = (*ztpPVEClient)(nil)

func (c *ztpPVEClient) QEMU() qemu.Service                        { return nil }
func (c *ztpPVEClient) Storage() storage.Service                  { return nil }
func (c *ztpPVEClient) CloudInit() cloudinit.Service              { return nil }
func (c *ztpPVEClient) Tasks() tasks.Service                      { return nil }
func (c *ztpPVEClient) Nodes() nodes.Service                      { return nil }
func (c *ztpPVEClient) Cluster() sdkcluster.Service               { return nil }
func (c *ztpPVEClient) ClusterStorage() sdkclusterstorage.Service { return c.clusterStorageSvc }
func (c *ztpPVEClient) Pools() pve.PoolService                    { return nil }

func ztpDeps(cs *ztpClusterStorage, buf *bytes.Buffer) Deps {
	logger, err := log.NewLogger("debug", buf)
	if err != nil {
		panic(err)
	}
	return Deps{
		PVE:    &ztpPVEClient{clusterStorageSvc: cs},
		Logger: logger,
	}
}

// resetZFSThickProvisioningState clears the package-level dedup ledger
// between tests so each test observes a clean process-lifetime state — the
// ledger is otherwise shared across the whole test binary run.
func resetZFSThickProvisioningState() {
	zfsThickProvisioningWarnedPools.Range(func(k, _ any) bool {
		zfsThickProvisioningWarnedPools.Delete(k)
		return true
	})
}

func TestWarnIfZFSThickProvisioned_FiresOnceForRepeatedOps(t *testing.T) {
	resetZFSThickProvisioningState()
	var buf bytes.Buffer
	sparseZero := 0
	cs := &ztpClusterStorage{entries: []ztpEntry{{storage: "zpool1", typ: pve.StorageTypeZFSPool, sparse: &sparseZero}}}
	deps := ztpDeps(cs, &buf)

	warnIfZFSThickProvisioned(context.Background(), deps, "zpool1", pve.StorageTypeZFSPool)
	warnIfZFSThickProvisioned(context.Background(), deps, "zpool1", pve.StorageTypeZFSPool)
	warnIfZFSThickProvisioned(context.Background(), deps, "zpool1", pve.StorageTypeZFSPool)

	out := buf.String()
	count := strings.Count(out, "zfspool storage provisions thick")
	if count != 1 {
		t.Errorf("expected exactly 1 Info log for repeated ops on the same pool, got %d; output: %s", count, out)
	}
	// The first call populates the ledger and returns before a second
	// ListStorage call would ever be needed on subsequent calls.
	if cs.callCount != 1 {
		t.Errorf("expected exactly 1 ListStorage call across all 3 repeats (dedup short-circuits the rest), got %d", cs.callCount)
	}
}

func TestWarnIfZFSThickProvisioned_SecondDistinctPool_FiresOwnInfo(t *testing.T) {
	resetZFSThickProvisioningState()
	var buf bytes.Buffer
	sparseZero := 0
	cs := &ztpClusterStorage{entries: []ztpEntry{
		{storage: "zpool1", typ: pve.StorageTypeZFSPool, sparse: &sparseZero},
		{storage: "zpool2", typ: pve.StorageTypeZFSPool, sparse: &sparseZero},
	}}
	deps := ztpDeps(cs, &buf)

	warnIfZFSThickProvisioned(context.Background(), deps, "zpool1", pve.StorageTypeZFSPool)
	warnIfZFSThickProvisioned(context.Background(), deps, "zpool2", pve.StorageTypeZFSPool)

	out := buf.String()
	if !strings.Contains(out, "\"zpool1\"") {
		t.Errorf("expected an Info log naming zpool1, got: %s", out)
	}
	if !strings.Contains(out, "\"zpool2\"") {
		t.Errorf("expected an Info log naming zpool2, got: %s", out)
	}
	count := strings.Count(out, "zfspool storage provisions thick")
	if count != 2 {
		t.Errorf("expected 2 Info logs (one per distinct pool), got %d; output: %s", count, out)
	}
}

func TestWarnIfZFSThickProvisioned_Sparse1_Silent(t *testing.T) {
	resetZFSThickProvisioningState()
	var buf bytes.Buffer
	sparseOne := 1
	cs := &ztpClusterStorage{entries: []ztpEntry{{storage: "zpool1", typ: pve.StorageTypeZFSPool, sparse: &sparseOne}}}
	deps := ztpDeps(cs, &buf)

	warnIfZFSThickProvisioned(context.Background(), deps, "zpool1", pve.StorageTypeZFSPool)

	if strings.Contains(buf.String(), "zfspool storage provisions thick") {
		t.Errorf("sparse=1 pool must be silent, got: %s", buf.String())
	}
}

func TestWarnIfZFSThickProvisioned_SparseAbsent_TreatedAsThick(t *testing.T) {
	resetZFSThickProvisioningState()
	var buf bytes.Buffer
	cs := &ztpClusterStorage{entries: []ztpEntry{{storage: "zpool1", typ: pve.StorageTypeZFSPool, sparse: nil}}}
	deps := ztpDeps(cs, &buf)

	warnIfZFSThickProvisioned(context.Background(), deps, "zpool1", pve.StorageTypeZFSPool)

	if !strings.Contains(buf.String(), "zfspool storage provisions thick") {
		t.Errorf("absent sparse key must be treated as thick (0), got: %s", buf.String())
	}
}

// TestWarnIfZFSThickProvisioned_NonZFSPoolKnownType_NoLookup verifies that a
// caller-supplied knownStorageType of anything other than exactly "zfspool"
// skips the check entirely — no ListStorage call, no log.
func TestWarnIfZFSThickProvisioned_NonZFSPoolKnownType_NoLookup(t *testing.T) {
	resetZFSThickProvisioningState()
	var buf bytes.Buffer
	cs := &ztpClusterStorage{entries: []ztpEntry{{storage: "lvmpool", typ: "lvmthin"}}}
	deps := ztpDeps(cs, &buf)

	warnIfZFSThickProvisioned(context.Background(), deps, "lvmpool", "lvmthin")

	if cs.callCount != 0 {
		t.Errorf("ListStorage must not be called when knownStorageType is not exactly \"zfspool\"; got %d call(s)", cs.callCount)
	}
	if buf.Len() > 0 {
		t.Errorf("expected no log output, got: %s", buf.String())
	}
}

// TestWarnIfZFSThickProvisioned_UnresolvedType_NoIndependentLookup verifies
// the critical non-regression: when the caller did not resolve a storage
// type at all (knownStorageType == "", e.g. because create_disk's own
// discard/ssd values were both explicit and never triggered a live type
// lookup — see needsDiskPerfStorageTypeLookup), this diagnostic must NOT
// perform its own independent ListStorage call. Introducing an unconditional
// live lookup here would silently reintroduce the exact API round-trip an
// operator opted out of.
func TestWarnIfZFSThickProvisioned_UnresolvedType_NoIndependentLookup(t *testing.T) {
	resetZFSThickProvisioningState()
	var buf bytes.Buffer
	sparseZero := 0
	cs := &ztpClusterStorage{entries: []ztpEntry{{storage: "zpool1", typ: pve.StorageTypeZFSPool, sparse: &sparseZero}}}
	deps := ztpDeps(cs, &buf)

	warnIfZFSThickProvisioned(context.Background(), deps, "zpool1", "")

	if cs.callCount != 0 {
		t.Errorf("an unresolved (empty) knownStorageType must never trigger an independent lookup; got %d call(s)", cs.callCount)
	}
	if buf.Len() > 0 {
		t.Errorf("expected no log output, got: %s", buf.String())
	}
}

func TestWarnIfZFSThickProvisioned_LookupError_FailsOpen(t *testing.T) {
	resetZFSThickProvisioningState()
	var buf bytes.Buffer
	cs := &ztpClusterStorage{err: errors.New("PVE API unreachable")}
	deps := ztpDeps(cs, &buf)

	// Must not panic, must not log at Info, and (implicitly, since this is
	// a diagnostic-only void function) must not surface any error to a caller.
	warnIfZFSThickProvisioned(context.Background(), deps, "zpool1", pve.StorageTypeZFSPool)

	if strings.Contains(buf.String(), "zfspool storage provisions thick") {
		t.Errorf("a ListStorage error must never produce the thick-provisioning Info log, got: %s", buf.String())
	}
}

func TestWarnIfZFSThickProvisioned_EmptyStorageName_NoOp(t *testing.T) {
	resetZFSThickProvisioningState()
	var buf bytes.Buffer
	cs := &ztpClusterStorage{entries: []ztpEntry{{storage: "zpool1", typ: pve.StorageTypeZFSPool}}}
	deps := ztpDeps(cs, &buf)

	warnIfZFSThickProvisioned(context.Background(), deps, "", pve.StorageTypeZFSPool)

	if cs.callCount != 0 {
		t.Errorf("empty storage name must not trigger any lookup; got %d call(s)", cs.callCount)
	}
}
