// attach_disk_migrate_internal_test.go — white-box tests for the A2
// cross-node migration path: the resolve-node branch's knob and legacy
// gating, and the holder guard's mover flow end to end against a node-aware
// fake PVE (fresh mover, isolation transfer, offline migration, final attach,
// mover destroy).
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	clusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
	sdkerrors "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/errors"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

const migTestToken = "bpd-1122334455667788"

// migFakeClient is a node-aware stateful fake PVE cluster: every VM lives on
// exactly one node, config reads addressed to the wrong node answer 404 (the
// behavior migrateMoverToNode's source-vs-target probing depends on), move
// operations refuse to cross nodes (PVE's real constraint that makes the
// mover necessary), and the migrate endpoint moves a VM between nodes,
// renaming its volumes when the local-disk flag rides along.
type migFakeClient struct {
	pve.Client

	mu      sync.Mutex
	configs map[int]map[string]any
	nodes   map[int]string
	renameN map[int]int
	// migrations records every CreateQemuMigrate call.
	migrations []migRecord
	// deletedVMs records every DeleteQemu call that removed a VM.
	deletedVMs []int
	// destroyed records volumes physically removed by an owned-unused sweep.
	destroyed []string
	// migrateErr, when set, fails the next CreateQemuMigrate and clears
	// itself (one-shot).
	migrateErr error
	// renameOnMigrate mimics node-local storage: migration renames each disk
	// volume for the target storage's naming (fresh disk index).
	renameOnMigrate bool
}

type migRecord struct {
	vmid           int
	source, target string
	withLocalDisks bool
	targetStorage  string
}

func newMigFakeClient(configs map[int]map[string]any, nodes map[int]string) *migFakeClient {
	return &migFakeClient{configs: configs, nodes: nodes, renameN: map[int]int{}}
}

func (c *migFakeClient) Pools() pve.PoolService                 { return nil }
func (c *migFakeClient) ClusterStorage() clusterstorage.Service { return nil }
func (c *migFakeClient) QEMU() qemu.Service                     { return &migFakeQEMU{c: c} }
func (c *migFakeClient) Nodes() sdknodes.Service                { return &migFakeNodes{c: c} }
func (c *migFakeClient) Cluster() sdkcluster.Service            { return &migFakeCluster{c: c} }

func (c *migFakeClient) bareOf(val string) string {
	if comma := strings.IndexByte(val, ','); comma >= 0 {
		return val[:comma]
	}
	return val
}

// vmids returns the VMIDs present, for assertions about creation and deletion.
func (c *migFakeClient) vmids() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]int, 0, len(c.configs))
	for vmid := range c.configs {
		out = append(out, vmid)
	}
	return out
}

type migFakeQEMU struct {
	qemu.Service
	c *migFakeClient
}

func (q *migFakeQEMU) Config(_ context.Context, node string, vmid int) (map[string]any, error) {
	q.c.mu.Lock()
	defer q.c.mu.Unlock()
	cfg, ok := q.c.configs[vmid]
	if !ok || q.c.nodes[vmid] != node {
		return nil, fmt.Errorf("migFake: no config for vmid %d on node %s: %w", vmid, node, sdkerrors.ErrNotFound)
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		out[k] = v
	}
	return out, nil
}

func (q *migFakeQEMU) Create(_ context.Context, node string, params map[string]any) (string, error) {
	q.c.mu.Lock()
	defer q.c.mu.Unlock()
	vmid, _ := params["vmid"].(int)
	if vmid <= 0 {
		return "", fmt.Errorf("migFake: create without vmid")
	}
	if _, exists := q.c.configs[vmid]; exists {
		return "", fmt.Errorf("migFake: unable to create VM %d: config file already exists", vmid)
	}
	cfg := map[string]any{}
	for _, key := range []string{"name", "tags"} {
		if v, ok := params[key].(string); ok {
			cfg[key] = v
		}
	}
	if v, ok := params["protection"].(int); ok {
		cfg["protection"] = v == 1
	}
	q.c.configs[vmid] = cfg
	q.c.nodes[vmid] = node
	return "", nil
}

func (q *migFakeQEMU) AttachDisk(_ context.Context, node string, vmid int, volid, _ string, opts *qemu.AttachOpts) (string, error) {
	q.c.mu.Lock()
	defer q.c.mu.Unlock()
	if q.c.nodes[vmid] != node {
		return "", fmt.Errorf("migFake: attach on wrong node %s for vmid %d: %w", node, vmid, sdkerrors.ErrNotFound)
	}
	cfg := q.c.configs[vmid]
	slot := ""
	if opts != nil {
		slot = opts.DiskID
	}
	cfg[slot] = volid
	return slot, nil
}

func (q *migFakeQEMU) DetachDisk(_ context.Context, node string, vmid int, diskID string) error {
	q.c.mu.Lock()
	defer q.c.mu.Unlock()
	if q.c.nodes[vmid] != node {
		return fmt.Errorf("migFake: detach on wrong node %s for vmid %d: %w", node, vmid, sdkerrors.ErrNotFound)
	}
	cfg := q.c.configs[vmid]
	raw, present := cfg[diskID]
	if !present {
		return nil
	}
	bare := q.c.bareOf(raw.(string))
	delete(cfg, diskID)
	if owner, ok := pve.EmbeddedDiskVMID(bare); ok && owner == vmid {
		q.c.destroyed = append(q.c.destroyed, bare)
	}
	return nil
}

type migFakeNodes struct {
	sdknodes.Service
	c *migFakeClient
}

func (n *migFakeNodes) ListStorageContent(_ context.Context, _ string, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
	resp := sdknodes.ListStorageContentResponse{}
	return &resp, nil
}

func (n *migFakeNodes) UpdateQemuConfig(_ context.Context, node string, vmidStr string, params *sdknodes.UpdateQemuConfigParams) error {
	n.c.mu.Lock()
	defer n.c.mu.Unlock()
	vmid, _ := strconv.Atoi(vmidStr)
	cfg, ok := n.c.configs[vmid]
	if !ok || n.c.nodes[vmid] != node {
		return fmt.Errorf("migFake: no config for vmid %s on node %s: %w", vmidStr, node, sdkerrors.ErrNotFound)
	}
	if params.Delete != nil {
		slot := *params.Delete
		if raw, present := cfg[slot]; present {
			bare := n.c.bareOf(raw.(string))
			delete(cfg, slot)
			if !strings.HasPrefix(slot, "unused") {
				for i := 0; ; i++ {
					key := fmt.Sprintf("unused%d", i)
					if _, taken := cfg[key]; !taken {
						cfg[key] = bare
						break
					}
				}
			}
		}
	}
	if params.Description != nil {
		cfg["description"] = *params.Description
	}
	if params.Protection != nil {
		cfg["protection"] = *params.Protection
	}
	return nil
}

func (n *migFakeNodes) CreateQemuMoveDisk(_ context.Context, node string, vmidStr string, params *sdknodes.CreateQemuMoveDiskParams) (*sdknodes.CreateQemuMoveDiskResponse, error) {
	n.c.mu.Lock()
	defer n.c.mu.Unlock()
	srcVMID, _ := strconv.Atoi(vmidStr)
	target := int(*params.TargetVmid)
	if n.c.nodes[srcVMID] != n.c.nodes[target] {
		return nil, fmt.Errorf("migFake: Both VMs need to be on the same node")
	}
	srcCfg := n.c.configs[srcVMID]
	raw, present := srcCfg[params.Disk]
	if !present {
		return nil, fmt.Errorf("migFake: source %s has no key %s", vmidStr, params.Disk)
	}
	val := raw.(string)
	bare := n.c.bareOf(val)
	opts := strings.TrimPrefix(val, bare)
	if strings.HasPrefix(params.Disk, "unused") {
		opts = ""
	}
	storage := bare
	if i := strings.IndexByte(bare, ':'); i >= 0 {
		storage = bare[:i]
	}
	landed := fmt.Sprintf("%s:vm-%d-disk-%d", storage, target, n.c.renameN[target])
	n.c.renameN[target]++
	delete(srcCfg, params.Disk)
	n.c.configs[target][*params.TargetDisk] = landed + opts
	_ = node
	resp := json.RawMessage(`""`)
	return &resp, nil
}

func (n *migFakeNodes) CreateQemuMigrate(_ context.Context, node string, vmidStr string, params *sdknodes.CreateQemuMigrateParams) (*sdknodes.CreateQemuMigrateResponse, error) {
	n.c.mu.Lock()
	defer n.c.mu.Unlock()
	if n.c.migrateErr != nil {
		err := n.c.migrateErr
		n.c.migrateErr = nil
		return nil, err
	}
	vmid, _ := strconv.Atoi(vmidStr)
	if n.c.nodes[vmid] != node {
		return nil, fmt.Errorf("migFake: migrate addressed to wrong node %s for vmid %d: %w", node, vmid, sdkerrors.ErrNotFound)
	}
	rec := migRecord{vmid: vmid, source: node, target: params.Target}
	if params.WithLocalDisks != nil {
		rec.withLocalDisks = *params.WithLocalDisks
	}
	if params.Targetstorage != nil {
		rec.targetStorage = *params.Targetstorage
	}
	n.c.migrations = append(n.c.migrations, rec)
	n.c.nodes[vmid] = params.Target
	if n.c.renameOnMigrate {
		cfg := n.c.configs[vmid]
		for slot, raw := range cfg {
			val, ok := raw.(string)
			if !ok || !strings.HasPrefix(slot, "scsi") {
				continue
			}
			bare := n.c.bareOf(val)
			opts := strings.TrimPrefix(val, bare)
			storage := bare
			if i := strings.IndexByte(bare, ':'); i >= 0 {
				storage = bare[:i]
			}
			renamed := fmt.Sprintf("%s:vm-%d-disk-%d", storage, vmid, n.c.renameN[vmid])
			n.c.renameN[vmid]++
			cfg[slot] = renamed + opts
		}
	}
	resp := json.RawMessage(`""`)
	return &resp, nil
}

func (n *migFakeNodes) DeleteQemu(_ context.Context, node string, vmidStr string, _ *sdknodes.DeleteQemuParams) (*sdknodes.DeleteQemuResponse, error) {
	n.c.mu.Lock()
	defer n.c.mu.Unlock()
	vmid, _ := strconv.Atoi(vmidStr)
	if _, ok := n.c.configs[vmid]; !ok || n.c.nodes[vmid] != node {
		return nil, fmt.Errorf("migFake: no VM %d on node %s: %w", vmid, node, sdkerrors.ErrNotFound)
	}
	delete(n.c.configs, vmid)
	delete(n.c.nodes, vmid)
	n.c.deletedVMs = append(n.c.deletedVMs, vmid)
	resp := json.RawMessage(`""`)
	return &resp, nil
}

type migFakeCluster struct {
	sdkcluster.Service
	c *migFakeClient
}

func (cl *migFakeCluster) ListResources(context.Context, *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
	cl.c.mu.Lock()
	defer cl.c.mu.Unlock()
	var resp sdkcluster.ListResourcesResponse
	for vmid, cfg := range cl.c.configs {
		tags, _ := cfg["tags"].(string)
		b, err := json.Marshal(map[string]any{
			"vmid": vmid, "node": cl.c.nodes[vmid], "type": "qemu", "tags": tags,
		})
		if err != nil {
			return nil, err
		}
		resp = append(resp, b)
	}
	return &resp, nil
}

// migFakeBackendResolver classifies every storage with a fixed kind and
// reports the disk's node from the fake cluster (local NodeForExisting).
type migFakeBackendResolver struct {
	kind pve.BackendKind
	node string
}

func (r *migFakeBackendResolver) Resolve(context.Context, string) (pve.Backend, error) {
	return &migFakeBackend{kind: r.kind, node: r.node}, nil
}

type migFakeBackend struct {
	kind pve.BackendKind
	node string
}

func (b *migFakeBackend) Kind() pve.BackendKind { return b.kind }
func (b *migFakeBackend) NodeForCreate(context.Context, string, string) (string, error) {
	return b.node, nil
}
func (b *migFakeBackend) NodeForExisting(context.Context, string) (string, error) {
	return b.node, nil
}

func migTestDeps(c *migFakeClient) Deps {
	return Deps{
		Config: &config.CPIConfig{Node: "pve1", DiskStorage: "data"},
		PVE:    c,
	}
}

// findMoverVMID returns the VMID of the single mover-tagged VM, or 0.
func findMoverVMID(t *testing.T, c *migFakeClient) int {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	found := 0
	for vmid, cfg := range c.configs {
		tags, _ := cfg["tags"].(string)
		if pve.TagsMarkDiskMover(tags) {
			if found != 0 {
				t.Fatalf("two mover VMs present: %d and %d", found, vmid)
			}
			found = vmid
		}
	}
	return found
}

func TestAttachDiskResolveNode_CrossNodeLocalBackend(t *testing.T) {
	t.Parallel()

	// VM 700 runs on pve2; the local backend reports the disk on pve1.
	newDeps := func() Deps {
		c := newMigFakeClient(map[int]map[string]any{700: {}}, map[int]string{700: "pve2"})
		deps := migTestDeps(c)
		deps.Resolver = &migFakeBackendResolver{kind: pve.BackendLocal, node: "pve1"}
		return deps
	}

	t.Run("migration off refuses naming the knob", func(t *testing.T) {
		t.Parallel()
		deps := newDeps()
		deps.Config.DiskMigration = config.DiskMigrationOff
		_, _, err := attachDiskResolveNode(context.Background(), deps, "700", "data:vm-9001-disk-0", migTestToken)
		if err == nil || !strings.Contains(err.Error(), "disabled by configuration") ||
			!strings.Contains(err.Error(), "pve.disk_migration") {
			t.Fatalf("err = %v, want the migration-disabled refusal naming the knob", err)
		}
	})

	t.Run("legacy disk refused even with migration on", func(t *testing.T) {
		t.Parallel()
		deps := newDeps()
		_, _, err := attachDiskResolveNode(context.Background(), deps, "700", "data:vm-9001-disk-0", "")
		if err == nil || !strings.Contains(err.Error(), "legacy disk") {
			t.Fatalf("err = %v, want the legacy-disk refusal", err)
		}
	})

	t.Run("stable-ID disk retargets to the VM's node", func(t *testing.T) {
		t.Parallel()
		deps := newDeps()
		node, vmid, err := attachDiskResolveNode(context.Background(), deps, "700", "data:vm-9001-disk-0", migTestToken)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if node != "pve2" || vmid != 700 {
			t.Errorf("(node, vmid) = (%q, %d), want (pve2, 700)", node, vmid)
		}
	})
}

// TestAttachCrossNode_MigratesViaMover drives the full mover flow against
// shared storage: a parker-named volume on a pve1 parker, the VM on pve2. The
// disk must be isolated onto a fresh mover (sibling disks untouched), the
// mover migrated to pve2 without a local-disk copy, the disk attached to the
// VM by the ordinary reassignment, and the mover destroyed.
func TestAttachCrossNode_MigratesViaMover(t *testing.T) {
	t.Parallel()

	sibling := "data:vm-90000-disk-1,serial=bpd-9999888877776666"
	c := newMigFakeClient(map[int]map[string]any{
		700: {},
		90000: {
			"tags":       "bosh-cpi;bosh-parker",
			"protection": true,
			"scsi0":      "data:vm-90000-disk-0,serial=" + migTestToken,
			"scsi1":      sibling,
		},
	}, map[int]string{700: "pve2", 90000: "pve1"})
	deps := migTestDeps(c)

	meta := &pve.DiskCIDMeta{ID: migTestToken, Anchor: true}
	holder := pve.DiskHolder{Found: true, VMID: 90000, Node: "pve1", IsParker: true, Slot: "scsi0", Tags: "bosh-cpi;bosh-parker"}
	rd := resolvedDisk{
		diskCID: "pvd-x", birth: "data:vm-9001-disk-0", volid: "data:vm-90000-disk-0",
		meta: meta, stableID: migTestToken, holder: &holder,
	}

	plan, err := guardAndUnparkBeforeAttach(context.Background(), deps, "attach_disk", &rd, "pve2", 700)
	if err != nil {
		t.Fatalf("guardAndUnparkBeforeAttach: %v", err)
	}
	if !plan.viaTransfer || !plan.destroyMover {
		t.Fatalf("plan = %+v, want a transfer plan that destroys its mover", plan)
	}
	moverVMID := plan.parker.VMID
	if moverVMID == 90000 || moverVMID < 90000 || moverVMID > 90999 {
		t.Fatalf("mover VMID = %d, want a fresh VMID in the parker band distinct from the shared parker", moverVMID)
	}
	if plan.parker.Node != "pve2" {
		t.Errorf("mover node = %q, want pve2 (migrated)", plan.parker.Node)
	}
	if len(c.migrations) != 1 {
		t.Fatalf("migrations = %+v, want exactly one", c.migrations)
	}
	if m := c.migrations[0]; m.vmid != moverVMID || m.source != "pve1" || m.target != "pve2" || m.withLocalDisks {
		t.Errorf("migration = %+v, want the mover offline-migrated pve1→pve2 with no local-disk copy (shared storage)", m)
	}
	// The sibling disk never travels: still on the shared parker, on pve1.
	if got, _ := c.configs[90000]["scsi1"].(string); got != sibling {
		t.Errorf("sibling disk = %q, want untouched %q", got, sibling)
	}
	if c.nodes[90000] != "pve1" {
		t.Errorf("shared parker moved to %q; a durable parker must never migrate", c.nodes[90000])
	}
	if !strings.HasPrefix(rd.volid, "data:vm-"+strconv.Itoa(moverVMID)+"-disk-") {
		t.Errorf("rd.volid = %q, want renamed for mover %d", rd.volid, moverVMID)
	}

	diskID, devPath, err := attachDiskCore(context.Background(), deps, "attach_disk", "700", "pve2", 700, "pvd-x", rd, plan)
	if err != nil {
		t.Fatalf("attachDiskCore: %v", err)
	}
	if diskID != "scsi1" || !strings.Contains(devPath, "scsi1") {
		t.Errorf("diskID=%q devPath=%q", diskID, devPath)
	}
	got, _ := c.configs[700][diskID].(string)
	if !strings.Contains(got, "serial="+migTestToken) {
		t.Errorf("attached drive string %q lacks the identity serial", got)
	}
	// The mover is gone; the shared parker and its sibling disk are not.
	if mover := findMoverVMID(t, c); mover != 0 {
		t.Errorf("mover %d still present after the attach", mover)
	}
	if len(c.deletedVMs) != 1 || c.deletedVMs[0] != moverVMID {
		t.Errorf("deletedVMs = %v, want exactly the mover %d", c.deletedVMs, moverVMID)
	}
	if len(c.destroyed) != 0 {
		t.Errorf("volumes destroyed: %v", c.destroyed)
	}
	if _, ok := c.configs[90000]; !ok {
		t.Error("shared parker destroyed")
	}
}

// TestAttachCrossNode_LocalBackend_CopiesLocalDisks drives the mover flow for
// a birth-named volume on a node-local backend: the migrate request must ask
// PVE to copy local disks mapping each storage to itself, and the rename the
// copy performs must not lose the disk — the serial carries the identity.
func TestAttachCrossNode_LocalBackend_CopiesLocalDisks(t *testing.T) {
	t.Parallel()

	c := newMigFakeClient(map[int]map[string]any{
		700: {},
		90000: {
			"tags":       "bosh-cpi;bosh-parker",
			"protection": true,
			"scsi0":      "data:vm-9001-disk-0,serial=" + migTestToken,
		},
	}, map[int]string{700: "pve2", 90000: "pve1"})
	c.renameOnMigrate = true
	deps := migTestDeps(c)
	deps.Resolver = &migFakeBackendResolver{kind: pve.BackendLocal, node: "pve1"}

	meta := &pve.DiskCIDMeta{ID: migTestToken, Anchor: true}
	holder := pve.DiskHolder{Found: true, VMID: 90000, Node: "pve1", IsParker: true, Slot: "scsi0", Tags: "bosh-cpi;bosh-parker"}
	rd := resolvedDisk{
		diskCID: "pvd-x", birth: "data:vm-9001-disk-0", volid: "data:vm-9001-disk-0",
		meta: meta, stableID: migTestToken, holder: &holder,
	}

	plan, err := guardAndUnparkBeforeAttach(context.Background(), deps, "attach_disk", &rd, "pve2", 700)
	if err != nil {
		t.Fatalf("guardAndUnparkBeforeAttach: %v", err)
	}
	if len(c.migrations) != 1 {
		t.Fatalf("migrations = %+v, want exactly one", c.migrations)
	}
	if m := c.migrations[0]; !m.withLocalDisks || m.targetStorage != "1" {
		t.Errorf("migration = %+v, want with-local-disks and targetstorage \"1\"", m)
	}

	if _, _, err := attachDiskCore(context.Background(), deps, "attach_disk", "700", "pve2", 700, "pvd-x", rd, plan); err != nil {
		t.Fatalf("attachDiskCore: %v", err)
	}
	got, _ := c.configs[700]["scsi1"].(string)
	if !strings.Contains(got, "serial="+migTestToken) {
		t.Errorf("attached drive string %q lacks the identity serial", got)
	}
	if mover := findMoverVMID(t, c); mover != 0 {
		t.Errorf("mover %d still present after the attach", mover)
	}
	if len(c.destroyed) != 0 {
		t.Errorf("volumes destroyed: %v", c.destroyed)
	}
}

// TestAttachCrossNode_AdoptsInterruptedMover covers re-entry: a previous run
// isolated the disk onto a mover and crashed before (or during) the
// migration. The retried attach must adopt that mover — never stack a second
// one — migrate it, and finish.
func TestAttachCrossNode_AdoptsInterruptedMover(t *testing.T) {
	t.Parallel()

	moverTags := "bosh-cpi;bosh-parker;bosh-disk-mover"
	c := newMigFakeClient(map[int]map[string]any{
		700: {},
		90007: {
			"tags":       moverTags,
			"protection": true,
			"scsi0":      "data:vm-90007-disk-0,serial=" + migTestToken,
		},
	}, map[int]string{700: "pve2", 90007: "pve1"})
	deps := migTestDeps(c)

	meta := &pve.DiskCIDMeta{ID: migTestToken, Anchor: true}
	holder := pve.DiskHolder{Found: true, VMID: 90007, Node: "pve1", IsParker: true, Slot: "scsi0", Tags: moverTags}
	rd := resolvedDisk{
		diskCID: "pvd-x", birth: "data:vm-9001-disk-0", volid: "data:vm-90007-disk-0",
		meta: meta, stableID: migTestToken, holder: &holder,
	}

	plan, err := guardAndUnparkBeforeAttach(context.Background(), deps, "attach_disk", &rd, "pve2", 700)
	if err != nil {
		t.Fatalf("guardAndUnparkBeforeAttach: %v", err)
	}
	if plan.parker.VMID != 90007 {
		t.Fatalf("plan adopted VMID %d, want the existing mover 90007", plan.parker.VMID)
	}
	if len(c.migrations) != 1 || c.migrations[0].vmid != 90007 {
		t.Fatalf("migrations = %+v, want exactly one for the adopted mover", c.migrations)
	}
	// No second mover was created: only the VM and the adopted mover exist.
	if ids := c.vmids(); len(ids) != 2 {
		t.Fatalf("vmids = %v, want exactly the VM and the adopted mover", ids)
	}

	if _, _, err := attachDiskCore(context.Background(), deps, "attach_disk", "700", "pve2", 700, "pvd-x", rd, plan); err != nil {
		t.Fatalf("attachDiskCore: %v", err)
	}
	if _, ok := c.configs[90007]; ok {
		t.Error("adopted mover still present after the attach")
	}
	if len(c.destroyed) != 0 {
		t.Errorf("volumes destroyed: %v", c.destroyed)
	}
}

// TestAttachCrossNode_AdoptedMoverAlreadyOnTargetNode covers the crash window
// between migration and destroy: the mover (and disk) already sit on the
// VM's node. The same-node branch must plan the ordinary transfer AND still
// destroy the mover afterwards.
func TestAttachCrossNode_AdoptedMoverAlreadyOnTargetNode(t *testing.T) {
	t.Parallel()

	moverTags := "bosh-cpi;bosh-parker;bosh-disk-mover"
	c := newMigFakeClient(map[int]map[string]any{
		700: {},
		90007: {
			"tags":       moverTags,
			"protection": true,
			"scsi0":      "data:vm-90007-disk-0,serial=" + migTestToken,
		},
	}, map[int]string{700: "pve2", 90007: "pve2"})
	deps := migTestDeps(c)

	meta := &pve.DiskCIDMeta{ID: migTestToken, Anchor: true}
	holder := pve.DiskHolder{Found: true, VMID: 90007, Node: "pve2", IsParker: true, Slot: "scsi0", Tags: moverTags}
	rd := resolvedDisk{
		diskCID: "pvd-x", birth: "data:vm-9001-disk-0", volid: "data:vm-90007-disk-0",
		meta: meta, stableID: migTestToken, holder: &holder,
	}

	plan, err := guardAndUnparkBeforeAttach(context.Background(), deps, "attach_disk", &rd, "pve2", 700)
	if err != nil {
		t.Fatalf("guardAndUnparkBeforeAttach: %v", err)
	}
	if !plan.viaTransfer || !plan.destroyMover {
		t.Fatalf("plan = %+v, want the same-node transfer that destroys its mover", plan)
	}
	if len(c.migrations) != 0 {
		t.Errorf("migrations = %+v, want none (already on the target node)", c.migrations)
	}
	if _, _, err := attachDiskCore(context.Background(), deps, "attach_disk", "700", "pve2", 700, "pvd-x", rd, plan); err != nil {
		t.Fatalf("attachDiskCore: %v", err)
	}
	if _, ok := c.configs[90007]; ok {
		t.Error("mover still present after the attach")
	}
}

// TestAttachCrossNode_SharedBirthNamed_KeepsCheapPath asserts the mover flow
// does NOT engage where the existing config-edit path is safe and cheap: a
// birth-named volume on shared storage.
func TestAttachCrossNode_SharedBirthNamed_KeepsCheapPath(t *testing.T) {
	t.Parallel()

	c := newMigFakeClient(map[int]map[string]any{
		700: {},
		90000: {
			"tags":       "bosh-cpi;bosh-parker",
			"protection": true,
			"scsi0":      "data:vm-9001-disk-0,serial=" + migTestToken,
		},
	}, map[int]string{700: "pve2", 90000: "pve1"})
	deps := migTestDeps(c)
	deps.Resolver = &migFakeBackendResolver{kind: pve.BackendShared, node: "pve1"}

	meta := &pve.DiskCIDMeta{ID: migTestToken, Anchor: true}
	holder := pve.DiskHolder{Found: true, VMID: 90000, Node: "pve1", IsParker: true, Slot: "scsi0", Tags: "bosh-cpi;bosh-parker"}
	rd := resolvedDisk{
		diskCID: "pvd-x", birth: "data:vm-9001-disk-0", volid: "data:vm-9001-disk-0",
		meta: meta, stableID: migTestToken, holder: &holder,
	}

	plan, err := guardAndUnparkBeforeAttach(context.Background(), deps, "attach_disk", &rd, "pve2", 700)
	if err != nil {
		t.Fatalf("guardAndUnparkBeforeAttach: %v", err)
	}
	if plan.viaTransfer || plan.destroyMover {
		t.Fatalf("plan = %+v, want the config-edit path (no transfer, no mover)", plan)
	}
	if len(c.migrations) != 0 {
		t.Errorf("migrations = %+v, want none", c.migrations)
	}
	if mover := findMoverVMID(t, c); mover != 0 {
		t.Errorf("mover %d created for a shared birth-named volume", mover)
	}
	// The unpark ran: the parker no longer references the volume.
	if _, present := c.configs[90000]["scsi0"]; present {
		t.Error("parker still holds the volume after the unpark")
	}
}
