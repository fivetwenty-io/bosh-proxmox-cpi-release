package pve

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	sdknodes "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/nodes"
	sdkqemu "github.com/fivetwenty-io/proxmox-apiclient-go/v3/pkg/api/qemu"
)

func accessStorages(t *testing.T, rows ...map[string]any) *StorageInfoCache {
	t.Helper()
	return NewStorageInfoCache(&fakeLister{entries: rows}, time.Minute)
}

var accessMembers = []string{"A", "B", "C", "D"}

func TestVolumeAccessSet_IntersectsEveryVolumesNodes(t *testing.T) {
	t.Parallel()
	cache := accessStorages(t,
		map[string]any{"storage": "nfs-ab", "type": "nfs", "nodes": "B,A"},
		map[string]any{"storage": "nfs-ac", "type": "nfs", "nodes": "A,C"},
		map[string]any{"storage": "ceph", "type": "rbd"},
	)
	got, err := VolumeAccessSet(context.Background(), cache, accessMembers, []AccessVolume{
		{Volume: "nfs-ab:100/vm-100-disk-0.qcow2"},
		{Volume: "nfs-ac:100/vm-100-disk-1.qcow2"},
		{Volume: "ceph:vm-100-disk-2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"A"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("root disk on A,B with a persistent disk on A,C must give %v, got %v", want, got)
	}
}

func TestVolumeAccessSet_UnrestrictedSharedStorageGivesEveryMember(t *testing.T) {
	t.Parallel()
	cache := accessStorages(t, map[string]any{"storage": "ceph", "type": "rbd"})
	got, err := VolumeAccessSet(context.Background(), cache, accessMembers, []AccessVolume{{Volume: "ceph:vm-1-disk-0"}})
	if err != nil || !reflect.DeepEqual(got, accessMembers) {
		t.Fatalf("got %v err=%v, want every member", got, err)
	}
	none, err := VolumeAccessSet(context.Background(), cache, accessMembers, nil)
	if err != nil || !reflect.DeepEqual(none, accessMembers) {
		t.Fatalf("no volumes must give every member, got %v err=%v", none, err)
	}
}

func TestVolumeAccessSet_NewSharedTypesReachEveryMember(t *testing.T) {
	t.Parallel()
	for _, typ := range []string{"iscsi", "iscsidirect", "zfs", "drbd"} {
		cache := accessStorages(t, map[string]any{"storage": "s", "type": typ})
		got, err := VolumeAccessSet(context.Background(), cache, accessMembers, []AccessVolume{{Volume: "s:vol-1"}})
		if err != nil || !reflect.DeepEqual(got, accessMembers) {
			t.Errorf("type %s: got %v err=%v, want every member without a home node", typ, got, err)
		}
	}
}

func TestVolumeAccessSet_LocalVolumeReachesItsHomeNodeOnly(t *testing.T) {
	t.Parallel()
	cache := accessStorages(t,
		map[string]any{"storage": "local-lvm", "type": "lvmthin"},
		map[string]any{"storage": "ceph", "type": "rbd"},
	)
	got, err := VolumeAccessSet(context.Background(), cache, accessMembers, []AccessVolume{
		{Volume: "local-lvm:vm-1-disk-0", HomeNode: "C"},
		{Volume: "ceph:vm-1-disk-1", HomeNode: "C"},
	})
	if err != nil || !reflect.DeepEqual(got, []string{"C"}) {
		t.Fatalf("got %v err=%v, want C", got, err)
	}
}

func TestVolumeAccessSet_LocalVolumeOutsideStorageNodesReachesNone(t *testing.T) {
	t.Parallel()
	cache := accessStorages(t, map[string]any{"storage": "local-lvm", "type": "lvmthin", "nodes": "A"})
	got, err := VolumeAccessSet(context.Background(), cache, accessMembers, []AccessVolume{
		{Volume: "local-lvm:vm-1-disk-0", HomeNode: "B"},
	})
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v err=%v, want the empty set", got, err)
	}
}

func TestVolumeAccessSet_VMStateOnSingleNodeStorageNarrowsTheSet(t *testing.T) {
	t.Parallel()
	cache := accessStorages(t,
		map[string]any{"storage": "ceph", "type": "rbd"},
		map[string]any{"storage": "state-nfs", "type": "nfs", "nodes": "D"},
	)
	cfg := map[string]any{
		"scsi0":   "ceph:vm-7-disk-0,size=10G",
		"vmstate": "state-nfs:7/vm-7-state-pre.raw",
	}
	got, err := VolumeAccessSet(context.Background(), cache, accessMembers, ConfigAccessVolumes(cfg, "A"))
	if err != nil || !reflect.DeepEqual(got, []string{"D"}) {
		t.Fatalf("got %v err=%v, want D", got, err)
	}
}

func TestVolumeAccessSet_NodesOutsideMembershipAreDropped(t *testing.T) {
	t.Parallel()
	cache := accessStorages(t, map[string]any{"storage": "nfs1", "type": "nfs", "nodes": "A,Z"})
	got, err := VolumeAccessSet(context.Background(), cache, accessMembers, []AccessVolume{{Volume: "nfs1:1/x"}})
	if err != nil || !reflect.DeepEqual(got, []string{"A"}) {
		t.Fatalf("got %v err=%v, want A", got, err)
	}
}

func TestVolumeAccessSet_DisabledStorageReachesNone(t *testing.T) {
	t.Parallel()
	cache := accessStorages(t, map[string]any{"storage": "nfs1", "type": "nfs", "disable": 1})
	got, err := VolumeAccessSet(context.Background(), cache, accessMembers, []AccessVolume{{Volume: "nfs1:1/x"}})
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v err=%v, want the empty set", got, err)
	}
}

func TestVolumeAccessSet_FailsClosed(t *testing.T) {
	t.Parallel()
	cache := accessStorages(t,
		map[string]any{"storage": "local-lvm", "type": "lvmthin"},
		map[string]any{"storage": "ceph", "type": "rbd"},
	)
	cases := map[string][]AccessVolume{
		"no storage qualifier":      {{Volume: "vm-1-disk-0"}},
		"empty volume name":         {{Volume: "ceph:"}},
		"storage not in the index":  {{Volume: "ghost:vm-1-disk-0"}},
		"local volume, no home":     {{Volume: "local-lvm:vm-1-disk-0"}},
		"shared first, local later": {{Volume: "ceph:vm-1-disk-0"}, {Volume: "local-lvm:vm-1-disk-1"}},
	}
	for name, vols := range cases {
		if got, err := VolumeAccessSet(context.Background(), cache, accessMembers, vols); err == nil {
			t.Errorf("%s: got %v, want an error", name, got)
		}
	}
	if _, err := VolumeAccessSet(context.Background(), nil, accessMembers, nil); err == nil {
		t.Error("a nil storage source must be an error")
	}
}

func TestConfigAccessVolumes_ListsEveryVolumeBearingKey(t *testing.T) {
	t.Parallel()
	cfg := map[string]any{
		"scsi0":       "ceph:vm-1-disk-0,size=32G,serial=abc",
		"scsi1":       "ceph:vm-1-disk-0,size=32G",
		"ide2":        "local:iso/cloud.iso,media=cdrom",
		"ide3":        "local-lvm:vm-1-cloudinit,media=cdrom",
		"sata0":       "none",
		"virtio0":     "cdrom",
		"efidisk0":    "ceph:vm-1-disk-1,size=4M",
		"tpmstate0":   "ceph:vm-1-disk-2,size=4M,version=v2.0",
		"unused0":     "nfs1:1/vm-1-disk-9.qcow2",
		"vmstate":     "nfs1:1/vm-1-state-pre.raw",
		"name":        "scsi-not-a-key",
		"net0":        "virtio=AA:BB,bridge=vmbr0",
		"scsihw":      "virtio-scsi-pci",
		"memory":      2048,
		"description": "ceph:vm-1-decoy",
	}
	volumes := ConfigAccessVolumes(cfg, "A")
	got := make([]string, 0, len(volumes))
	for _, v := range volumes {
		if v.HomeNode != "A" {
			t.Errorf("%s must carry the home node A, got %q", v.Volume, v.HomeNode)
		}
		got = append(got, v.Volume)
	}
	want := []string{
		"ceph:vm-1-disk-0", "ceph:vm-1-disk-1", "ceph:vm-1-disk-2", "local-lvm:vm-1-cloudinit",
		"local:iso/cloud.iso", "nfs1:1/vm-1-disk-9.qcow2", "nfs1:1/vm-1-state-pre.raw",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

type vavQEMU struct {
	sdkqemu.Service
	snapshots []map[string]any
	err       error
}

func (q *vavQEMU) ListSnapshots(context.Context, string, int) ([]map[string]any, error) {
	return q.snapshots, q.err
}

type vavNodes struct {
	sdknodes.Service
	configs map[string]string
}

func (n *vavNodes) ListQemuSnapshotConfig(_ context.Context, _, _, name string) (*sdknodes.ListQemuSnapshotConfigResponse, error) {
	body, ok := n.configs[name]
	if !ok {
		return nil, errors.New("no such snapshot")
	}
	resp := sdknodes.ListQemuSnapshotConfigResponse(body)
	return &resp, nil
}

type vavClient struct {
	Client
	q *vavQEMU
	n *vavNodes
}

func (c *vavClient) QEMU() sdkqemu.Service   { return c.q }
func (c *vavClient) Nodes() sdknodes.Service { return c.n }

func TestVMAccessVolumes_IncludesSnapshotAndVMStateVolumes(t *testing.T) {
	t.Parallel()
	snapCfg, _ := json.Marshal(map[string]any{
		"scsi0": "ceph:vm-1-disk-0,size=32G", "scsi1": "nfs-old:1/vm-1-disk-1.qcow2",
		"vmstate": "state-nfs:1/vm-1-state-pre.raw", "snaptime": 1,
	})
	c := &vavClient{
		q: &vavQEMU{snapshots: []map[string]any{{"name": "current"}, {"name": "pre"}}},
		n: &vavNodes{configs: map[string]string{"pre": string(snapCfg)}},
	}
	got, err := VMAccessVolumes(context.Background(), c, "A", 1, map[string]any{"scsi0": "ceph:vm-1-disk-0,size=32G"})
	if err != nil {
		t.Fatal(err)
	}
	vols := make([]string, 0, len(got))
	for _, v := range got {
		vols = append(vols, v.Volume)
	}
	want := []string{"ceph:vm-1-disk-0", "nfs-old:1/vm-1-disk-1.qcow2", "state-nfs:1/vm-1-state-pre.raw"}
	if !reflect.DeepEqual(vols, want) {
		t.Fatalf("got %v want %v", vols, want)
	}
}

func TestVMAccessVolumes_UnreadableSnapshotIsAnError(t *testing.T) {
	t.Parallel()
	c := &vavClient{
		q: &vavQEMU{snapshots: []map[string]any{{"name": "pre"}}},
		n: &vavNodes{configs: map[string]string{}},
	}
	if _, err := VMAccessVolumes(context.Background(), c, "A", 1, nil); err == nil {
		t.Fatal("a snapshot whose config cannot be read must fail the access set")
	}
	c = &vavClient{q: &vavQEMU{err: errors.New("boom")}, n: &vavNodes{}}
	if _, err := VMAccessVolumes(context.Background(), c, "A", 1, nil); err == nil {
		t.Fatal("a failed snapshot listing must fail the access set")
	}
}
