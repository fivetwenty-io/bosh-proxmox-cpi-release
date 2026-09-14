package handlers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/config"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/log"
	"github.com/fivetwenty-io/bosh-proxmox-cpi/internal/pve"
)

func TestStorageSetReplicasNeeded(t *testing.T) {
	t.Parallel()
	on := Deps{Config: &config.CPIConfig{StemcellReplicateStorageSet: true, EphemeralStorageSet: "eph"}}
	if set, ok := storageSetReplicasNeeded(on, "abcd1234ef"); !ok || set != "eph" {
		t.Fatalf("want eph/true, got %q/%v", set, ok)
	}
	if _, ok := storageSetReplicasNeeded(on, ""); ok {
		t.Fatal("empty sha must disable replicas")
	}
	if _, ok := storageSetReplicasNeeded(on, "abc"); ok {
		t.Fatal("a digest shorter than eight characters must disable replicas")
	}
	off := Deps{Config: &config.CPIConfig{EphemeralStorageSet: "eph"}}
	if _, ok := storageSetReplicasNeeded(off, "abcd1234ef"); ok {
		t.Fatal("property off must disable replicas")
	}
	unbound := Deps{Config: &config.CPIConfig{StemcellReplicateStorageSet: true}}
	if _, ok := storageSetReplicasNeeded(unbound, "abcd1234ef"); ok {
		t.Fatal("no bound set must disable replicas")
	}
	if _, ok := storageSetReplicasNeeded(Deps{}, "abcd1234ef"); ok {
		t.Fatal("nil config must disable replicas")
	}
}

func replicaStorageDef(t *testing.T, id, server, export string, shared bool, content string) pve.StorageInfo {
	t.Helper()
	// An NFS storage is shared by protocol whatever its "shared" flag says
	// (pve.StorageInfo.IsShared), so a node-local member has to be a
	// plugin that is not inherently shared. "dir" is the one the fan-out
	// most plausibly meets on a set an operator wrote by hand.
	row := map[string]any{"storage": id, "type": "nfs", "shared": 1, "server": server, "export": export, "content": content}
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
		"ns_1":       replicaStorageDef(t, "ns_1", "nas", "/ns1", true, "images,import"),
		"ns_2":       replicaStorageDef(t, "ns_2", "nas", "/ns2", true, "images"),
		"ns_1_alias": replicaStorageDef(t, "ns_1_alias", "nas", "/ns1", true, "images"),
		"ns_3":       replicaStorageDef(t, "ns_3", "nas", "/ns3", false, "images"),
		"ns_4":       replicaStorageDef(t, "ns_4", "nas", "/ns4", true, "iso"),
		"ns-2":       replicaStorageDef(t, "ns-2", "nas", "/ns2b", true, "images"),
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
	cfg := &config.CPIConfig{VMStorage: "ns_1", EphemeralStorageSet: "eph", StemcellReplicateStorageSet: true,
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
	cfg := &config.CPIConfig{VMStorage: "ns_1", EphemeralStorageSet: "eph", StemcellReplicateStorageSet: true,
		StorageSets: map[string]config.StorageSet{"eph": {Names: []string{"ns_1", "ns_missing"},
			Strategy: config.StoragePlacementStrategy{Name: "spread", Version: 1}}}}
	deps := Deps{Config: cfg, Logger: log.NewNopLogger(), ReplicaInventory: src}
	if _, err := storageSetReplicaTargets(context.Background(), deps, "eph", "n1", "ns_1"); err == nil {
		t.Fatal("want a discover error for a member missing from the cluster")
	}
}
