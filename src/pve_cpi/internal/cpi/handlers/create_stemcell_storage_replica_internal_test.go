package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
)

func TestStorageSetReplicasNeeded(t *testing.T) {
	t.Parallel()
	on := Deps{Config: &config.CPIConfig{StemcellReplicateStorageSet: boolPtr(true), EphemeralStorageSet: "eph"}}
	if set, ok := storageSetReplicasNeeded(on, "abcd1234ef"); !ok || set != "eph" {
		t.Fatalf("want eph/true, got %q/%v", set, ok)
	}
	if _, ok := storageSetReplicasNeeded(on, ""); ok {
		t.Fatal("empty sha must disable replicas")
	}
	if _, ok := storageSetReplicasNeeded(on, "abc"); ok {
		t.Fatal("a digest shorter than eight characters must disable replicas")
	}
	// Unset is the shipping default, and a bound set is the whole reason the
	// default is on, so this is the case most deployments actually take.
	dflt := Deps{Config: &config.CPIConfig{EphemeralStorageSet: "eph"}}
	if set, ok := storageSetReplicasNeeded(dflt, "abcd1234ef"); !ok || set != "eph" {
		t.Fatalf("an unset property with a bound set must replicate, got %q/%v", set, ok)
	}
	off := Deps{Config: &config.CPIConfig{StemcellReplicateStorageSet: boolPtr(false), EphemeralStorageSet: "eph"}}
	if _, ok := storageSetReplicasNeeded(off, "abcd1234ef"); ok {
		t.Fatal("an explicit false must disable replicas")
	}
	imported := Deps{Config: &config.CPIConfig{EphemeralStorageSet: "eph", StemcellStrategy: config.StemcellStrategyImport}}
	if _, ok := storageSetReplicasNeeded(imported, "abcd1234ef"); ok {
		t.Fatal("the import strategy builds no templates, so it must disable replicas")
	}
	unbound := Deps{Config: &config.CPIConfig{StemcellReplicateStorageSet: boolPtr(true)}}
	if _, ok := storageSetReplicasNeeded(unbound, "abcd1234ef"); ok {
		t.Fatal("no bound set must disable replicas")
	}
	if _, ok := storageSetReplicasNeeded(Deps{}, "abcd1234ef"); ok {
		t.Fatal("nil config must disable replicas")
	}
}

func replicaStorageDef(t *testing.T, id, export string, shared bool, content string) pve.StorageInfo {
	t.Helper()
	// An NFS storage is shared by protocol whatever its "shared" flag says
	// (pve.StorageInfo.IsShared), so a node-local member has to be a
	// plugin that is not inherently shared. "dir" is the one the fan-out
	// most plausibly meets on a set an operator wrote by hand.
	row := map[string]any{"storage": id, "type": "nfs", "shared": 1, "server": "nas", "export": export, "content": content}
	if !shared {
		row = map[string]any{"storage": id, "type": "dir", "shared": 0, "path": export, "content": content}
	}
	def, err := pve.ParseStorageEntry(planJSON(t, row))
	if err != nil {
		t.Fatal(err)
	}
	return def
}

func TestFilterStorageReplicaMembers(t *testing.T) {
	t.Parallel()
	defs := map[string]pve.StorageInfo{
		"ns_1":       replicaStorageDef(t, "ns_1", "/ns1", true, "images,import"),
		"ns_2":       replicaStorageDef(t, "ns_2", "/ns2", true, "images"),
		"ns_1_alias": replicaStorageDef(t, "ns_1_alias", "/ns1", true, "images"),
		"ns_3":       replicaStorageDef(t, "ns_3", "/ns3", false, "images"),
		"ns_4":       replicaStorageDef(t, "ns_4", "/ns4", true, "iso"),
		"ns-2":       replicaStorageDef(t, "ns-2", "/ns2b", true, "images"),
	}
	definition := func(id string) (pve.StorageInfo, bool) { d, ok := defs[id]; return d, ok }
	members := []string{"ns_1", "ns_2", "ns_1_alias", "ns_3", "ns_4", "ns-2", "ghost"}
	got := filterStorageReplicaMembers(log.NewNopLogger(), members, definition, "ns_1")
	want := []storageReplicaTarget{
		{StorageID: "ns_2", Tag: "bosh-stemcell-storage-ns-2"},
		{StorageID: "ns_1_alias", Tag: "bosh-stemcell-storage-ns-1-alias"},
	}
	if len(got) != len(want) {
		t.Fatalf("want %+v, got %+v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d: want %+v, got %+v", i, want[i], got[i])
		}
	}
}

func TestStorageSetReplicaTargets(t *testing.T) {
	t.Parallel()
	src := &planFixtureSource{statuses: map[string][]json.RawMessage{}}
	add := func(id, server, export string, shared int, content string) {
		src.defs = append(src.defs, planJSON(t, map[string]any{"storage": id, "type": "nfs", "shared": shared, "server": server, "export": export, "content": content}))
		src.statuses["n1"] = append(src.statuses["n1"], planJSON(t, map[string]any{"storage": id, "active": 1, "enabled": 1, "total": uint64(100) << 30, "avail": uint64(80) << 30}))
	}
	add("ns_1", "nas", "/ns1", 1, "images,import")
	add("ns_2", "nas", "/ns2", 1, "images")
	add("ns_5", "nas", "/ns5", 1, "images")
	cfg := &config.CPIConfig{VMStorage: "ns_1", EphemeralStorageSet: "eph", StemcellReplicateStorageSet: boolPtr(true),
		StorageSets: map[string]config.StorageSet{"eph": {Names: []string{"ns_1", "ns_2", "ns_5"},
			Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}}}}
	deps := Deps{Config: cfg, Logger: log.NewNopLogger(), ReplicaInventory: src}

	got, err := storageSetReplicaTargets(context.Background(), deps, "eph", "n1", "ns_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].StorageID != "ns_2" || got[1].StorageID != "ns_5" {
		t.Fatalf("want ns_2 then ns_5, got %+v", got)
	}
	if got[0].Tag != "bosh-stemcell-storage-ns-2" || got[1].Tag != "bosh-stemcell-storage-ns-5" {
		t.Fatalf("tags: %+v", got)
	}
}

func TestStorageSetReplicaTargets_DiscoverErrorIsReturned(t *testing.T) {
	t.Parallel()
	src := &planFixtureSource{statuses: map[string][]json.RawMessage{}}
	src.defs = append(src.defs, planJSON(t, map[string]any{"storage": "ns_1", "type": "nfs", "shared": 1, "server": "nas", "export": "/ns1", "content": "images,import"}))
	src.statuses["n1"] = append(src.statuses["n1"], planJSON(t, map[string]any{"storage": "ns_1", "active": 1, "enabled": 1, "total": uint64(100) << 30, "avail": uint64(80) << 30}))
	cfg := &config.CPIConfig{VMStorage: "ns_1", EphemeralStorageSet: "eph", StemcellReplicateStorageSet: boolPtr(true),
		StorageSets: map[string]config.StorageSet{"eph": {Names: []string{"ns_1", "ns_missing"},
			Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}}}}
	deps := Deps{Config: cfg, Logger: log.NewNopLogger(), ReplicaInventory: src}
	if _, err := storageSetReplicaTargets(context.Background(), deps, "eph", "n1", "ns_1"); err == nil {
		t.Fatal("want a discover error for a member missing from the cluster")
	}
}

// replicaQemuRow renders one /nodes/{node}/qemu row for the tolerant
// cluster scan the storage fan-out runs before it builds anything.
func replicaQemuRow(t *testing.T, vmid int64, name, tags string) json.RawMessage {
	t.Helper()
	return planJSON(t, map[string]any{"vmid": vmid, "name": name, "tags": tags, "template": 1, "status": "stopped"})
}

func TestEnsureStorageReplicaTemplateVM_BuildsOnMember(t *testing.T) {
	t.Parallel()
	var created map[string]any
	var createNode string
	freezeCalled := false
	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, node string, params map[string]any) (string, error) {
			createNode = node
			created = params
			return "", nil
		},
	}
	nodes := &wbTemplateNodes{
		listQemuFn: listQemuEmpty(),
		createQemuTemplateFn: func(_ context.Context, _, _ string, _ *sdknodes.CreateQemuTemplateParams) (*sdknodes.CreateQemuTemplateResponse, error) {
			freezeCalled = true
			raw := sdknodes.CreateQemuTemplateResponse(`""`)
			return &raw, nil
		},
	}
	deps := buildEnsureTemplateDeps(qemu, nodes, &wbMockTasks{}, &wbTemplateStorage{})
	deps.Config.VMStorage = "ns_1"
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{listResourcesFn: listClusterResourcesEmpty()}
	var tags string
	captureTemplateTagsUpdate(t, deps, &tags)

	target := storageReplicaTarget{StorageID: "ns_2", Tag: "bosh-stemcell-storage-ns-2"}
	cp := stemcellCloudProps{Name: "ubuntu-noble", Version: "1.585"}
	vmid, err := ensureStorageReplicaTemplateVM(context.Background(), deps, "pve-node1", target, nil,
		"nfs", "stem.qcow2", "abcd1234ef00", pve.StemcellKindHeavy, ":heavy:nfs:import/stem.qcow2", "dir-a", cp, "test")
	if err != nil {
		t.Fatal(err)
	}
	if vmid < 30000 || vmid > 30999 {
		t.Fatalf("vmid %d outside the template band", vmid)
	}
	if createNode != "pve-node1" {
		t.Fatalf("replica must build on the template node, got %q", createNode)
	}
	root, _ := created[rootDiskKey(deps.Config)].(string)
	if !strings.HasPrefix(root, "ns_2:0,import-from=nfs:import/stem.qcow2") {
		t.Fatalf("root disk must land on the member: %q", root)
	}
	if !strings.Contains(tags, "bosh-stemcell-storage-ns-2") || !strings.Contains(tags, "bosh-stemcell-sha-abcd1234") {
		t.Fatalf("tags: %q", tags)
	}
	if !freezeCalled {
		t.Fatal("replica was not frozen into a template")
	}
}

func TestEnsureStorageReplicaTemplateVM_DedupsFromRefs(t *testing.T) {
	t.Parallel()
	createCalled := false
	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			createCalled = true
			return "", nil
		},
	}
	deps := buildEnsureTemplateDeps(qemu, &wbTemplateNodes{listQemuFn: listQemuEmpty()}, &wbMockTasks{}, &wbTemplateStorage{})
	existing := []pve.TemplateRef{
		{VMID: 30044, Node: "pve-node1", Tags: stemcellCacheTag + ";bosh-stemcell-sha-abcd1234;bosh-stemcell-storage-ns-2"},
	}
	target := storageReplicaTarget{StorageID: "ns_2", Tag: "bosh-stemcell-storage-ns-2"}
	vmid, err := ensureStorageReplicaTemplateVM(context.Background(), deps, "pve-node1", target, existing,
		"nfs", "stem.qcow2", "abcd1234ef00", pve.StemcellKindHeavy, ":heavy:nfs:import/stem.qcow2", "dir-a",
		stemcellCloudProps{Name: "ubuntu-noble", Version: "1.585"}, "test")
	if err != nil || vmid != 30044 {
		t.Fatalf("want dedup hit 30044, got %d err=%v", vmid, err)
	}
	if createCalled {
		t.Fatal("dedup hit must not create")
	}
}

func TestLogStorageReplicationSummary(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := capturingLogger(t, &buf)
	logStorageReplicationSummary(logger, "create_stemcell: storage-set replication", []storageReplicaOutcome{
		{Storage: "ns_2"},
		{Storage: "ns_5", Stage: "ensure-template", Err: errors.New("boom")},
	})
	out := buf.String()
	for _, want := range []string{"1 of 2 members hold a replica", "failed_storages", "ns_5", "error_ns_5", "ensure-template: boom"} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary lacks %q:\n%s", want, out)
		}
	}
}

func TestMaybeReplicateTemplateToStorageSet_BuildsOnEachMember(t *testing.T) {
	t.Parallel()
	const sha8 = "abcd1234"
	const fullSHA = sha8 + "00000000000000000000000000000000000000000000000000000000"
	const primaryVMID = int64(30010)

	var mu sync.Mutex
	var roots []string
	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, params map[string]any) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			root, _ := params["virtio0"].(string)
			roots = append(roots, root)
			return "", nil
		},
		configFn: func(_ context.Context, _ string, vmid int) (map[string]any, error) {
			if int64(vmid) == primaryVMID {
				return map[string]any{"virtio0": "ns_1:base-30010-disk-0.qcow2,size=5G"}, nil
			}
			return map[string]any{}, nil
		},
	}
	nodes := &wbTemplateNodes{
		listQemuFn: func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			resp := sdknodes.ListQemuResponse{
				replicaQemuRow(t, primaryVMID, "bosh-stemcell-ubuntu-noble-1-585-"+sha8,
					stemcellCacheTag+";bosh-stemcell-sha-"+sha8),
			}
			return &resp, nil
		},
	}
	deps := buildEnsureTemplateDeps(qemu, nodes, &wbMockTasks{}, &wbTemplateStorage{})
	deps.Config.VMStorage = "ns_1"
	deps.Config.EphemeralStorageSet = "eph"
	deps.Config.StemcellReplicateStorageSet = boolPtr(true)
	deps.Config.StorageSets = map[string]config.StorageSet{"eph": {Names: []string{"ns_1", "ns_2", "ns_5"},
		Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}}}
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			resp := sdkcluster.ListResourcesResponse{
				clusterResourceQemuTemplate(primaryVMID, "pve-node1", "bosh-stemcell-ubuntu-noble-1-585-"+sha8,
					stemcellCacheTag+";bosh-stemcell-sha-"+sha8),
			}
			return &resp, nil
		},
	}
	src := &planFixtureSource{statuses: map[string][]json.RawMessage{}}
	add := func(id, export, content string) {
		src.defs = append(src.defs, planJSON(t, map[string]any{"storage": id, "type": "nfs", "shared": 1, "server": "nas", "export": export, "content": content}))
		src.statuses["pve-node1"] = append(src.statuses["pve-node1"], planJSON(t, map[string]any{"storage": id, "active": 1, "enabled": 1, "total": uint64(100) << 30, "avail": uint64(80) << 30}))
	}
	add("ns_1", "/ns1", "images,import")
	add("ns_2", "/ns2", "images")
	add("ns_5", "/ns5", "images")
	deps.ReplicaInventory = src

	maybeReplicateTemplateToStorageSet(context.Background(), deps, "pve-node1", "nfs", "stem.qcow2", fullSHA,
		":heavy:nfs:import/stem.qcow2", "dir-a", pve.StemcellKindHeavy, stemcellCloudProps{Name: "ubuntu-noble", Version: "1.585"}, "test")

	sort.Strings(roots)
	if len(roots) != 2 || !strings.HasPrefix(roots[0], "ns_2:0,import-from=nfs:import/stem.qcow2") || !strings.HasPrefix(roots[1], "ns_5:0,import-from=nfs:import/stem.qcow2") {
		t.Fatalf("want one create on ns_2 and one on ns_5, got %v", roots)
	}
}

func TestMaybeReplicateTemplateToStorageSet_SkipsOnRootBusMismatch(t *testing.T) {
	t.Parallel()
	const sha8 = "abcd1234"
	const fullSHA = sha8 + "00000000000000000000000000000000000000000000000000000000"
	createCalled := false
	qemu := &wbMockQEMU{
		createFn: func(_ context.Context, _ string, _ map[string]any) (string, error) {
			createCalled = true
			return "", nil
		},
		configFn: func(_ context.Context, _ string, _ int) (map[string]any, error) {
			return map[string]any{"scsi0": "ns_1:base-30010-disk-0.qcow2,size=5G"}, nil
		},
	}
	nodes := &wbTemplateNodes{
		listQemuFn: func(_ context.Context, _ string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
			resp := sdknodes.ListQemuResponse{
				replicaQemuRow(t, 30010, "bosh-stemcell-ubuntu-noble-1-585-"+sha8,
					stemcellCacheTag+";bosh-stemcell-sha-"+sha8),
			}
			return &resp, nil
		},
	}
	deps := buildEnsureTemplateDeps(qemu, nodes, &wbMockTasks{}, &wbTemplateStorage{})
	deps.Config.VMStorage = "ns_1"
	deps.Config.EphemeralStorageSet = "eph"
	deps.Config.StemcellReplicateStorageSet = boolPtr(true)
	deps.Config.StorageSets = map[string]config.StorageSet{"eph": {Names: []string{"ns_1", "ns_2"},
		Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}}}
	deps.PVE.(*wbTemplateMockClient).clusterSvc = &wbClusterForAlloc{
		listResourcesFn: func(_ context.Context, _ *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
			resp := sdkcluster.ListResourcesResponse{
				clusterResourceQemuTemplate(30010, "pve-node1", "bosh-stemcell-ubuntu-noble-1-585-"+sha8, stemcellCacheTag+";bosh-stemcell-sha-"+sha8),
			}
			return &resp, nil
		},
	}
	src := &planFixtureSource{statuses: map[string][]json.RawMessage{}}
	for _, id := range []string{"ns_1", "ns_2"} {
		src.defs = append(src.defs, planJSON(t, map[string]any{"storage": id, "type": "nfs", "shared": 1, "server": "nas", "export": "/" + id, "content": "images,import"}))
		src.statuses["pve-node1"] = append(src.statuses["pve-node1"], planJSON(t, map[string]any{"storage": id, "active": 1, "enabled": 1, "total": uint64(100) << 30, "avail": uint64(80) << 30}))
	}
	deps.ReplicaInventory = src

	maybeReplicateTemplateToStorageSet(context.Background(), deps, "pve-node1", "nfs", "stem.qcow2", fullSHA,
		":heavy:nfs:import/stem.qcow2", "dir-a", pve.StemcellKindHeavy, stemcellCloudProps{Name: "ubuntu-noble", Version: "1.585"}, "test")
	if createCalled {
		t.Fatal("a primary on scsi0 under a virtio root_disk_bus must skip the whole fan-out")
	}
}
