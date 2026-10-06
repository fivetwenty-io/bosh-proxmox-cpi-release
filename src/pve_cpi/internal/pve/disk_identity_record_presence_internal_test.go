// disk_identity_record_presence_internal_test.go — white-box tests for how
// the resolver decides whether a parker's record of a disk's transfer names a
// volume that is still there, while one slot carries the disk's serial. They
// cover a node that is down, a volume that only storage or a snapshot still
// holds, and a landing that belongs to another disk.
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

	cpierrors "github.com/fivetwenty-io/bosh-proxmox-cpi/internal/errors"
)

// copiedOtherID is the serial of another disk that shares a parker with the
// disk under test.
const copiedOtherID = "bpd-8899aabbccddeeff"

// offlineNodeClient is a storageFakeClient on a quorate two-node cluster
// whose second node, pve2, the cluster reports offline. The guests in onPVE2
// live there. Only /cluster/resources still lists them, because pve2 can't
// answer its own listing, and a read of their configs fails the way PVE's
// proxy fails toward a node that is down. Every other guest lives on pve1.
type offlineNodeClient struct {
	*storageFakeClient
	onPVE2 map[int]string
	// storageEntry, when set, is the /storage entry the client serves for
	// storage "data". Without one the storage can't be classified.
	storageEntry string
}

func (c *offlineNodeClient) ClusterStorage() clusterstorage.Service {
	if c.storageEntry == "" {
		return nil
	}
	return &fakeClusterStorageService{listFn: func(context.Context, *clusterstorage.ListStorageParams) (*clusterstorage.ListStorageResponse, error) {
		resp := clusterstorage.ListStorageResponse{json.RawMessage(c.storageEntry)}
		return &resp, nil
	}}
}

// newOfflineNodeClient builds the client. rows lists pve1's guests in the
// order pve1 lists them, and onPVE2 maps each guest on pve2 to its tags.
func newOfflineNodeClient(configs map[int]map[string]any, onPVE2 map[int]string, floating map[string]bool, rows ...int) *offlineNodeClient {
	inner := copiedClient(configs, rows...)
	inner.configErr = map[int]error{}
	for vmid := range onPVE2 {
		inner.configErr[vmid] = errors.New("595 Errors during connection establishment, proxy handler: No route to host")
	}
	return &offlineNodeClient{storageFakeClient: &storageFakeClient{scanFakeClient: inner, floating: floating}, onPVE2: onPVE2}
}

func (c *offlineNodeClient) Nodes() sdknodes.Service {
	inner := c.scanFakeClient.Nodes().(*fakeNodesService)
	pve1 := inner.listQemuFn
	inner.listQemuFn = func(ctx context.Context, node string, params *sdknodes.ListQemuParams) (*sdknodes.ListQemuResponse, error) {
		if node == "pve2" {
			return nil, errors.New("595 Errors during connection establishment, proxy handler: No route to host")
		}
		return pve1(ctx, node, params)
	}
	return inner
}

func (c *offlineNodeClient) Cluster() sdkcluster.Service {
	inner := c.scanFakeClient.Cluster().(*fakeClusterService)
	pve1 := inner.listResourcesFn
	inner.listConfigNodesFn = func(context.Context) (*sdkcluster.ListConfigNodesResponse, error) {
		resp := sdkcluster.ListConfigNodesResponse{json.RawMessage(`{"name":"pve1"}`), json.RawMessage(`{"name":"pve2"}`)}
		return &resp, nil
	}
	inner.listStatusFn = func(context.Context) (*sdkcluster.ListStatusResponse, error) {
		resp := sdkcluster.ListStatusResponse{
			json.RawMessage(`{"type":"cluster","quorate":1}`),
			json.RawMessage(`{"type":"node","name":"pve1","online":1}`),
			json.RawMessage(`{"type":"node","name":"pve2","online":0}`),
		}
		return &resp, nil
	}
	inner.listResourcesFn = func(ctx context.Context, params *sdkcluster.ListResourcesParams) (*sdkcluster.ListResourcesResponse, error) {
		resp, err := pve1(ctx, params)
		if err != nil {
			return nil, err
		}
		for vmid, tags := range c.onPVE2 {
			row := clusterRow(vmid, tags)
			row["node"] = "pve2"
			b, marshalErr := json.Marshal(row)
			if marshalErr != nil {
				return nil, marshalErr
			}
			*resp = append(*resp, b)
		}
		return resp, nil
	}
	return inner
}

// requireRetriable fails unless err is a retriable error that is not a copy
// refusal and that contains every string in want.
func requireRetriable(t *testing.T, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("err = nil, want a retriable error")
	}
	if _, copied := IsDiskIdentityCopied(err); copied {
		t.Fatalf("err = %v, want a retriable error, not a copy refusal", err)
	}
	var typed *cpierrors.Error
	if !errors.As(err, &typed) || !typed.OkToRetry() {
		t.Fatalf("err = %v, want a retriable CPI error", err)
	}
	for _, s := range want {
		if !strings.Contains(err.Error(), s) {
			t.Fatalf("err = %v\nwant it to contain %q", err, s)
		}
	}
}

// TestResolveDiskIdentity_OfflineNodeDoesNotBlockASlotHit pins the rule that
// the record check reads the parkers the identity scan read, with the scan's
// tolerance of a node the quorate cluster reports offline. Before the change
// it walked every parker /cluster/resources lists, and one parker on a node
// that was down failed every disk call in the cluster.
func TestResolveDiskIdentity_OfflineNodeDoesNotBlockASlotHit(t *testing.T) {
	t.Parallel()

	t.Run("a parker on the offline node", func(t *testing.T) {
		t.Parallel()
		c := newOfflineNodeClient(map[int]map[string]any{
			700:   {"scsi1": "data:vm-700-disk-1,serial=" + copiedToken},
			90005: {cfgKeyTags: parkerTags, "description": copiedRecord("data:vm-700-disk-1", "scsi4")},
		}, map[int]string{90005: parkerTags}, nil, 700)
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil || !ident.Holder.Found || ident.Holder.VMID != 700 || ident.Volid != "data:vm-700-disk-1" {
			t.Fatalf("identity = %+v, err = %v; want the slot on VM 700", ident, err)
		}
	})

	t.Run("a stale record whose volume check would read the offline node", func(t *testing.T) {
		t.Parallel()
		// Parker 90000 on pve1 keeps the record an attach couldn't remove. The
		// guest read that looks for another key naming the recorded volume
		// skips pve2, and shared storage no longer holds the volume.
		c := newOfflineNodeClient(map[int]map[string]any{
			90000: {cfgKeyTags: parkerTags, "description": copiedRecord("data:vm-90000-disk-0", "scsi4")},
			700:   {"scsi1": "data:vm-700-disk-2,serial=" + copiedToken},
			800:   {},
		}, map[int]string{800: ""}, nil, 90000, 700)
		c.storageEntry = sharedDataStorage
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil || ident.Holder.VMID != 700 || ident.Volid != "data:vm-700-disk-2" {
			t.Fatalf("identity = %+v, err = %v; want the slot on VM 700", ident, err)
		}
	})

	t.Run("a recorded volume storage still holds while a node is offline", func(t *testing.T) {
		t.Parallel()
		// Nothing the CPI can read names the recorded volume, but a guest on
		// pve2 could, under another disk's serial, so the call can't refuse
		// for good and fails until pve2 answers again.
		c := newOfflineNodeClient(map[int]map[string]any{
			90000: {cfgKeyTags: parkerTags, "description": copiedRecord("data:vm-90001-disk-0", "scsi4")},
			700:   {},
			800:   {"scsi1": "data:vm-800-disk-0,serial=" + copiedToken},
			900:   {},
		}, map[int]string{900: ""}, map[string]bool{"data:vm-90001-disk-0": true}, 90000, 700, 800)
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		requireRetriable(t, err, "data:vm-90001-disk-0", "pve2")
		if ident.Holder.Found || ident.Volid != "" {
			t.Fatalf("identity = %+v, want none", ident)
		}
	})
}

// TestResolveDiskIdentity_RecordedVolumeStillOnStorage covers a record whose
// volume no disk key names but that still exists. A deferred park of a volume
// the source doesn't own leaves exactly that, from the slot delete until the
// snapshot that holds the volume is deleted. A clone from that snapshot, or a
// restore of an older backup, then carries the serial on a slot of its own.
// Before the change, only a disk key or a landing counted, so the resolver
// handed out the copy's volume.
func TestResolveDiskIdentity_RecordedVolumeStillOnStorage(t *testing.T) {
	t.Parallel()

	deferred := func() map[int]map[string]any {
		return map[int]map[string]any{
			90000: {cfgKeyTags: parkerTags, "description": copiedRecord("data:vm-90001-disk-0", "scsi4")},
			700:   {},
			800:   {"scsi1": "data:vm-800-disk-0,serial=" + copiedToken},
		}
	}

	t.Run("the deferred park with a clone from the snapshot", func(t *testing.T) {
		t.Parallel()
		inner := copiedClient(deferred(), 90000, 700, 800)
		inner.snapshots = map[int]map[string]map[string]any{
			700: {"pre-upgrade": {"scsi1": "data:vm-90001-disk-0,serial=" + copiedToken + ",size=10G"}},
		}
		c := &storageFakeClient{scanFakeClient: inner}
		_, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		requireCopiedRefusal(t, err,
			"slot scsi1 of VM 800 on node pve1 with volume data:vm-800-disk-0",
			"names volume data:vm-90001-disk-0",
			`snapshot "pre-upgrade" of source VM 700 on node pve1 names that volume on scsi1`)
	})

	t.Run("a restore beside a volume only storage holds", func(t *testing.T) {
		t.Parallel()
		c := &storageFakeClient{scanFakeClient: copiedClient(deferred(), 90000, 700, 800),
			floating: map[string]bool{"data:vm-90001-disk-0": true}}
		_, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		requireCopiedRefusal(t, err,
			"names volume data:vm-90001-disk-0",
			"storage data on node pve1 still holds that volume and no guest names it")
	})

	t.Run("neither storage nor a snapshot can settle it", func(t *testing.T) {
		t.Parallel()
		// The storage service can't answer and the snapshot listing fails, so
		// nothing says whether the recorded volume is still there.
		c := copiedClient(deferred(), 90000, 700, 800)
		c.snapshotErr = errors.New("500 Internal Server Error")
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		requireRetriable(t, err, "data:vm-90001-disk-0", "500 Internal Server Error")
		if ident.Holder.Found || ident.Volid != "" {
			t.Fatalf("identity = %+v, want none", ident)
		}
	})

	t.Run("another disk's record names the volume", func(t *testing.T) {
		t.Parallel()
		// The attach that moved the disk to VM 700 couldn't remove parker
		// 90000's record, and PVE gave the freed name to another disk that
		// parker 90001 records. That disk's volume is still there, and it
		// isn't this one's.
		c := &storageFakeClient{scanFakeClient: copiedClient(map[int]map[string]any{
			90000: {cfgKeyTags: parkerTags, "description": copiedRecord("data:vm-90000-disk-0", "scsi4")},
			90001: {cfgKeyTags: parkerTags, "description": parkerRecords(t, map[string]parkerProvEntry{
				copiedOtherID: transferRecord("data:vm-90000-disk-0", "scsi2", "702"),
			})},
			700: {"scsi1": "data:vm-700-disk-2,serial=" + copiedToken},
		}, 90000, 90001, 700), floating: map[string]bool{"data:vm-90000-disk-0": true}}
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil || ident.Holder.VMID != 700 || ident.Volid != "data:vm-700-disk-2" {
			t.Fatalf("identity = %+v, err = %v; want the slot on VM 700", ident, err)
		}
	})

	t.Run("another disk's landing took the freed name on the slot its record names", func(t *testing.T) {
		t.Parallel()
		// The freed name landed on scsi5 with no serial yet, and the other
		// disk's unfinished record names scsi5, so the volume is that disk's.
		c := &storageFakeClient{scanFakeClient: copiedClient(map[int]map[string]any{
			90000: {cfgKeyTags: parkerTags, "scsi5": "data:vm-90000-disk-0",
				"description": parkerRecords(t, map[string]parkerProvEntry{
					copiedToken:   transferRecord("data:vm-90000-disk-0", "scsi4", "700"),
					copiedOtherID: transferRecord("data:vm-702-disk-0", "scsi5", "702"),
				})},
			700: {"scsi1": "data:vm-700-disk-2,serial=" + copiedToken},
		}, 90000, 700)}
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil || ident.Holder.VMID != 700 || ident.Volid != "data:vm-700-disk-2" {
			t.Fatalf("identity = %+v, err = %v; want the slot on VM 700", ident, err)
		}
	})
}

// TestResolveDiskIdentity_OnlyTheRecordsOwnLandingCounts pins the rule that a
// volume with no serial on the record's parker counts as the disk's landing
// only on the slot the record names, and only while no other disk's unfinished
// record names that slot. Before the change any such volume on the parker
// counted, so one detach of another disk onto that parker refused a healthy
// disk whose stale record sat there, permanently.
func TestResolveDiskIdentity_OnlyTheRecordsOwnLandingCounts(t *testing.T) {
	t.Parallel()

	t.Run("another disk's landing on another slot", func(t *testing.T) {
		t.Parallel()
		c := &storageFakeClient{scanFakeClient: copiedClient(map[int]map[string]any{
			90000: {cfgKeyTags: parkerTags, "scsi5": "data:vm-90000-disk-3",
				"description": copiedRecord("data:vm-90000-disk-0", "scsi4")},
			700: {"scsi1": "data:vm-700-disk-1,serial=" + copiedToken},
		}, 90000, 700)}
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil || ident.Holder.VMID != 700 || ident.Volid != "data:vm-700-disk-1" {
			t.Fatalf("identity = %+v, err = %v; want the slot on VM 700", ident, err)
		}
	})

	t.Run("a landing on the record's slot that another unfinished record names", func(t *testing.T) {
		t.Parallel()
		c := &storageFakeClient{scanFakeClient: copiedClient(map[int]map[string]any{
			90000: {cfgKeyTags: parkerTags, "scsi4": "data:vm-90000-disk-3",
				"description": parkerRecords(t, map[string]parkerProvEntry{
					copiedToken:   transferRecord("data:vm-700-disk-0", "scsi4", "700"),
					copiedOtherID: transferRecord("data:vm-702-disk-0", "scsi4", "702"),
				})},
			700: {"scsi1": "data:vm-700-disk-1,serial=" + copiedToken},
		}, 90000, 700)}
		ident, err := ResolveDiskIdentity(context.Background(), c, nil, copiedBirth, copiedToken, copiedCfg)
		if err != nil || ident.Holder.VMID != 700 || ident.Volid != "data:vm-700-disk-1" {
			t.Fatalf("identity = %+v, err = %v; want the slot on VM 700", ident, err)
		}
	})
}
