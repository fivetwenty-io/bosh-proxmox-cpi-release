// disk_identity_record_scope_internal_test.go - white-box tests for which
// evidence decides whether a parker's record of a disk's transfer names a
// volume that is still there: a stale record of another disk, storage that
// only some nodes can see, and a record whose volume names no storage.
package pve

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	sdkcluster "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/cluster"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/clusterstorage"
	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	"github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/storage"

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// The /storage entries the node-aware client serves for storage "data".
const (
	localDataStorage      = `{"storage":"data","type":"lvmthin","content":"images"}`
	sharedDataStorage     = `{"storage":"data","type":"rbd","content":"images","shared":1}`
	localDataStoragePVE1  = `{"storage":"data","type":"lvmthin","content":"images","nodes":"pve1"}`
	nodeDownProxyResponse = "595 Errors during connection establishment, proxy handler: No route to host"
)

// nodeClusterClient is a scanFakeClient on a quorate two-node cluster, pve1
// and pve2, where each guest lives on one node and each node's storage holds
// its own volumes. A node in offline is one the cluster reports offline, and
// every read on it fails the way PVE's proxy fails toward a node that is down.
//
// Only pve1 and pve2 are cluster members, so a read on any other node fails
// the way PVE fails toward a node it can't resolve, which is what a node that
// left the cluster but stays in a storage's nodes list gets. A node in
// storageErr answers its storage reads with that error while the node itself
// stays online, the way a storage with no backing on that node answers.
type nodeClusterClient struct {
	*scanFakeClient
	nodeOf       map[int]string
	offline      map[string]bool
	onStorage    map[string]map[string]bool
	storageEntry string
	storageErr   map[string]error
}

// newNodeClusterClient builds the client. rows lists the guests in the order
// their nodes list them, and a guest that nodeOf doesn't name lives on pve1.
func newNodeClusterClient(configs map[int]map[string]any, nodeOf map[int]string, offline map[string]bool,
	onStorage map[string]map[string]bool, storageEntry string, rows ...int,
) *nodeClusterClient {
	inner := copiedClient(configs, rows...)
	inner.configErr = map[int]error{}
	for vmid, node := range nodeOf {
		if offline[node] {
			inner.configErr[vmid] = errors.New(nodeDownProxyResponse)
		}
	}
	return &nodeClusterClient{scanFakeClient: inner, nodeOf: nodeOf, offline: offline, onStorage: onStorage, storageEntry: storageEntry}
}

func (c *nodeClusterClient) node(vmid int) string {
	if node, ok := c.nodeOf[vmid]; ok {
		return node
	}
	return "pve1"
}

// placedRows returns the guest rows with each guest's node, keeping only the
// ones on node when node isn't empty.
func (c *nodeClusterClient) placedRows(node string) ([]json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []json.RawMessage
	for _, row := range c.rows {
		vmid, _ := row["vmid"].(int)
		if node != "" && c.node(vmid) != node {
			continue
		}
		placed := make(map[string]any, len(row))
		for k, v := range row {
			placed[k] = v
		}
		placed["node"] = c.node(vmid)
		b, err := json.Marshal(placed)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

func (c *nodeClusterClient) Nodes() sdknodes.Service {
	inner := c.scanFakeClient.Nodes().(*fakeNodesService)
	inner.listQemuFn = func(_ context.Context, node string, _ *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		if c.offline[node] {
			return nil, errors.New(nodeDownProxyResponse)
		}
		rows, err := c.placedRows(node)
		if err != nil {
			return nil, err
		}
		resp := sdknodes.ListQemuResponse(rows)
		return &resp, nil
	}
	inner.listStorageContentFn = func(_ context.Context, node, _ string, _ *sdknodes.ListStorageContentParams) (*sdknodes.ListStorageContentResponse, error) {
		if err := c.storageReadErr(node); err != nil {
			return nil, err
		}
		var resp sdknodes.ListStorageContentResponse
		for volume := range c.onStorage[node] {
			b, err := json.Marshal(map[string]any{"volid": volume, "content": "images"})
			if err != nil {
				return nil, err
			}
			resp = append(resp, b)
		}
		return &resp, nil
	}
	return inner
}

func (c *nodeClusterClient) Cluster() sdkcluster.Service {
	inner := c.scanFakeClient.Cluster().(*fakeClusterService)
	inner.listConfigNodesFn = func(context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
		resp := sdkcluster.ListConfigNodesResponse{json.RawMessage(`{"name":"pve1"}`), json.RawMessage(`{"name":"pve2"}`)}
		return &resp, nil
	}
	inner.listStatusFn = func(context.Context) (*sdkcluster.ListStatusResponse, error) {
		resp := sdkcluster.ListStatusResponse{json.RawMessage(`{"type":"cluster","quorate":1}`)}
		for _, node := range []string{"pve1", "pve2"} {
			online := 1
			if c.offline[node] {
				online = 0
			}
			b, err := json.Marshal(map[string]any{"type": "node", "name": node, "online": online})
			if err != nil {
				return nil, err
			}
			resp = append(resp, b)
		}
		return &resp, nil
	}
	inner.listResourcesFn = func(context.Context, *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		rows, err := c.placedRows("")
		if err != nil {
			return nil, err
		}
		resp := sdkcluster.ListResourcesResponse(rows)
		return &resp, nil
	}
	return inner
}

func (c *nodeClusterClient) ClusterStorage() clusterstorage.Service {
	return &fakeClusterStorageService{listFn: func(context.Context, *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
		resp := clusterstorage.ListStorageResponse{json.RawMessage(c.storageEntry)}
		return &resp, nil
	}}
}

func (c *nodeClusterClient) Storage() storage.Service {
	return &fakeStorageService{existsFn: func(_ context.Context, node, _, volume string) (bool, error) {
		if err := c.storageReadErr(node); err != nil {
			return false, err
		}
		return c.onStorage[node][volume], nil
	}}
}

// storageReadErr is how a storage read on node fails, or nil when the node's
// storage answers.
func (c *nodeClusterClient) storageReadErr(node string) error {
	switch {
	case node != "pve1" && node != "pve2":
		return errors.New("500 hostname lookup '" + node + "' failed - failed to get address info for: " + node +
			": Name or service not known")
	case c.offline[node]:
		return errors.New(nodeDownProxyResponse)
	default:
		return c.storageErr[node]
	}
}

// deferredParkConfigs is the deferred released park of the disk: parker
// 90000's record names data:vm-90001-disk-0, no key names that volume, and a
// copy on VM 800 carries the disk's serial.
func deferredParkConfigs() map[int]map[string]any {
	return map[int]map[string]any{
		90000: {cfgKeyTags: parkerTags, "description": copiedRecord("data:vm-90001-disk-0", "scsi4")},
		700:   {},
		800:   {"scsi1": "data:vm-800-disk-0,serial=" + copiedToken},
	}
}

// TestResolveDiskIdentity_StaleRecordOfAnotherDiskDoesNotDecide pins the
// rule that another disk's record sets the volume aside only while that
// record is unfinished, and only after the snapshot read. Parker 90002 keeps
// a record of another disk naming the volume, but that disk's serial is on VM
// 702, so the record is left over from a finished move and says nothing about
// who holds the name now. Before the change that record set the volume aside,
// and the copy on VM 800 resolved as the disk.
func TestResolveDiskIdentity_StaleRecordOfAnotherDiskDoesNotDecide(t *testing.T) {
	t.Parallel()

	withStaleRecord := func(t *testing.T) map[int]map[string]any {
		configs := deferredParkConfigs()
		configs[90002] = map[string]any{cfgKeyTags: parkerTags, "description": parkerRecords(t, map[string]parkerProvEntry{
			copiedOtherID: transferRecord("data:vm-90001-disk-0", "scsi2", "702"),
		})}
		configs[702] = map[string]any{"scsi1": "data:vm-702-disk-0,serial=" + copiedOtherID}
		return configs
	}

	t.Run("beside the deferred park's snapshot", func(t *testing.T) {
		t.Parallel()
		inner := copiedClient(withStaleRecord(t), 90000, 90002, 700, 702, 800)
		inner.snapshots = map[int]map[string]map[string]any{
			700: {"pre-upgrade": {"scsi1": "data:vm-90001-disk-0,serial=" + copiedToken + ",size=10G"}},
		}
		c := &storageFakeClient{scanFakeClient: inner}
		_, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		requireCopiedRefusal(t, err,
			"slot scsi1 of VM 800 on node pve1 with volume data:vm-800-disk-0",
			`snapshot "pre-upgrade" of source VM 700 on node pve1 names that volume on scsi1`)
	})

	t.Run("beside a volume only storage holds", func(t *testing.T) {
		t.Parallel()
		c := &storageFakeClient{scanFakeClient: copiedClient(withStaleRecord(t), 90000, 90002, 700, 702, 800),
			floating: map[string]bool{"data:vm-90001-disk-0": true}}
		_, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		requireCopiedRefusal(t, err,
			"names volume data:vm-90001-disk-0",
			"storage data on node pve1 still holds that volume and no guest names it")
	})

	t.Run("an unfinished record of another disk beside the deferred park's snapshot", func(t *testing.T) {
		t.Parallel()
		// No guest carries the other disk's serial, so its record is
		// unfinished. The snapshot still decides first, because it names the
		// volume by the name it had when the snapshot was taken.
		configs := withStaleRecord(t)
		delete(configs, 702)
		inner := copiedClient(configs, 90000, 90002, 700, 800)
		inner.snapshots = map[int]map[string]map[string]any{
			700: {"pre-upgrade": {"scsi1": "data:vm-90001-disk-0,serial=" + copiedToken + ",size=10G"}},
		}
		c := &storageFakeClient{scanFakeClient: inner}
		_, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		requireCopiedRefusal(t, err,
			"slot scsi1 of VM 800 on node pve1 with volume data:vm-800-disk-0",
			`snapshot "pre-upgrade" of source VM 700 on node pve1 names that volume on scsi1`)
	})

	t.Run("an unfinished record of another disk sets aside a volume only storage holds", func(t *testing.T) {
		t.Parallel()
		configs := withStaleRecord(t)
		delete(configs, 702)
		c := &storageFakeClient{scanFakeClient: copiedClient(configs, 90000, 90002, 700, 800),
			floating: map[string]bool{"data:vm-90001-disk-0": true}}
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil || ident.Holder.VMID != 800 || ident.Volid != "data:vm-800-disk-0" {
			t.Fatalf("identity = %+v, err = %v; want the slot on VM 800", ident, err)
		}
	})

	t.Run("an unfinished record of another disk beside a snapshot read that failed", func(t *testing.T) {
		t.Parallel()
		// A snapshot outranks the record, so while the snapshot read can't
		// answer, the record can't decide either. Before the change the
		// failed read was dropped, and the copy on VM 800 resolved as the
		// disk.
		configs := withStaleRecord(t)
		delete(configs, 702)
		inner := copiedClient(configs, 90000, 90002, 700, 800)
		inner.snapshotErr = errors.New("500 Internal Server Error")
		c := &storageFakeClient{scanFakeClient: inner, floating: map[string]bool{"data:vm-90001-disk-0": true}}
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		requireRetriable(t, err, "data:vm-90001-disk-0", "source VM 700", "500 Internal Server Error")
		if ident.Holder.Found || ident.Volid != "" {
			t.Fatalf("identity = %+v, want none", ident)
		}
	})

	t.Run("another disk's key beside a snapshot read that failed", func(t *testing.T) {
		t.Parallel()
		configs := deferredParkConfigs()
		configs[90001] = map[string]any{cfgKeyTags: parkerTags, "scsi2": "data:vm-90001-disk-0,serial=" + copiedOtherID}
		inner := copiedClient(configs, 90000, 90001, 700, 800)
		inner.snapshotErr = errors.New("500 Internal Server Error")
		c := &storageFakeClient{scanFakeClient: inner}
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		requireRetriable(t, err, "data:vm-90001-disk-0", "source VM 700", "500 Internal Server Error")
		if ident.Holder.Found || ident.Volid != "" {
			t.Fatalf("identity = %+v, want none", ident)
		}
	})
}

// TestResolveDiskIdentity_SnapshotOfAnotherDiskDoesNotDecide pins the rule
// that a snapshot of the record's source VM names the volume as this disk's
// only on a drive line that carries this disk's serial or none. PVE reuses a
// freed VMID, so the source the record names can be a new guest whose own
// disk took the volume's name and whose snapshot names it under that disk's
// serial. Before the change that snapshot refused the disk on VM 701 forever.
func TestResolveDiskIdentity_SnapshotOfAnotherDiskDoesNotDecide(t *testing.T) {
	t.Parallel()

	reused := func(snapshotLine string) *storageFakeClient {
		inner := copiedClient(map[int]map[string]any{
			90000: {cfgKeyTags: parkerTags, "description": copiedRecord("data:vm-700-disk-1", "scsi4")},
			700:   {"scsi1": "data:vm-700-disk-1,serial=" + copiedOtherID},
			701:   {"scsi1": "data:vm-701-disk-0,serial=" + copiedToken},
		}, 90000, 700, 701)
		inner.snapshots = map[int]map[string]map[string]any{700: {"s1": {"scsi1": snapshotLine}}}
		return &storageFakeClient{scanFakeClient: inner, floating: map[string]bool{"data:vm-700-disk-1": true}}
	}

	t.Run("a snapshot line with another disk's serial", func(t *testing.T) {
		t.Parallel()
		c := reused("data:vm-700-disk-1,serial=" + copiedOtherID + ",size=10G")
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil || ident.Holder.VMID != 701 || ident.Volid != "data:vm-701-disk-0" {
			t.Fatalf("identity = %+v, err = %v; want the slot on VM 701", ident, err)
		}
	})

	t.Run("a snapshot line with no serial", func(t *testing.T) {
		t.Parallel()
		c := reused("data:vm-700-disk-1,size=10G")
		_, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		requireCopiedRefusal(t, err,
			"slot scsi1 of VM 701 on node pve1 with volume data:vm-701-disk-0",
			`snapshot "s1" of source VM 700 on node pve1 names that volume on scsi1`)
	})

	t.Run("a snapshot line with this disk's serial", func(t *testing.T) {
		t.Parallel()
		c := reused("data:vm-700-disk-1,serial=" + copiedToken + ",size=10G")
		_, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		requireCopiedRefusal(t, err,
			"slot scsi1 of VM 701 on node pve1 with volume data:vm-701-disk-0",
			`snapshot "s1" of source VM 700 on node pve1 names that volume on scsi1`)
	})
}

// TestResolveDiskIdentity_NodeLocalStorageIsProvenOnEveryNode pins the rule
// that a recorded volume on node-local storage is proven gone on every node
// where that storage is enabled, not only on the parker's node. A parker can
// be migrated after its transfer, and PVE leaves a volume behind that the
// parker doesn't name. Before the change the proof read only the parker's
// current node.
func TestResolveDiskIdentity_NodeLocalStorageIsProvenOnEveryNode(t *testing.T) {
	t.Parallel()

	const volume = "data:vm-90001-disk-0"

	t.Run("the parker moved off the node whose storage holds the volume", func(t *testing.T) {
		t.Parallel()
		c := newNodeClusterClient(deferredParkConfigs(), map[int]string{90000: "pve2"}, nil,
			map[string]map[string]bool{"pve1": {volume: true}}, localDataStorage, 90000, 700, 800)
		_, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		requireCopiedRefusal(t, err,
			"names volume "+volume,
			"storage data on node pve1 still holds that volume and no guest names it")
	})

	t.Run("a node where the storage is enabled is offline", func(t *testing.T) {
		t.Parallel()
		configs := deferredParkConfigs()
		configs[900] = map[string]any{}
		c := newNodeClusterClient(configs, map[int]string{900: "pve2"}, map[string]bool{"pve2": true},
			nil, localDataStorage, 90000, 700, 800, 900)
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		requireRetriable(t, err, volume, "node-local storage data, because node(s) pve2 are offline")
		if ident.Holder.Found || ident.Volid != "" {
			t.Fatalf("identity = %+v, want none", ident)
		}
	})

	t.Run("an offline node the storage isn't enabled on", func(t *testing.T) {
		t.Parallel()
		configs := deferredParkConfigs()
		configs[900] = map[string]any{}
		c := newNodeClusterClient(configs, map[int]string{900: "pve2"}, map[string]bool{"pve2": true},
			nil, localDataStoragePVE1, 90000, 700, 800, 900)
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil || ident.Holder.VMID != 800 || ident.Volid != "data:vm-800-disk-0" {
			t.Fatalf("identity = %+v, err = %v; want the slot on VM 800", ident, err)
		}
	})

	t.Run("shared storage is proven once, on the parker's node", func(t *testing.T) {
		t.Parallel()
		// Every node sees shared storage, so one answer settles it. The fake
		// shows the volume only to pve1, so a proof on pve1 would refuse.
		c := newNodeClusterClient(deferredParkConfigs(), map[int]string{90000: "pve2"}, nil,
			map[string]map[string]bool{"pve1": {volume: true}}, sharedDataStorage, 90000, 700, 800)
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil || ident.Holder.VMID != 800 || ident.Volid != "data:vm-800-disk-0" {
			t.Fatalf("identity = %+v, err = %v; want the slot on VM 800", ident, err)
		}
	})

	t.Run("a storage of a shared type without the shared flag is proven once", func(t *testing.T) {
		t.Parallel()
		// An rbd storage is shared by its type, so one answer settles it, and
		// the offline node can't hold a volume the parker's node doesn't see.
		// Before the change only the flag counted, and the offline node made
		// the call retriable.
		configs := deferredParkConfigs()
		configs[900] = map[string]any{}
		c := newNodeClusterClient(configs, map[int]string{900: "pve2"}, map[string]bool{"pve2": true},
			nil, `{"storage":"data","type":"rbd","content":"images"}`, 90000, 700, 800, 900)
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil || ident.Holder.VMID != 800 || ident.Volid != "data:vm-800-disk-0" {
			t.Fatalf("identity = %+v, err = %v; want the slot on VM 800", ident, err)
		}
	})

	t.Run("a node in the storage's list that left the cluster isn't asked", func(t *testing.T) {
		t.Parallel()
		// pvecm delnode leaves the node in storage.cfg, and PVE can't reach
		// it. Before the change the proof asked it, and every call stayed
		// retriable.
		c := newNodeClusterClient(deferredParkConfigs(), nil, nil, nil,
			`{"storage":"data","type":"lvmthin","content":"images","nodes":"pve1,pve2,pve3"}`, 90000, 700, 800)
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil || ident.Holder.VMID != 800 || ident.Volid != "data:vm-800-disk-0" {
			t.Fatalf("identity = %+v, err = %v; want the slot on VM 800", ident, err)
		}
	})

	t.Run("a node where the storage can't answer is named, not taken as absence", func(t *testing.T) {
		t.Parallel()
		c := newNodeClusterClient(deferredParkConfigs(), nil, nil, nil, localDataStorage, 90000, 700, 800)
		c.storageErr = map[string]error{"pve2": errors.New(`500 no such volume group "pve"`)}
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		requireRetriable(t, err, volume, "is still on storage data on node(s) pve2")
		if ident.Holder.Found || ident.Volid != "" {
			t.Fatalf("identity = %+v, want none", ident)
		}
	})

	t.Run("a node whose storage can't answer doesn't hide the volume on another node", func(t *testing.T) {
		t.Parallel()
		c := newNodeClusterClient(deferredParkConfigs(), map[int]string{90000: "pve2"}, nil,
			map[string]map[string]bool{"pve1": {volume: true}}, localDataStorage, 90000, 700, 800)
		c.storageErr = map[string]error{"pve2": errors.New(`500 no such volume group "pve"`)}
		_, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		requireCopiedRefusal(t, err,
			"names volume "+volume,
			"storage data on node pve1 still holds that volume and no guest names it")
	})

	t.Run("an unfinished record of another disk doesn't decide while a node is offline", func(t *testing.T) {
		t.Parallel()
		// A guest on the offline node could carry the other disk's serial,
		// which would make its record a finished one that says nothing about
		// the volume, so storage decides and the offline node keeps the
		// answer retriable. Without that gate the copy on VM 800 resolved.
		configs := deferredParkConfigs()
		configs[90002] = map[string]any{cfgKeyTags: parkerTags, "description": parkerRecords(t, map[string]parkerProvEntry{
			copiedOtherID: transferRecord(volume, "scsi2", "702"),
		})}
		configs[900] = map[string]any{}
		c := newNodeClusterClient(configs, map[int]string{900: "pve2"}, map[string]bool{"pve2": true},
			map[string]map[string]bool{"pve1": {volume: true}}, sharedDataStorage, 90000, 90002, 700, 800, 900)
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		requireRetriable(t, err, "storage data on node pve1 still holds volume "+volume, "node(s) pve2 are offline")
		if ident.Holder.Found || ident.Volid != "" {
			t.Fatalf("identity = %+v, want none", ident)
		}
	})
}

// TestResolveDiskIdentity_ExclusionCountsOnlyOnItsOwnNode pins the rule that
// on node-local storage another disk's key or record sets aside only the
// volume on the node where that disk's guest sits. The same volid names a
// different volume on every node, so a key on pve2 says nothing about the
// volume on pve1. Before the change the key on pve2 set the volume on pve1
// aside, and the copy on VM 800 resolved as the disk.
func TestResolveDiskIdentity_ExclusionCountsOnlyOnItsOwnNode(t *testing.T) {
	t.Parallel()

	const volume = "data:vm-90001-disk-0"

	t.Run("another disk's key on another node", func(t *testing.T) {
		t.Parallel()
		configs := deferredParkConfigs()
		configs[90001] = map[string]any{cfgKeyTags: parkerTags, "scsi2": volume + ",serial=" + copiedOtherID}
		c := newNodeClusterClient(configs, map[int]string{90001: "pve2"}, nil,
			map[string]map[string]bool{"pve1": {volume: true}, "pve2": {volume: true}}, localDataStorage, 90000, 90001, 700, 800)
		_, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		requireCopiedRefusal(t, err,
			"names volume "+volume,
			"storage data on node pve1 still holds that volume and no guest names it")
	})

	t.Run("another disk's unfinished record on another node", func(t *testing.T) {
		t.Parallel()
		configs := deferredParkConfigs()
		configs[90002] = map[string]any{cfgKeyTags: parkerTags, "description": parkerRecords(t, map[string]parkerProvEntry{
			copiedOtherID: transferRecord(volume, "scsi2", "702"),
		})}
		c := newNodeClusterClient(configs, map[int]string{90002: "pve2"}, nil,
			map[string]map[string]bool{"pve1": {volume: true}, "pve2": {volume: true}}, localDataStorage, 90000, 90002, 700, 800)
		_, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		requireCopiedRefusal(t, err,
			"names volume "+volume,
			"storage data on node pve1 still holds that volume and no guest names it")
	})

	t.Run("another disk's key on the node that holds the volume", func(t *testing.T) {
		t.Parallel()
		configs := deferredParkConfigs()
		configs[90001] = map[string]any{cfgKeyTags: parkerTags, "scsi2": volume + ",serial=" + copiedOtherID}
		c := newNodeClusterClient(configs, map[int]string{90001: "pve2"}, nil,
			map[string]map[string]bool{"pve2": {volume: true}}, localDataStorage, 90000, 90001, 700, 800)
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil || ident.Holder.VMID != 800 || ident.Volid != "data:vm-800-disk-0" {
			t.Fatalf("identity = %+v, err = %v; want the slot on VM 800", ident, err)
		}
	})

	t.Run("another disk's key on shared storage sets the volume aside on every node", func(t *testing.T) {
		t.Parallel()
		configs := deferredParkConfigs()
		configs[90001] = map[string]any{cfgKeyTags: parkerTags, "scsi2": volume + ",serial=" + copiedOtherID}
		c := newNodeClusterClient(configs, map[int]string{90001: "pve2"}, nil,
			map[string]map[string]bool{"pve1": {volume: true}, "pve2": {volume: true}}, sharedDataStorage, 90000, 90001, 700, 800)
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil || ident.Holder.VMID != 800 || ident.Volid != "data:vm-800-disk-0" {
			t.Fatalf("identity = %+v, err = %v; want the slot on VM 800", ident, err)
		}
	})
}

// TestResolveDiskIdentity_RecordNamingNoStorageIsPermanent pins the refusal
// for a record whose volume names no storage. Nothing about that record
// changes on a retry, so a retriable answer had the Director retry the call
// forever.
func TestResolveDiskIdentity_RecordNamingNoStorageIsPermanent(t *testing.T) {
	t.Parallel()

	c := &storageFakeClient{scanFakeClient: copiedClient(map[int]map[string]any{
		90000: {cfgKeyTags: parkerTags, "description": copiedRecord("vm-90001-disk-0", "scsi4")},
		700:   {},
		800:   {"scsi1": "data:vm-800-disk-0,serial=" + copiedToken},
	}, 90000, 700, 800)}
	ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
	if err == nil {
		t.Fatalf("identity = %+v, want a refusal", ident)
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || typed.OkToRetry() {
		t.Fatalf("err = %v, want a permanent CPI error", err)
	}
	for _, want := range []string{"parker VM 90000 on node pve1", `"vm-90001-disk-0"`, copiedToken, "bosh_parked_disks", DiskIdentityCopiedRunbook} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v\nwant it to contain %q", err, want)
		}
	}
}

// unclassifiedStorageClient is a nodeClusterClient whose /storage read fails,
// so the absence proof can't tell whether the storage is shared.
type unclassifiedStorageClient struct{ *nodeClusterClient }

func (c *unclassifiedStorageClient) ClusterStorage() clusterstorage.Service {
	return &fakeClusterStorageService{listFn: func(context.Context, *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
		return nil, errors.New("500 Internal Server Error")
	}}
}

// TestResolveDiskIdentity_UnclassifiedStorageWithAnExclusionIsRetriable pins
// the answer when the /storage read fails and another disk's key or record
// names the volume. If the storage is shared, that claim covers every node,
// and a node-local proof on any other node finds the volume and refuses
// permanently with evidence that's false. So the answer is a retriable error
// that names the storage. Before the change the call refused permanently.
func TestResolveDiskIdentity_UnclassifiedStorageWithAnExclusionIsRetriable(t *testing.T) {
	t.Parallel()

	const volume = "data:vm-90001-disk-0"
	onBothNodes := map[string]map[string]bool{"pve1": {volume: true}, "pve2": {volume: true}}

	t.Run("another disk's key", func(t *testing.T) {
		t.Parallel()
		configs := deferredParkConfigs()
		configs[90001] = map[string]any{cfgKeyTags: parkerTags, "scsi2": volume + ",serial=" + copiedOtherID}
		inner := newNodeClusterClient(configs, map[int]string{90001: "pve1"}, nil, onBothNodes, sharedDataStorage, 90000, 90001, 700, 800)
		ident, err := ResolveDiskIdentity(context.Background(), &unclassifiedStorageClient{inner}, nil, copiedBirth, copiedToken, copiedCfg)
		requireRetriable(t, err, volume, "the read of storage data failed", "node(s) pve1", "retry once storage data can be read")
		if ident.Holder.Found || ident.Volid != "" {
			t.Fatalf("identity = %+v, want none", ident)
		}
		// The same call resolves once the read answers.
		ident, err = ResolveDiskIdentity(context.Background(), inner, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil || ident.Holder.VMID != 800 || ident.Volid != "data:vm-800-disk-0" {
			t.Fatalf("identity = %+v, err = %v; want the slot on VM 800 once storage reads", ident, err)
		}
	})

	t.Run("another disk's unfinished record", func(t *testing.T) {
		t.Parallel()
		configs := deferredParkConfigs()
		configs[90002] = map[string]any{cfgKeyTags: parkerTags, "description": parkerRecords(t, map[string]parkerProvEntry{
			copiedOtherID: transferRecord(volume, "scsi2", "702"),
		})}
		inner := newNodeClusterClient(configs, map[int]string{90002: "pve1"}, nil, onBothNodes, sharedDataStorage, 90000, 90002, 700, 800)
		_, err := ResolveDiskIdentity(context.Background(), &unclassifiedStorageClient{inner}, nil, copiedBirth, copiedToken, copiedCfg)
		requireRetriable(t, err, volume, "the read of storage data failed", "node(s) pve1")
	})
}

// TestResolveDiskIdentity_SnapshotFailureNamesTheClaimNode pins the node the
// retriable error names when the snapshot read failed beside another disk's
// key. On shared storage the proof runs on the parker's node, pve1, but the
// key sits on pve2, and the operator looks for the key where the message
// points.
func TestResolveDiskIdentity_SnapshotFailureNamesTheClaimNode(t *testing.T) {
	t.Parallel()

	const volume = "data:vm-90001-disk-0"

	for _, tc := range []struct {
		name, storageEntry string
	}{
		{"shared storage", sharedDataStorage},
		{"node-local storage", localDataStorage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			configs := deferredParkConfigs()
			configs[90001] = map[string]any{cfgKeyTags: parkerTags, "scsi2": volume + ",serial=" + copiedOtherID}
			c := newNodeClusterClient(configs, map[int]string{90001: "pve2"}, nil,
				map[string]map[string]bool{"pve1": {volume: true}, "pve2": {volume: true}}, tc.storageEntry, 90000, 90001, 700, 800)
			c.snapshotErr = errors.New("500 Internal Server Error")
			_, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
			requireRetriable(t, err, volume, "names it on node(s) pve2,")
			if strings.Contains(err.Error(), "node(s) pve1") {
				t.Fatalf("err = %v\nwant it to name the key's node, not the parker's", err)
			}
		})
	}
}

// TestResolveDiskIdentity_StorageNodesListWithNoMemberNamesTheList pins the
// error for node-local storage whose nodes list names only nodes that have
// left the cluster. The cluster answers and the list doesn't change, so the
// error says the operator has to fix the list, where it used to say to retry
// once the cluster answers.
func TestResolveDiskIdentity_StorageNodesListWithNoMemberNamesTheList(t *testing.T) {
	t.Parallel()

	const volume = "data:vm-90001-disk-0"

	c := newNodeClusterClient(deferredParkConfigs(), nil, nil, nil,
		`{"storage":"data","type":"lvmthin","content":"images","nodes":"pve3"}`, 90000, 700, 800)
	ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
	requireRetriable(t, err, volume, "the nodes list of storage data, pve3, names no current cluster member", "fix that list")
	if strings.Contains(err.Error(), "retry once the cluster answers") {
		t.Fatalf("err = %v\nwant no advice to wait for the cluster", err)
	}
	if ident.Holder.Found || ident.Volid != "" {
		t.Fatalf("identity = %+v, want none", ident)
	}
}
