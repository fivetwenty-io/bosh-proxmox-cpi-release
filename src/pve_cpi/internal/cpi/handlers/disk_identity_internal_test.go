// disk_identity_internal_test.go — white-box tests for the D13 identity seam
// inside the handlers: the attach guard's transfer planning, the config-put
// attach's serial injection, delete_vm's renamed-disk preservation, and the
// source-level invariant that every disk handler routes through the resolver.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	clusterstorage "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"

	"github.com/fivetwenty-io/bosh-pve-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-pve-cpi/internal/pve"
)

const idTestToken = "bpd-aabbccdd00112233"

// idFakeQEMU / idFakeNodes / idFakeCluster + idFakeClient form a compact
// stateful single-node PVE, faithful to the move_disk semantics the live
// spike established (rename on move, options carried on attached-slot moves,
// dropped on unused-entry moves, owned unused volumes physically removed).
type idFakeClient struct {
	pve.Client

	mu        sync.Mutex
	configs   map[int]map[string]any
	destroyed []string
	renameN   map[int]int
}

func newIDFakeClient(configs map[int]map[string]any) *idFakeClient {
	return &idFakeClient{configs: configs, renameN: map[int]int{}}
}

func (c *idFakeClient) Pools() pve.PoolService { return nil }

func (c *idFakeClient) ClusterStorage() clusterstorage.Service { return nil }

func (c *idFakeClient) QEMU() qemu.Service { return &idFakeQEMU{c: c} }

func (c *idFakeClient) Nodes() sdknodes.Service { return &idFakeNodes{c: c} }

func (c *idFakeClient) Cluster() sdkcluster.Service { return &idFakeCluster{c: c} }

func (c *idFakeClient) bareOf(val string) string {
	if comma := strings.IndexByte(val, ','); comma >= 0 {
		return val[:comma]
	}
	return val
}

type idFakeQEMU struct {
	qemu.Service
	c *idFakeClient
}

func (q *idFakeQEMU) Config(_ context.Context, _ string, vmid int) (map[string]any, error) {
	q.c.mu.Lock()
	defer q.c.mu.Unlock()
	cfg, ok := q.c.configs[vmid]
	if !ok {
		return nil, fmt.Errorf("idFake: no config for vmid %d", vmid)
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		out[k] = v
	}
	return out, nil
}

func (q *idFakeQEMU) AttachDisk(_ context.Context, _ string, vmid int, volid, _ string, opts *qemu.AttachOpts) (string, error) {
	q.c.mu.Lock()
	defer q.c.mu.Unlock()
	cfg := q.c.configs[vmid]
	slot := ""
	if opts != nil {
		slot = opts.DiskID
	}
	cfg[slot] = volid
	return slot, nil
}

func (q *idFakeQEMU) DetachDisk(_ context.Context, _ string, vmid int, diskID string) error {
	q.c.mu.Lock()
	defer q.c.mu.Unlock()
	cfg := q.c.configs[vmid]
	raw, present := cfg[diskID]
	if !present {
		return nil
	}
	bare := q.c.bareOf(raw.(string))
	delete(cfg, diskID)
	// SDK detach-and-sweep: an owned volume is physically removed with its
	// unused entry.
	if owner, ok := pve.EmbeddedDiskVMID(bare); ok && owner == vmid {
		q.c.destroyed = append(q.c.destroyed, bare)
	}
	return nil
}

type idFakeNodes struct {
	sdknodes.Service
	c *idFakeClient
}

func (n *idFakeNodes) UpdateQemuConfig(_ context.Context, _ string, vmidStr string, params *sdknodes.UpdateQemuConfigParams) error {
	n.c.mu.Lock()
	defer n.c.mu.Unlock()
	vmid, _ := strconv.Atoi(vmidStr)
	cfg, ok := n.c.configs[vmid]
	if !ok {
		return fmt.Errorf("idFake: no config for vmid %s", vmidStr)
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

func (n *idFakeNodes) CreateQemuMoveDisk(_ context.Context, _ string, vmidStr string, params *sdknodes.CreateQemuMoveDiskParams) (*sdknodes.CreateQemuMoveDiskResponse, error) {
	n.c.mu.Lock()
	defer n.c.mu.Unlock()
	srcVMID, _ := strconv.Atoi(vmidStr)
	srcCfg := n.c.configs[srcVMID]
	raw, present := srcCfg[params.Disk]
	if !present {
		return nil, fmt.Errorf("idFake: source %s has no key %s", vmidStr, params.Disk)
	}
	val := raw.(string)
	bare := n.c.bareOf(val)
	opts := strings.TrimPrefix(val, bare)
	if strings.HasPrefix(params.Disk, "unused") {
		opts = ""
	}
	target := int(*params.TargetVmid)
	storage := bare
	if i := strings.IndexByte(bare, ':'); i >= 0 {
		storage = bare[:i]
	}
	landed := fmt.Sprintf("%s:vm-%d-disk-%d", storage, target, n.c.renameN[target])
	n.c.renameN[target]++
	delete(srcCfg, params.Disk)
	n.c.configs[target][*params.TargetDisk] = landed + opts
	resp := json.RawMessage(`""`)
	return &resp, nil
}

type idFakeCluster struct {
	sdkcluster.Service
	c *idFakeClient
}

func (cl *idFakeCluster) ListResources(context.Context, *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
	cl.c.mu.Lock()
	defer cl.c.mu.Unlock()
	var resp sdkcluster.ListResourcesResponse
	for vmid, cfg := range cl.c.configs {
		tags, _ := cfg["tags"].(string)
		b, err := json.Marshal(map[string]any{"vmid": vmid, "node": "pve1", "type": "qemu", "tags": tags})
		if err != nil {
			return nil, err
		}
		resp = append(resp, b)
	}
	return &resp, nil
}

func idTestDeps(c *idFakeClient) Deps {
	return Deps{
		Config: &config.CPIConfig{Node: "pve1", DiskStorage: "data"},
		PVE:    c,
	}
}

func TestGuardAndUnparkBeforeAttach_TransferPlanning(t *testing.T) {
	t.Parallel()

	meta := &pve.DiskCIDMeta{ID: idTestToken, Anchor: true}

	t.Run("same-node parker plans a reassignment and leaves the disk parked", func(t *testing.T) {
		t.Parallel()
		deps := idTestDeps(newIDFakeClient(nil))
		holder := pve.DiskHolder{Found: true, VMID: 90000, Node: "pve1", IsParker: true, Slot: "scsi0"}
		rd := resolvedDisk{diskCID: "pvd-x", birth: "data:vm-9001-disk-0", volid: "data:vm-90000-disk-0", meta: meta, stableID: idTestToken, holder: &holder}
		plan, err := guardAndUnparkBeforeAttach(context.Background(), deps, "attach_disk", &rd, "pve1", 700)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !plan.viaTransfer || plan.parker.VMID != 90000 {
			t.Errorf("plan = %+v, want the reassignment transfer", plan)
		}
	})

	t.Run("cross-node parker with a parker-named volume is refused", func(t *testing.T) {
		t.Parallel()
		deps := idTestDeps(newIDFakeClient(nil))
		holder := pve.DiskHolder{Found: true, VMID: 90000, Node: "pve2", IsParker: true, Slot: "scsi0"}
		rd := resolvedDisk{diskCID: "pvd-x", birth: "data:vm-9001-disk-0", volid: "data:vm-90000-disk-0", meta: meta, stableID: idTestToken, holder: &holder}
		_, err := guardAndUnparkBeforeAttach(context.Background(), deps, "attach_disk", &rd, "pve1", 700)
		if err == nil || !strings.Contains(err.Error(), "same-node only") {
			t.Fatalf("err = %v, want the same-node-only refusal", err)
		}
	})

	t.Run("foreign non-parker holder is refused", func(t *testing.T) {
		t.Parallel()
		deps := idTestDeps(newIDFakeClient(nil))
		holder := pve.DiskHolder{Found: true, VMID: 800, Node: "pve1"}
		rd := resolvedDisk{diskCID: "pvd-x", birth: "data:vm-9001-disk-0", volid: "data:vm-800-disk-1", meta: meta, stableID: idTestToken, holder: &holder}
		_, err := guardAndUnparkBeforeAttach(context.Background(), deps, "attach_disk", &rd, "pve1", 700)
		if err == nil || !strings.Contains(err.Error(), "already attached") {
			t.Fatalf("err = %v, want the foreign-holder refusal", err)
		}
	})
}

func TestAttachDiskCore_ConfigPutInjectsSerial(t *testing.T) {
	t.Parallel()

	c := newIDFakeClient(map[int]map[string]any{700: {}})
	deps := idTestDeps(c)
	rd := resolvedDisk{
		diskCID:  "pvd-x",
		birth:    "data:vm-9001-disk-0",
		volid:    "data:vm-9001-disk-0",
		meta:     &pve.DiskCIDMeta{ID: idTestToken},
		stableID: idTestToken,
	}
	diskID, devPath, err := attachDiskCore(context.Background(), deps, "attach_disk", "700", "pve1", 700, "pvd-x", rd, attachPlan{})
	if err != nil {
		t.Fatalf("attachDiskCore: %v", err)
	}
	if diskID != "scsi1" || !strings.Contains(devPath, "scsi1") {
		t.Errorf("diskID=%q devPath=%q", diskID, devPath)
	}
	got, _ := c.configs[700]["scsi1"].(string)
	if !strings.Contains(got, "serial="+idTestToken) {
		t.Errorf("attached drive string %q lacks the identity serial", got)
	}
	if !strings.HasPrefix(got, "data:vm-9001-disk-0") {
		t.Errorf("attached drive string %q must lead with the volid", got)
	}
}

// TestDetachForeignActiveDisks_RenamedDiskPreserved is the D13 delete_vm
// regression: a persistent disk a reassignment renamed for the VM being
// deleted must be recognized as foreign (by its serial) and preserved by
// transferring it to a parker — a plain detach's owned-unused sweep would
// deallocate it.
func TestDetachForeignActiveDisks_RenamedDiskPreserved(t *testing.T) {
	t.Parallel()

	c := newIDFakeClient(map[int]map[string]any{
		700: {
			"virtio0": "data:vm-700-disk-0,size=20G",
			"scsi1":   "data:vm-700-disk-1,serial=" + idTestToken + ",size=10G",
		},
		90000: {
			"tags":       "bosh-cpi;bosh-parker",
			"protection": true,
		},
	})
	deps := idTestDeps(c)
	if err := detachForeignActiveDisks(context.Background(), deps, "pve1", "700", 700, deps.Log(context.Background())); err != nil {
		t.Fatalf("detachForeignActiveDisks: %v", err)
	}

	if len(c.destroyed) != 0 {
		t.Fatalf("persistent disk destroyed: %v", c.destroyed)
	}
	// The disk left the VM entirely (no active slot, no unused entry) and
	// landed on the parker carrying its serial.
	srcCfg := c.configs[700]
	if _, present := srcCfg["scsi1"]; present {
		t.Error("scsi1 still present on the VM being deleted")
	}
	if len(pve.FindUnusedDiskEntries(srcCfg)) != 0 {
		t.Errorf("unused entries remain on the VM: %v", srcCfg)
	}
	parkerDisks := qemu.ParseDisks(c.configs[90000])
	found := false
	for _, optStr := range parkerDisks {
		if serial, ok := pve.StableIDFromDriveOptStr(optStr); ok && serial == idTestToken {
			found = true
			if !strings.HasPrefix(optStr, "data:vm-90000-disk-") {
				t.Errorf("parker drive %q not renamed for the parker", optStr)
			}
		}
	}
	if !found {
		t.Errorf("parker does not carry the disk's serial; config=%v", c.configs[90000])
	}
	// The VM's own root disk stayed put for the destroy that follows.
	if _, present := srcCfg["virtio0"]; !present {
		t.Error("the VM's own root disk must not be touched")
	}
}

// TestDiskHandlersRouteThroughIdentityResolver enforces the D13 consumer
// invariant at the source level: every handler file that decodes a disk CID
// (decodeDiskCID) must resolve it through the identity seam
// (resolveDiskForOp) before using the volid, and nothing outside the two
// allowlisted files may call pve.ParseEncodedDiskCID directly.
func TestDiskHandlersRouteThroughIdentityResolver(t *testing.T) {
	t.Parallel()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	// decodeDiskCID's own definition; and the placement reader, which
	// consumes only CID metadata (pool/node/AZ) plus the volid's storage
	// prefix — both stable across reassignment renames, so it needs no
	// resolution and must not pay a cluster scan per create_vm.
	rawDecodeAllowlist := map[string]bool{
		"disk_cid_decode.go":     true,
		"create_vm_placement.go": true,
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, readErr := os.ReadFile(name)
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		text := string(src)
		if strings.Contains(text, "pve.ParseEncodedDiskCID(") && !rawDecodeAllowlist[name] {
			t.Errorf("%s calls pve.ParseEncodedDiskCID directly; disk handlers must decode via decodeDiskCID and resolve via resolveDiskForOp", name)
		}
		if name == "disk_cid_decode.go" || name == "disk_identity.go" {
			continue
		}
		if strings.Contains(text, "decodeDiskCID(") && !strings.Contains(text, "resolveDiskForOp(") {
			t.Errorf("%s decodes a disk CID but never resolves it through resolveDiskForOp; a renamed stable-ID volume would be unfindable there", name)
		}
	}
}
